package plg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/wire"
)

// FuzzCheckData builds a type from the fuzz input and checks the validator against an oracle computed while
// building it: the type is data exactly when every part of it is.
func FuzzCheckData(f *testing.F) {
	for _, seed := range [][]byte{
		{},
		{5, 0},
		{6, 8, 2, 0, 3},
		{9, 8, 1, 4},
		{9, 1},
		{7, 0},
		{8, 2, 10, 0, 0},
		{5, 13, 0},
		{12},
		{11, 2},
		{8, 2, 0, 9, 8, 0},
		{6, 5, 14},
		{15},
		{16},
		{17},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, program []byte) {
		gen := typeGen{program: program}
		typ, isData := gen.next(0)

		err := checkData(typ, map[reflect.Type]bool{})
		if isData {
			assert.NoError(t, err, "%s", typ)
		} else {
			require.ErrorIs(t, err, errNotData, "%s", typ)
		}
	})
}

// typeGen builds a type from a byte program, one byte per decision.
type typeGen struct {
	program []byte
	pos     int
}

const maxGenDepth = 6

func (g *typeGen) op() byte {
	if g.pos >= len(g.program) {
		return 0
	}

	g.pos++

	return g.program[g.pos-1]
}

//nolint:gocyclo // one case per type constructor keeps the oracle readable
func (g *typeGen) next(depth int) (reflect.Type, bool) {
	op := g.op()
	if depth >= maxGenDepth {
		op = 0
	}

	switch op % 18 {
	case 0:
		return reflect.TypeFor[string](), true
	case 1:
		return reflect.TypeFor[int8](), true
	case 2:
		return reflect.TypeFor[float64](), true
	case 3:
		return reflect.TypeFor[time.Time](), true
	case 4:
		return reflect.TypeFor[uint64](), true
	case 5:
		elem, ok := g.next(depth + 1)

		return reflect.SliceOf(elem), ok
	case 6:
		elem, ok := g.next(depth + 1)

		return reflect.MapOf(reflect.TypeFor[string](), elem), ok
	case 7:
		elem, _ := g.next(depth + 1)

		return reflect.MapOf(reflect.TypeFor[int](), elem), false
	case 8:
		return g.structOf(depth)
	case 9:
		elem, ok := g.next(depth + 1)

		return reflect.PointerTo(elem), ok && elem.Kind() == reflect.Struct
	case 10:
		elem, _ := g.next(depth + 1)

		return reflect.FuncOf([]reflect.Type{elem}, nil, false), false
	case 11:
		elem, _ := g.next(depth + 1)

		return reflect.ChanOf(reflect.BothDir, elem), false
	case 12:
		return reflect.TypeFor[any](), false
	case 13:
		elem, _ := g.next(depth + 1)

		return reflect.StructOf([]reflect.StructField{{Name: "f", PkgPath: "p", Type: elem}}), false
	case 14:
		return reflect.TypeFor[complex64](), false
	case 15:
		elem, _ := g.next(depth + 1)

		return reflect.ArrayOf(2, elem), false
	case 16:
		return reflect.TypeFor[uintptr](), false
	default:
		return reflect.TypeFor[error](), false
	}
}

func (g *typeGen) structOf(depth int) (reflect.Type, bool) {
	n := int(g.op() % 3)
	fields := make([]reflect.StructField, n)
	isData := true

	for i := range fields {
		typ, ok := g.next(depth + 1)
		fields[i] = reflect.StructField{Name: "F" + string(rune('0'+i)), Type: typ}
		isData = isData && ok
	}

	return reflect.StructOf(fields), isData
}

type pair interface {
	M(ctx context.Context) (string, int8, error)
}

