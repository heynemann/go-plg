package plg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
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
	attempts chan time.Time                // receives the time of every start, when non-nil
	gate     chan struct{}                 // when non-nil, every start waits for a value from it, or for its context
	call     func() (engine.Result, error) // the instances' Call, when non-nil
	closeErr error                         // returned by the instances' Close
	guest    string                        // the instances' Guest
	failures atomic.Int32                  // how many of the next starts fail
	panics   atomic.Int32                  // how many of the next starts panic
	quota    int32                         // when non-zero, starts beyond this many fail
	started  atomic.Int32
	closed   atomic.Int32
	closeBug atomic.Bool // the instances' Close panics
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

	if s.panics.Add(-1) >= 0 {
		panic("spawn bug")
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

func (f *fakeInstance) Call(context.Context, uint32, []byte) (engine.Result, error) {
	if f.owner.call != nil {
		return f.owner.call()
	}

	return engine.Result{}, nil
}

func (*fakeInstance) Broken() bool { return false }

func (f *fakeInstance) Close(context.Context) error {
	f.owner.closed.Add(1)

	if f.owner.closeBug.Load() {
		panic("close bug")
	}

	return f.owner.closeErr
}

func (f *fakeInstance) Guest() string { return f.owner.guest }

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

// reporting returns a panic reporter keeping 16 entries, logging to handler.
func reporting(handler slog.Handler) *panicReporter {
	return newPanicReporter(slog.New(handler), nil, 16)
}

// newTestPool returns a pool of sp's instances; cfg's spawn, key, language and limit are set, and its panic
// reporter and budget default to a discarding one and no budget.
func newTestPool(spawner *fakeSpawner, cfg poolConfig) *pool {
	cfg.spawn, cfg.key, cfg.lang, cfg.limit = spawner.spawn, "test/k", LanguageGo, testLimit
	if cfg.panics == nil {
		cfg.panics = reporting(slog.DiscardHandler)
	}

	if cfg.budget == nil {
		cfg.budget = &memoryBudget{}
	}

	return newPool(cfg)
}

// warmPool warms a pool built by newTestPool.
func warmPool(tb testing.TB, spawner *fakeSpawner, cfg poolConfig) *pool {
	tb.Helper()

	instances := newTestPool(spawner, cfg)
	require.NoError(tb, instances.warm(tb.Context()))

	return instances
}

// startPool warms a pool built by newTestPool in a synctest bubble, then waits for its refiller to find the pool
// at its minimum and wait for a signal. Otherwise the refiller's first look at the pool races the test's first
// callers: seeing one waiting, it could start the instance the test expects that caller to start.
func startPool(t *testing.T, spawner *fakeSpawner, cfg poolConfig) *pool {
	t.Helper()

	instances := warmPool(t, spawner, cfg)

	synctest.Wait()

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
		instances := startPool(t, spawner, poolConfig{panics: reporting(rec), minSize: 1, maxSize: 1})
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
		instances := startPool(t, spawner, poolConfig{panics: reporting(rec), budget: budget, minSize: 1, maxSize: 4})
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
			panics: reporting(slog.DiscardHandler), spawn: spawner.spawn, budget: budget, key: "test/other",
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
				panics: reporting(slog.DiscardHandler), spawn: spawner.spawn, budget: budget, key: "test/k",
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
		instances := startPool(t, spawner, poolConfig{panics: reporting(rec), minSize: 2, maxSize: 2})

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
	instances := warmPool(t, spawner, poolConfig{budget: budget, minSize: 2, maxSize: maxSize})
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
	instances := warmPool(b, &fakeSpawner{}, poolConfig{minSize: 1, maxSize: 1})
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
	for _, size := range []int32{1, int32(runtime.GOMAXPROCS(0))} { //nolint:gosec // G115: a handful of CPUs
		b.Run("pool="+strconv.Itoa(int(size)), func(b *testing.B) {
			instances := warmPool(b, &fakeSpawner{}, poolConfig{minSize: size, maxSize: size})
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

// requirePanicError extracts the PanicError from err, checks its key, language and phase and returns a copy.
func requirePanicError(t *testing.T, err error, phase Phase) PanicError {
	t.Helper()

	failure, ok := errors.AsType[*PanicError](err)
	require.True(t, ok, "a PanicError: %v", err)
	assert.Equal(t, "test/k", failure.Key)
	assert.Equal(t, LanguageGo, failure.Language)
	assert.Equal(t, phase, failure.Phase)
	assert.False(t, failure.Time.IsZero())

	return *failure
}

func TestPoolRecoversPanicsStartingInstances(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		reporter := newPanicReporter(slog.New(slog.DiscardHandler), nil, 16)
		spawner := &fakeSpawner{attempts: make(chan time.Time, 16)}
		spawner.panics.Store(1)

		cold := newTestPool(spawner, poolConfig{panics: reporter, minSize: 1, maxSize: 2})
		failure := requirePanicError(t, cold.warm(t.Context()), PhaseLoad)
		assert.Equal(t, "spawn bug", failure.Value)
		assert.Contains(t, string(failure.Stack), "(*fakeSpawner).spawn", "the stack where it panicked")
		assert.Equal(t, PoolStats{}, cold.stats(), "the place was given back")
		<-spawner.attempts

		instances := startPool(t, spawner, poolConfig{panics: reporter, minSize: 1, maxSize: 2})
		<-spawner.attempts

		held := acquireN(t, instances, 1)[0]

		spawner.panics.Store(1)

		_, err := instances.acquire(t.Context()) // nothing idle: the caller starts one
		requirePanicError(t, err, PhaseCall)
		<-spawner.attempts
		synctest.Wait()
		assert.Equal(t, 1, instances.stats().Size, "the caller's place was given back")

		spawner.panics.Store(1)
		instances.discard(t.Context(), held) // the refiller replaces it: a panic, then a success

		first, second := <-spawner.attempts, <-spawner.attempts
		assert.Equal(t, refillBackoffMin, second.Sub(first), "the panic was a failure: the refiller backed off")

		synctest.Wait()
		assert.Equal(t, PoolStats{Size: 1, Idle: 1, Refills: 1}, instances.stats())

		recent := reporter.history.recent()
		require.Len(t, recent, 3)
		assert.Equal(t, failure, *recent[0])
		assert.Equal(t, PhaseCall, recent[1].Phase)
		assert.Equal(t, PhaseRefill, recent[2].Phase)

		require.NoError(t, instances.close(t.Context()))
	})
}

// panickingHandler is a slog handler whose every record panics.
type panickingHandler struct{}

func (panickingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h panickingHandler) WithAttrs([]slog.Attr) slog.Handler     { return h }
func (h panickingHandler) WithGroup(string) slog.Handler          { return h }

func (panickingHandler) Handle(context.Context, slog.Record) error {
	panic("log handler bug")
}

func TestRefillerSurvivesAPanickingLogger(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		reporter := reporting(panickingHandler{})
		spawner := &fakeSpawner{attempts: make(chan time.Time, 4)}
		instances := startPool(t, spawner, poolConfig{panics: reporter, minSize: 1, maxSize: 1})
		<-spawner.attempts

		spawner.failures.Store(1)
		instances.discard(t.Context(), acquireN(t, instances, 1)[0]) // the failure's Warn log panics

		first, second := <-spawner.attempts, <-spawner.attempts
		assert.GreaterOrEqual(t, second.Sub(first), refillBackoffMin, "the panic counted as a failure")

		synctest.Wait()
		assert.Equal(t, PoolStats{Size: 1, Idle: 1, Refills: 1}, instances.stats(), "the refiller went on")

		recent := reporter.history.recent()
		require.Len(t, recent, 2, "the refiller's panic, then the logger's own when it was reported")
		requirePanicError(t, recent[0], PhaseRefill)
		assert.Equal(t, "log handler bug", recent[0].Value)
		requirePanicError(t, recent[1], PhaseCallback)
		assert.Equal(t, "slog.Handler.Handle", recent[1].Method)

		require.NoError(t, instances.close(t.Context()))
	})
}

func TestPoolReportsPanicsClosingInstances(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		reporter := reporting(rec)
		spawner := &fakeSpawner{}
		instances := startPool(t, spawner, poolConfig{panics: reporter, minSize: 1, maxSize: 1})

		spawner.closeBug.Store(true)
		instances.discard(t.Context(), acquireN(t, instances, 1)[0])
		synctest.Wait()

		recent := reporter.history.recent()
		require.Len(t, recent, 1)

		failure := requirePanicError(t, recent[0], PhaseClose)
		assert.Equal(t, "close bug", failure.Value)
		assert.Contains(t, string(failure.Stack), "(*fakeInstance).Close")

		records := rec.drain()
		require.Len(t, records, 2)
		assert.Equal(t, "plg: plugin failure", records[0].Message)
		assert.Equal(t, "plg: closing a discarded instance failed", records[1].Message)
		assert.Equal(t, PoolStats{Size: 1, Idle: 1, Refills: 1}, instances.stats(), "the place was given back")

		err := instances.close(t.Context())
		requirePanicError(t, err, PhaseClose)
		assert.Zero(t, instances.stats().Size, "every place given back")
	})
}

var errTrap = errors.New("wasm error: unreachable")

func TestCallFailuresArePanicErrors(t *testing.T) {
	t.Parallel()

	meth, err := newMethod(new(wire.Planner), reflect.TypeFor[pair](), 0, 0)
	require.NoError(t, err)

	tests := []struct {
		call  func() (engine.Result, error)
		value func(t *testing.T, pe *PanicError)
		name  string
		guest string // what the instance reports
	}{
		{
			name: "trap", guest: "fatal error: out of memory",
			call:  func() (engine.Result, error) { return engine.Result{}, errTrap },
			value: func(t *testing.T, pe *PanicError) { t.Helper(); require.ErrorIs(t, pe, errTrap) },
		},
		{
			name: "guest panic", guest: "plugin.go:3:1: panic",
			call: func() (engine.Result, error) {
				return engine.Result{Status: wire.StatusPanic, Message: "boom"}, nil
			},
			value: func(t *testing.T, pe *PanicError) { t.Helper(); assert.Equal(t, "boom", pe.Value) },
		},
		{
			name: "guest ABI rejection",
			call: func() (engine.Result, error) {
				return engine.Result{Status: wire.StatusABI, Message: "unknown method"}, nil
			},
			value: func(t *testing.T, pe *PanicError) {
				t.Helper()
				require.ErrorIs(t, pe, errGuestStatus)
				require.ErrorContains(t, pe, "guest status abi: unknown method")
			},
		},
		{
			name: "undecodable results",
			call: func() (engine.Result, error) { return engine.Result{Status: wire.StatusOK}, nil },
			value: func(t *testing.T, pe *PanicError) {
				t.Helper()
				require.ErrorIs(t, pe, engine.ErrABI)
				require.ErrorIs(t, pe, wire.ErrTruncated)
			},
		},
		{
			name: "host panic",
			call: func() (engine.Result, error) { panic("host bug") },
			value: func(t *testing.T, pe *PanicError) {
				t.Helper()
				assert.Equal(t, "host bug", pe.Value)
				assert.Contains(t, string(pe.Stack), "(*plugin).exchange", "recovered at the call boundary")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reporter := newPanicReporter(slog.New(slog.DiscardHandler), nil, 16)
			spawner := &fakeSpawner{call: tc.call, guest: tc.guest}
			instances := warmPool(t, spawner, poolConfig{panics: reporter, minSize: 1, maxSize: 1})
			plug := &plugin{key: "test/k", desc: Descriptor{Language: LanguageGo}, pool: instances}

			_, err := plug.call(t.Context(), meth, []reflect.Value{reflect.ValueOf(t.Context())})

			assert.Equal(t, PhaseCall, requirePanicError(t, err, PhaseCall).Phase)
			require.ErrorIs(t, err, ErrCall)

			failure, ok := errors.AsType[*PanicError](err)
			require.True(t, ok)
			assert.Equal(t, "M", failure.Method)
			tc.value(t, failure)

			if failure.Stack == nil {
				assert.Equal(t, tc.guest, failure.Guest, "a guest failure carries the guest's output")
			} else {
				assert.Empty(t, failure.Guest, "a host panic is not the guest's")
			}

			assert.Equal(t, []*PanicError{failure}, reporter.history.recent(), "reported once")
			assert.Equal(t, int32(1), spawner.closed.Load(), "the instance was discarded")
			require.NoError(t, instances.close(t.Context()))
		})
	}
}