// TestDecodeResultsRejectsMalformedBodies covers the guest answering garbage with an OK status: the call
// fails (and its instance is discarded) instead of returning made-up values.
func TestDecodeResultsRejectsMalformedBodies(t *testing.T) {
	t.Parallel()

	meth, err := newMethod(new(wire.Planner), reflect.TypeFor[pair](), 0, 0)
	require.NoError(t, err)

	valid, err := wire.AppendString(nil, "x")
	require.NoError(t, err)

	valid = wire.AppendInt(valid, -7)

	out, err := meth.decodeResults(valid)
	require.NoError(t, err)
	require.Len(t, out, 3)
	assert.Equal(t, "x", out[0].String())
	assert.Equal(t, int64(-7), out[1].Int())
	assert.True(t, out[2].IsNil(), "a nil error")

	for name, tc := range map[string]struct {
		want error
		body []byte
	}{
		"empty":     {wire.ErrTruncated, nil},
		"truncated": {wire.ErrTruncated, valid[:len(valid)-1]},
		"trailing":  {wire.ErrTrailing, append(slices.Clip(valid), 0)},
		"overflow":  {wire.ErrOverflow, wire.AppendInt(slices.Clip(valid[:len(valid)-8]), 300)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := meth.decodeResults(tc.body)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// testLimit is the memory limit of the fake instances in pool tests: one page.
const testLimit = engine.PageSize

var (
	errFake           = errors.New("fake failure")
	errSharedInstance = errors.New("two goroutines hold the same instance")
	errOverBound      = errors.New("pool over its bounds")
)

// fakeSpawner starts fake instances for pool tests.
type fakeSpawner struct {
	attempts chan time.Time // receives the time of every start, when non-nil
	gate     chan struct{}  // when non-nil, every start waits for a value from it, or for its context
	closeErr error          // returned by the instances' Close
	failures atomic.Int32   // how many of the next starts fail
	quota    int32          // when non-zero, starts beyond this many fail
	started  atomic.Int32
	closed   atomic.Int32
}

//nolint:ireturn // it is the pool's spawn function, which returns the pool's instance interface
func (s *fakeSpawner) spawn(ctx context.Context) (instance, error) {
	if s.attempts != nil {
		s.attempts <- time.Now()
	}

	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting at the gate: %w", context.Cause(ctx))
		}
	}

	if s.failures.Add(-1) >= 0 || (s.quota != 0 && s.started.Load() >= s.quota) {
		return nil, errFake
	}

	s.started.Add(1)

	return &fakeInstance{owner: s}, nil
}

// fakeInstance is an instance of a fakeSpawner; inUse catches two goroutines holding it at once.
type fakeInstance struct {
	owner *fakeSpawner
	inUse atomic.Bool
}

func (*fakeInstance) Call(context.Context, uint32, []byte) (engine.Result, error) {
	return engine.Result{}, nil
}

func (*fakeInstance) Broken() bool { return false }

func (f *fakeInstance) Close(context.Context) error {
	f.owner.closed.Add(1)

	return f.owner.closeErr
}

// recorder is a slog handler keeping every record, for tests to read once the logging goroutine is idle.
type recorder struct {
	records chan slog.Record
}

func newRecorder() *recorder {
	return &recorder{records: make(chan slog.Record, 256)}
}

func (*recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler     { return r }
func (r *recorder) WithGroup(string) slog.Handler          { return r }

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.records <- rec.Clone()

	return nil
}

// drain returns the records handled so far.
func (r *recorder) drain() []slog.Record {
	var out []slog.Record

	for {
		select {
		case rec := <-r.records:
			out = append(out, rec)
		default:
			return out
		}
	}
}

// attr returns the value of a record's attribute.
func attr(rec *slog.Record, key string) slog.Value {
	var found slog.Value

	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = a.Value
		}

		return a.Key != key
	})

	return found
}

// startPool warms a pool of sp's instances; cfg's spawn, key and limit are set, and its logger and budget
// default to a discarding logger and no budget.
func startPool(tb testing.TB, spawner *fakeSpawner, cfg poolConfig) *pool {
	tb.Helper()

	cfg.spawn, cfg.key, cfg.limit = spawner.spawn, "test/k", testLimit
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.DiscardHandler)
	}

	if cfg.budget == nil {
		cfg.budget = &memoryBudget{}
	}

	instances := newPool(cfg)
	require.NoError(tb, instances.warm(tb.Context()))

	return instances
}

func acquireN(t *testing.T, instances *pool, n int) []*slot {
	t.Helper()

	held := make([]*slot, n)

	for i := range held {
		var err error

		held[i], err = instances.acquire(t.Context())
		require.NoError(t, err)
	}

	return held
}