func TestPanicReporter(t *testing.T) {
	t.Parallel()

	t.Run("logs, records and calls back", func(t *testing.T) {
		t.Parallel()

		rec := newRecorder()

		var seen []*PanicError

		reporter := newPanicReporter(slog.New(rec), func(_ context.Context, failure *PanicError) {
			seen = append(seen, failure)
		}, 4)
		before := time.Now()
		full := &PanicError{
			Key: "n/k", Language: LanguageGo, Phase: PhaseCall, Method: "M", Value: "v", Guest: "g", Stack: []byte("s"),
		}
		bare := &PanicError{Key: "n", Phase: PhaseStorage, Value: errFake}

		reporter.report(t.Context(), full)
		reporter.report(t.Context(), bare)

		assert.False(t, full.Time.Before(before), "stamped when reported")
		assert.Equal(t, []*PanicError{full, bare}, seen, "the same values the callback got")
		assert.Equal(t, []*PanicError{full, bare}, reporter.history.recent())

		records := rec.drain()
		require.Len(t, records, 2)

		for _, record := range records {
			assert.Equal(t, slog.LevelError, record.Level)
			assert.Equal(t, "plg: plugin failure", record.Message)
		}

		for key, want := range map[string]string{
			"key": "n/k", "language": "go", "phase": "call", "method": "M", "value": "v", "guest": "g", "stack": "s",
		} {
			assert.Equal(t, want, attr(&records[0], key).String(), key)
		}

		assert.Equal(t, 3, records[1].NumAttrs(), "only the fields set: key, phase and value")
		assert.Equal(t, errFake.Error(), attr(&records[1], "value").String())
	})

	t.Run("a panicking callback is recorded, not called again", func(t *testing.T) {
		t.Parallel()

		rec := newRecorder()
		calls := 0
		reporter := newPanicReporter(slog.New(rec), func(context.Context, *PanicError) {
			calls++

			panic("callback bug")
		}, 4)

		failure := &PanicError{Key: "n/k", Language: LanguageGo, Phase: PhaseHTTP, Value: 1}
		reporter.report(t.Context(), failure)

		assert.Equal(t, 1, calls)

		recent := reporter.history.recent()
		require.Len(t, recent, 2)
		assert.Same(t, failure, recent[0])
		assert.Equal(t, PhaseCallback, recent[1].Phase)
		assert.Equal(t, "n/k", recent[1].Key)
		assert.Equal(t, LanguageGo, recent[1].Language)
		assert.Equal(t, "OnPanic", recent[1].Method)
		assert.Equal(t, "callback bug", recent[1].Value)
		assert.Contains(t, string(recent[1].Stack), "TestPanicReporter")
		assert.Len(t, rec.drain(), 2, "both logged")
	})

	t.Run("a panicking logger is recorded, not logged", func(t *testing.T) {
		t.Parallel()

		calls := 0
		reporter := newPanicReporter(slog.New(panickingHandler{}), func(context.Context, *PanicError) { calls++ }, 4)
		failure := &PanicError{Key: "n/k", Phase: PhaseCall, Value: 1}
		reporter.report(t.Context(), failure)

		recent := reporter.history.recent()
		require.Len(t, recent, 2)
		assert.Same(t, failure, recent[0])
		assert.Equal(t, PhaseCallback, recent[1].Phase)
		assert.Equal(t, "slog.Handler.Handle", recent[1].Method)
		assert.Equal(t, "log handler bug", recent[1].Value)
		assert.Equal(t, 1, calls, "the callback still ran")
	})
}