// acquireLater starts a caller acquiring from p and returns where its instance arrives.
func acquireLater(t *testing.T, instances *pool) <-chan *slot {
	t.Helper()

	got := make(chan *slot, 1)

	go func() {
		held, err := instances.acquire(t.Context())
		assert.NoError(t, err)

		got <- held
	}()

	return got
}

func TestPoolGrowsToItsMaximumThenWaits(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		spawner, budget := &fakeSpawner{}, &memoryBudget{}
		instances := startPool(t, spawner, poolConfig{budget: budget, minSize: 1, maxSize: 3})

		held := acquireN(t, instances, 3)
		assert.Equal(t, []uint64{1, 2, 3}, []uint64{held[0].id, held[1].id, held[2].id})
		assert.Equal(
			t,
			PoolStats{Size: 3, Grows: 2},
			instances.stats(),
			"callers started the instances past the minimum",
		)
		assert.Equal(t, uint64(3*testLimit), budget.used.Load())

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		_, err := instances.acquire(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded, "a full pool makes callers wait on their context")

		got := acquireLater(t, instances)

		synctest.Wait()
		instances.release(held[1])
		assert.Same(t, held[1], <-got, "a waiting caller gets the next instance given back")

		for _, h := range held {
			instances.release(h)
		}

		require.NoError(t, instances.close(t.Context()))
		assert.Equal(t, int32(3), spawner.closed.Load())
		assert.Zero(t, budget.used.Load())
	})
}

func TestPoolCallerStartFailureGivesThePlaceBack(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		spawner, budget := &fakeSpawner{}, &memoryBudget{}
		instances := startPool(t, spawner, poolConfig{budget: budget, minSize: 1, maxSize: 2})
		held := acquireN(t, instances, 1)[0]

		spawner.failures.Store(1)

		_, err := instances.acquire(t.Context())
		require.ErrorIs(t, err, errFake, "the caller's Create failed")
		assert.Equal(t, PoolStats{Size: 1}, instances.stats())
		assert.Equal(t, uint64(testLimit), budget.used.Load())

		next := acquireN(t, instances, 1)[0]
		assert.Equal(t, uint64(2), next.id, "the next caller starts an instance in the place given back")

		instances.release(held)
		instances.release(next)
		require.NoError(t, instances.close(t.Context()))
	})
}

func TestPoolRefillsToItsMinimumInTheBackground(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		spawner := &fakeSpawner{}
		instances := startPool(t, spawner, poolConfig{minSize: 3, maxSize: 4})

		for _, held := range acquireN(t, instances, 2) {
			instances.discard(t.Context(), held)
		}

		synctest.Wait()
		assert.Equal(t, PoolStats{Size: 3, Idle: 3, Refills: 2}, instances.stats(),
			"the refiller replaced both discarded instances; no caller started one")

		require.NoError(t, instances.close(t.Context()))
		assert.Equal(t, int32(5), spawner.closed.Load())
	})
}

// TestPoolStartsAnInstanceForWaitingCallers covers a discard while callers wait: the pool stays at its
// minimum, but the waiters' own attempt to grow came before the discard, so the refiller starts one for them.
func TestPoolStartsAnInstanceForWaitingCallers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		instances := startPool(t, &fakeSpawner{}, poolConfig{minSize: 1, maxSize: 2})
		held := acquireN(t, instances, 2)
		got := acquireLater(t, instances)

		synctest.Wait()
		instances.discard(t.Context(), held[0])

		next := <-got
		assert.Equal(t, uint64(3), next.id)
		assert.Equal(t, uint64(1), instances.stats().Refills)

		instances.release(next)
		instances.release(held[1])
		require.NoError(t, instances.close(t.Context()))
	})
}