func TestHTTPPanicsArePanicErrors(t *testing.T) {
	t.Parallel()

	ctr := New(WithPanicHistory(2))

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	ctr.httpPanicked("test/k", LanguageGo)(t.Context(), "transport bug")

	recent := ctr.RecentPanics()
	require.Len(t, recent, 1)

	failure := requirePanicError(t, recent[0], PhaseHTTP)
	assert.Equal(t, "transport bug", failure.Value)
	assert.Contains(t, string(failure.Stack), "TestHTTPPanicsArePanicErrors")
}

func TestPanicHistoryKeepsTheNewest(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		want    []int
		size    int
		records int
	}{
		{size: 3, records: 0, want: nil},
		{size: 3, records: 2, want: []int{0, 1}},
		{size: 3, records: 3, want: []int{0, 1, 2}},
		{size: 3, records: 7, want: []int{4, 5, 6}},
		{size: 1, records: 2, want: []int{1}},
		{size: 0, records: 2, want: nil},
		{size: -1, records: 2, want: nil},
	} {
		reporter := newPanicReporter(slog.New(slog.DiscardHandler), nil, tc.size)
		for i := range tc.records {
			reporter.history.record(&PanicError{Value: i})
		}

		recent := reporter.history.recent()
		got := make([]int, 0, len(recent))

		for _, failure := range recent {
			v, ok := failure.Value.(int)
			require.True(t, ok)

			got = append(got, v)
		}

		if tc.want == nil {
			assert.Nil(t, recent, "size %d, %d recorded", tc.size, tc.records)

			continue
		}

		assert.Equal(t, tc.want, got, "size %d, %d recorded", tc.size, tc.records)
	}
}