func TestPoolRefillBacksOffExponentially(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		spawner, rec := &fakeSpawner{attempts: make(chan time.Time, 32)}, newRecorder()
		instances := startPool(t, spawner, poolConfig{logger: slog.New(rec), minSize: 1, maxSize: 1})
		<-spawner.attempts // warm's

		spawner.failures.Store(10)
		instances.discard(t.Context(), acquireN(t, instances, 1)[0])

		times := make([]time.Time, 11) // ten failures, then a success
		for i := range times {
			times[i] = <-spawner.attempts
		}

		synctest.Wait()

		var gaps []time.Duration

		for i := 1; i < len(times); i++ {
			gaps = append(gaps, times[i].Sub(times[i-1]))
		}

		assert.Equal(t, []time.Duration{
			100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond,
			1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond, 12800 * time.Millisecond,
			25600 * time.Millisecond, 30 * time.Second,
		}, gaps, "doubling from 100 ms, capped at 30 s")

		records := rec.drain()
		require.Len(t, records, 10, "every failure is reported")

		for i, record := range records {
			assert.Equal(t, slog.LevelWarn, record.Level)
			assert.Equal(t, "test/k", attr(&record, "key").String())
			assert.Equal(t, errFake.Error(), attr(&record, "error").String())
			assert.Equal(t, gaps[i], attr(&record, "retry_in").Duration())
		}

		assert.Equal(t, PoolStats{Size: 1, Idle: 1, Refills: 1}, instances.stats())

		spawner.failures.Store(1)
		instances.discard(t.Context(), acquireN(t, instances, 1)[0])

		first, second := <-spawner.attempts, <-spawner.attempts
		assert.Equal(t, 100*time.Millisecond, second.Sub(first), "a success reset the delay")

		synctest.Wait()
		require.NoError(t, instances.close(t.Context()))
	})
}

func TestPoolStopsGrowingAtTheBudget(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		spawner, rec := &fakeSpawner{}, newRecorder()
		budget := &memoryBudget{total: 2 * testLimit}
		instances := startPool(t, spawner, poolConfig{logger: slog.New(rec), budget: budget, minSize: 1, maxSize: 4})
		held := acquireN(t, instances, 2)

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		_, err := instances.acquire(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded, "under its maximum, but the budget is spent: callers wait")

		synctest.Wait()
		assert.Equal(t, 2, instances.stats().Size)
		assert.Equal(t, uint64(2*testLimit), budget.used.Load())

		records := rec.drain()
		require.NotEmpty(t, records, "the refiller tried to start an instance for the waiting caller")
		assert.Contains(t, attr(&records[0], "error").String(), ErrMemoryBudget.Error())

		// Another pool on the same budget cannot start until an instance is closed.
		otherCfg := poolConfig{
			logger: slog.New(slog.DiscardHandler), spawn: spawner.spawn, budget: budget, key: "test/other",
			limit: testLimit, minSize: 1, maxSize: 1,
		}
		require.ErrorIs(t, newPool(otherCfg).warm(t.Context()), ErrMemoryBudget)

		instances.discard(t.Context(), held[0])
		synctest.Wait()

		other := newPool(otherCfg)
		require.NoError(t, other.warm(t.Context()))

		instances.release(held[1])
		require.NoError(t, instances.close(t.Context()))
		require.NoError(t, other.close(t.Context()))
		assert.Zero(t, budget.used.Load())
	})
}

func TestPoolWarmFailureClosesWhatItStarted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want   error
		budget uint64
		quota  int32
	}{
		{want: errFake, quota: 2},
		{want: ErrMemoryBudget, budget: 2 * testLimit},
	}

	for _, tc := range tests {
		t.Run(tc.want.Error(), func(t *testing.T) {
			t.Parallel()

			spawner, budget := &fakeSpawner{quota: tc.quota}, &memoryBudget{total: tc.budget}
			instances := newPool(poolConfig{
				logger: slog.New(slog.DiscardHandler), spawn: spawner.spawn, budget: budget, key: "test/k",
				limit: testLimit, minSize: 3, maxSize: 3,
			})

			require.ErrorIs(t, instances.warm(t.Context()), tc.want)
			assert.Equal(t, int32(2), spawner.started.Load())
			assert.Equal(t, int32(2), spawner.closed.Load())
			assert.Zero(t, instances.size.Load())
			assert.Zero(t, budget.used.Load())
		})
	}
}