// historyMark is the value of the PanicErrors TestPanicHistoryUnderConcurrency records.
type historyMark struct{ writer, round int }

var errOutOfOrder = errors.New("history entries out of order")

// historyInOrder checks a history read: at most size entries, each writer's in the order it recorded them.
func historyInOrder(entries []*PanicError, size int) error {
	if len(entries) > size {
		return fmt.Errorf("%w: %d entries", errOverBound, len(entries))
	}

	last := map[int]int{}

	for _, failure := range entries {
		mark, ok := failure.Value.(historyMark)
		if prev, seen := last[mark.writer]; !ok || (seen && mark.round <= prev) {
			return fmt.Errorf("%w: %v after round %d", errOutOfOrder, mark, prev)
		}

		last[mark.writer] = mark.round
	}

	return nil
}

// TestPanicHistoryUnderConcurrency records from 8 goroutines while 4 read: every read returns at most the
// history's size, with each writer's entries in the order it recorded them; once the writers are done, the
// history is full.
func TestPanicHistoryUnderConcurrency(t *testing.T) {
	t.Parallel()

	const (
		size    = 64
		writers = 8
		rounds  = 2000
	)

	history := &newPanicReporter(slog.New(slog.DiscardHandler), nil, size).history

	var (
		writing, reading sync.WaitGroup
		done             atomic.Bool
		readErr          atomic.Pointer[error]
	)

	for w := range writers {
		writing.Go(func() {
			for r := range rounds {
				history.record(&PanicError{Value: historyMark{w, r}})
			}
		})
	}

	for range 4 {
		reading.Go(func() {
			for !done.Load() {
				err := historyInOrder(history.recent(), size)
				if err != nil {
					readErr.CompareAndSwap(nil, &err)
				}
			}
		})
	}

	writing.Wait()
	done.Store(true)
	reading.Wait()

	if failed := readErr.Load(); failed != nil {
		require.NoError(t, *failed, "a read while writing")
	}

	final := history.recent()
	require.NoError(t, historyInOrder(final, size))
	assert.Len(t, final, size, "quiescent: the ring is full")
}

// TestPanicHistoryKeepsANewerEntryFromALateWriter has a writer store its entry after a newer one took the same
// slot, as one delayed between taking its sequence number and storing does: the newer entry stays.
func TestPanicHistoryKeepsANewerEntryFromALateWriter(t *testing.T) {
	t.Parallel()

	history := &newPanicReporter(slog.New(slog.DiscardHandler), nil, 2).history
	late := history.next.Add(1) - 1
	newer := []*PanicError{{Value: "second"}, {Value: "third"}} // the third takes the late first's slot

	for _, failure := range newer {
		history.record(failure)
	}

	history.store(late, &PanicError{Value: "first"})
	assert.Equal(t, newer, history.recent(), "the newest entries, the late one out")
}

// unreadable is a filesystem whose named file cannot be read.
type unreadable struct {
	fstest.MapFS

	name string
}

func (u unreadable) ReadFile(name string) ([]byte, error) {
	if name == u.name {
		return nil, fs.ErrPermission
	}

	return u.MapFS.ReadFile(name) //nolint:wrapcheck // the test filesystem's own error
}

func TestCheckNoGoStatements(t *testing.T) {
	t.Parallel()

	src := func(body string) *fstest.MapFile { return &fstest.MapFile{Data: []byte("package p\n\n" + body)} }
	clean := src("func f() { defer g() }\n")
	spawns := src("func f() {\n\tgo g()\n}\n")

	for _, tc := range []struct {
		files fs.FS
		want  error
		name  string
		dir   string
		msg   string
	}{
		{name: "clean", files: fstest.MapFS{"p/a.go": clean, "p/notes.txt": {Data: []byte("go g()")}}, dir: "p"},
		{
			name: "every position, nested and in subdirectories", dir: "p", want: ErrGoStatement,
			files: fstest.MapFS{
				"p/a.go":     spawns,
				"p/sub/b.go": src("var f = func() {\n\tfunc() { go g() }()\n}\n"),
			},
			msg: "p/a.go:4:2, p/sub/b.go:4:11",
		},
		{
			name: "only below the package directory", dir: "p",
			files: fstest.MapFS{"p/a.go": clean, "other/b.go": spawns},
		},
		{
			name: "tests and links are not loaded", dir: ".",
			files: fstest.MapFS{"a_test.go": spawns, "link.go": {Mode: fs.ModeSymlink, Data: []byte("x")}},
		},
		{
			name: "syntax errors fail as the guest would", dir: ".", want: errGuestLoad,
			files: fstest.MapFS{"a.go": {Data: []byte("package p\nfunc {")}}, msg: "a.go:2:6",
		},
		{
			name: "unreadable file", dir: ".", want: fs.ErrPermission,
			files: unreadable{fstest.MapFS{"a.go": clean}, "a.go"},
		},
		{name: "missing directory", dir: "nope", want: fs.ErrNotExist, files: fstest.MapFS{"a.go": clean}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkNoGoStatements(tc.files, tc.dir)
			if tc.want == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), tc.msg)
		})
	}
}

func TestGuestFailureValues(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "boom", guestFailure(engine.Result{Status: wire.StatusPanic, Message: "boom"}),
		"a panic's value is its message")

	abi, ok := guestFailure(engine.Result{Status: wire.StatusABI, Message: "bad payload"}).(error)
	require.True(t, ok, "any other status is an error naming it")
	require.ErrorIs(t, abi, errGuestStatus)
	assert.EqualError(t, abi, "guest status abi: bad payload")
}

// isClosed reports whether ch is closed, without waiting.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func testGeneration() *plugin {
	return newGeneration("test/k", reflect.TypeFor[pair](), &Descriptor{Key: "k"}, nil)
}

func TestGenerationGoesIdleAfterItsLastCall(t *testing.T) {
	t.Parallel()

	t.Run("serving", func(t *testing.T) {
		t.Parallel()

		gen := testGeneration()
		require.True(t, gen.enter())
		gen.leave()
		assert.False(t, isClosed(gen.idle), "a generation still served never goes idle")
	})

	t.Run("retired without calls", func(t *testing.T) {
		t.Parallel()

		gen := testGeneration()
		gen.retire()
		assert.True(t, isClosed(gen.idle))
		assert.False(t, gen.enter(), "a retired generation takes no call")
	})

	t.Run("retired with calls in flight", func(t *testing.T) {
		t.Parallel()

		gen := testGeneration()
		require.True(t, gen.enter())
		require.True(t, gen.enter())
		gen.retire()
		assert.False(t, isClosed(gen.idle), "two calls are in flight")
		assert.False(t, gen.enter(), "a retired generation takes no call")
		assert.False(t, isClosed(gen.idle), "a refused call does not count")
		gen.leave()
		assert.False(t, isClosed(gen.idle), "one call is in flight")
		gen.leave()
		assert.True(t, isClosed(gen.idle), "the last call left")
	})
}