// closeLater starts closing instances and returns where the result arrives, once close is waiting.
func closeLater(t *testing.T, instances *pool) <-chan error {
	t.Helper()

	done := make(chan error, 1)

	go func() { done <- instances.close(t.Context()) }()

	synctest.Wait()

	return done
}

func TestPoolCloseWaitsForInstancesInUse(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		spawner := &fakeSpawner{}
		instances := startPool(t, spawner, poolConfig{minSize: 2, maxSize: 2})
		held := acquireN(t, instances, 1)[0]
		done := closeLater(t, instances)

		instances.release(held)
		require.NoError(t, <-done)
		assert.Equal(t, int32(2), spawner.closed.Load())

		_, err := instances.acquire(t.Context())
		require.ErrorIs(t, err, ErrClosed)
	})
}

func TestPoolCloseCollectsADiscard(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		instances := startPool(t, &fakeSpawner{}, poolConfig{minSize: 1, maxSize: 1})
		held := acquireN(t, instances, 1)[0]
		done := closeLater(t, instances)

		instances.discard(t.Context(), held)
		require.NoError(t, <-done)
	})
}

func TestPoolCloseGivesUpWaitingWhenItsContextEnds(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		spawner := &fakeSpawner{}
		instances := startPool(t, spawner, poolConfig{minSize: 1, maxSize: 1})
		held := acquireN(t, instances, 1)[0]

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		require.ErrorIs(t, instances.close(ctx), context.DeadlineExceeded)
		instances.release(held)
		assert.Zero(t, spawner.closed.Load(), "the runtime closes what is left")
	})
}

func TestPoolCloseFailsWaitingCallers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		instances := startPool(t, &fakeSpawner{}, poolConfig{minSize: 1, maxSize: 1})
		held := acquireN(t, instances, 1)[0]
		failed := make(chan error, 1)

		go func() {
			_, err := instances.acquire(t.Context())
			failed <- err
		}()

		synctest.Wait()

		done := closeLater(t, instances)

		require.ErrorIs(t, <-failed, ErrClosed)
		instances.release(held)
		require.NoError(t, <-done)
	})
}

func TestPoolCloseCollectsAnInstanceACallerStartedMeanwhile(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		spawner := &fakeSpawner{gate: make(chan struct{}, 1)}
		spawner.gate <- struct{}{} // warm's

		instances := startPool(t, spawner, poolConfig{minSize: 1, maxSize: 2})
		held := acquireN(t, instances, 1)[0]
		failed := make(chan error, 1)

		go func() {
			_, err := instances.acquire(t.Context()) // starts an instance, held at the gate
			failed <- err
		}()

		synctest.Wait()
		instances.release(held)

		done := closeLater(t, instances)

		spawner.gate <- struct{}{}

		require.ErrorIs(t, <-failed, ErrClosed)
		require.NoError(t, <-done)
		assert.Equal(t, int32(2), spawner.closed.Load())
	})
}

func TestPoolCloseStopsARefillerBackingOff(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		spawner := &fakeSpawner{}
		instances := startPool(t, spawner, poolConfig{minSize: 1, maxSize: 1})
		spawner.failures.Store(1 << 30)
		instances.discard(t.Context(), acquireN(t, instances, 1)[0])
		synctest.Wait()

		start := time.Now()

		require.NoError(t, instances.close(t.Context()))
		assert.Equal(t, start, time.Now(), "without waiting for the backoff")
	})
}

func TestPoolCloseStopsARefillerStartingAnInstance(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		spawner := &fakeSpawner{gate: make(chan struct{}, 1)}
		spawner.gate <- struct{}{} // warm's

		instances := startPool(t, spawner, poolConfig{minSize: 1, maxSize: 1})
		instances.discard(t.Context(), acquireN(t, instances, 1)[0])
		synctest.Wait()

		require.NoError(t, instances.close(t.Context()))
		assert.Equal(t, PoolStats{}, instances.stats())
	})
}