// TestGenerationRetiringUnderLoad retires a generation while goroutines keep calling through it: no call sees it
// idle while in flight, it goes idle exactly once (closing idle twice would panic) once every call that entered
// has left, and it takes no call afterwards.
func TestGenerationRetiringUnderLoad(t *testing.T) {
	t.Parallel()

	const workers = 32

	gen := testGeneration()

	var calls, refused, violations atomic.Int64

	var wg sync.WaitGroup

	for range workers {
		wg.Go(func() {
			for gen.enter() {
				if isClosed(gen.idle) {
					violations.Add(1)
				}

				calls.Add(1)
				gen.leave()
				runtime.Gosched()
			}

			refused.Add(1)
		})
	}

	for calls.Load() < 100*workers { // retire in the middle of the traffic
		runtime.Gosched()
	}

	gen.retire()
	wg.Wait()

	assert.True(t, isClosed(gen.idle))
	assert.Zero(t, violations.Load(), "no call ran on an idle generation")
	assert.Equal(t, int64(workers), refused.Load(), "every worker was refused once retired")
	assert.Equal(t, int64(retiredBit), gen.state.Load(), "nothing is left in flight")
}

// startReloader runs a reloader whose rounds call round, until the test ends.
func startReloader(t *testing.T, interval time.Duration, round func(context.Context) error) *reloader {
	t.Helper()

	ctx, stop := context.WithCancel(context.Background())
	reloads := newReloader(stop)

	go reloads.run(ctx, interval, round)

	t.Cleanup(func() {
		stop()
		<-reloads.stopped
	})

	return reloads
}

var errRound = errors.New("round failed")

// TestReloaderCoalescesRequestsMadeDuringARound requests a reload while a round runs: that round started listing
// before the requests, so it cannot serve them; the next one serves them all.
func TestReloaderCoalescesRequestsMadeDuringARound(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		gate := make(chan error)

		var rounds atomic.Int32

		reloads := startReloader(t, 0, func(context.Context) error {
			rounds.Add(1)

			return <-gate
		})

		first := reloads.request()

		synctest.Wait()
		require.Equal(t, int32(1), rounds.Load(), "the first request starts a round")

		later := []*reloadRound{reloads.request(), reloads.request(), reloads.request()}

		gate <- errRound

		require.ErrorIs(t, first.wait(t.Context()), errRound)

		synctest.Wait()
		require.Equal(t, int32(2), rounds.Load(), "one more round serves the three later requests")

		gate <- nil

		for _, round := range later {
			require.NoError(t, round.wait(t.Context()), "the later requests get the second round's outcome")
		}

		synctest.Wait()
		assert.Equal(t, int32(2), rounds.Load(), "no request is left pending")
	})
}

func TestReloaderPollsEveryInterval(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var polled, unpolled atomic.Int32

		startReloader(t, time.Minute, func(context.Context) error {
			polled.Add(1)

			return nil
		})

		manual := startReloader(t, 0, func(context.Context) error {
			unpolled.Add(1)

			return nil
		})

		time.Sleep(time.Minute - time.Nanosecond)
		synctest.Wait()
		assert.Zero(t, polled.Load(), "the first poll comes one interval after the start")

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		assert.Equal(t, int32(1), polled.Load())

		time.Sleep(2 * time.Minute)
		synctest.Wait()
		assert.Equal(t, int32(3), polled.Load())
		assert.Zero(t, unpolled.Load(), "without an interval, nothing polls")

		require.NoError(t, manual.request().wait(t.Context()))
		assert.Equal(t, int32(1), unpolled.Load(), "requests still reload")
	})
}

func TestReloaderStops(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(context.Background())
		reloads := newReloader(stop)

		go reloads.run(ctx, 0, func(ctx context.Context) error {
			<-ctx.Done()

			return context.Cause(ctx)
		})

		running := reloads.request()

		synctest.Wait()

		pending := reloads.request()

		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, pending.wait(canceled), context.Canceled, "waiting ends with its context")

		stop()
		<-reloads.stopped

		require.ErrorIs(t, running.wait(t.Context()), context.Canceled, "the round in progress is stopped")
		require.ErrorIs(t, pending.wait(t.Context()), ErrClosed, "the pending round never runs")
		require.ErrorIs(t, reloads.request().wait(t.Context()), ErrClosed, "nor does any later one")
	})
}