func TestPoolReportsInstancesFailingToClose(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		spawner := &fakeSpawner{closeErr: errFake}
		instances := startPool(t, spawner, poolConfig{logger: slog.New(rec), minSize: 2, maxSize: 2})

		instances.discard(t.Context(), acquireN(t, instances, 1)[0])
		synctest.Wait()

		records := rec.drain()
		require.Len(t, records, 1)
		assert.Equal(t, "plg: closing a discarded instance failed", records[0].Message)
		assert.Equal(t, uint64(1), attr(&records[0], "instance").Uint64())

		require.ErrorIs(t, instances.close(t.Context()), errFake)
	})
}

// TestPoolUnderContention runs 64 goroutines × 1000 acquire/release cycles, discarding every 97th instance:
// no instance is ever held twice, the pool and the budget stay within their bounds, and close closes every
// instance started.
func TestPoolUnderContention(t *testing.T) {
	t.Parallel()

	const workers, rounds, maxSize = 64, 1000, 8

	spawner, budget := &fakeSpawner{}, &memoryBudget{total: maxSize * testLimit}
	instances := startPool(t, spawner, poolConfig{budget: budget, minSize: 2, maxSize: maxSize})
	errs := make(chan error, workers)

	var wg sync.WaitGroup

	for range workers {
		wg.Go(func() { errs <- hammerPool(t.Context(), instances, rounds) })
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	require.NoError(t, instances.close(t.Context()))
	assert.Equal(t, spawner.started.Load(), spawner.closed.Load(), "every instance started was closed")
	assert.Zero(t, budget.used.Load())
}

func hammerPool(ctx context.Context, instances *pool, rounds int) error {
	for round := range rounds {
		held, err := instances.acquire(ctx)
		if err != nil {
			return err
		}

		inst, ok := held.inst.(*fakeInstance)
		if !ok || !inst.inUse.CompareAndSwap(false, true) {
			return errSharedInstance
		}

		if instances.size.Load() > instances.maxSize || instances.budget.used.Load() > instances.budget.total {
			return errOverBound
		}

		runtime.Gosched()
		inst.inUse.Store(false)

		if round%97 == 0 {
			instances.discard(ctx, held)
		} else {
			instances.release(held)
		}
	}

	return nil
}

func TestMemoryBudgetAccounting(t *testing.T) {
	t.Parallel()

	budget := &memoryBudget{total: 10}
	assert.True(t, budget.reserve(4))
	assert.False(t, budget.reserve(7), "one byte over")
	assert.True(t, budget.reserve(6), "exactly full")
	assert.False(t, budget.reserve(1))
	budget.unreserve(6)
	assert.True(t, budget.reserve(6), "given back")
	assert.Equal(t, uint64(10), budget.reserved.Load())

	assert.True(t, budget.acquire(10), "instances are charged apart from reservations")
	assert.False(t, budget.acquire(1))
	budget.release(10)
	assert.Zero(t, budget.used.Load())

	unlimited := &memoryBudget{}
	assert.True(t, unlimited.reserve(1<<62))
	assert.True(t, unlimited.reserve(1<<62), "no budget refuses nothing")
	assert.True(t, unlimited.acquire(1<<62))
}

// BenchmarkPoolAcquireRelease is one call's pool overhead with an instance idle.
func BenchmarkPoolAcquireRelease(b *testing.B) {
	instances := startPool(b, &fakeSpawner{}, poolConfig{minSize: 1, maxSize: 1})
	b.Cleanup(func() { assert.NoError(b, instances.close(context.Background())) })

	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		held, err := instances.acquire(ctx)
		if err != nil {
			b.Fatal(err)
		}

		instances.release(held)
	}
}

// BenchmarkPoolAcquireReleaseParallel is the pool overhead under contention: GOMAXPROCS goroutines share one
// instance (they wait for each other) or as many instances as goroutines.
func BenchmarkPoolAcquireReleaseParallel(b *testing.B) {
	for _, size := range []int{1, runtime.GOMAXPROCS(0)} {
		b.Run("pool="+strconv.Itoa(size), func(b *testing.B) {
			instances := startPool(b, &fakeSpawner{}, poolConfig{minSize: size, maxSize: size})
			b.Cleanup(func() { assert.NoError(b, instances.close(context.Background())) })

			ctx := b.Context()

			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					held, err := instances.acquire(ctx)
					if err != nil {
						b.Error(err)

						return
					}

					instances.release(held)
				}
			})
		})
	}
}
