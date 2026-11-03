//go:build integration

package plg_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

const mib = 1 << 20

// instanceID returns the identity of the instance the plugin's next call would get, without making the call.
func instanceID(tb testing.TB, ctr *plg.Container, key string) uint64 {
	tb.Helper()

	held, err := plg.HoldInstance(tb.Context(), ctr, key)
	require.NoError(tb, err)
	held.Release()

	return held.ID
}

// TestPoolServesManyConcurrentCalls runs 64 goroutines making calls through one plugin served by up to 8
// instances, with ordinary errors and instance-discarding panics mixed in: every call gets its own answer, and
// the pool stays within its bounds.
func TestPoolServesManyConcurrentCalls(t *testing.T) {
	t.Parallel()

	const workers, calls, maxSize = 64, 100, 8

	ctr := fooerContainer(t, map[string]string{"ref": "fooer"}, plg.WithPool(2, maxSize))
	ref := getFooer(t, ctr, "fooer/ref")
	errs := make(chan error, workers)

	var wg sync.WaitGroup

	for worker := range workers {
		wg.Go(func() {
			for call := range calls {
				err := checkedCall(t.Context(), ref, fmt.Sprintf("w%d-c%d", worker, call), call)
				if err != nil {
					errs <- err

					return
				}
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	stats := plg.Stats(ctr, "fooer/ref")
	assert.LessOrEqual(t, stats.Size, maxSize)
	assert.LessOrEqual(t, plg.MemoryInUse(ctr), uint64(maxSize)*128*mib)
}

// checkedCall makes one call through ref, chosen by n: mostly Foo, every tenth an ordinary error and every
// twenty-fifth a panic, and checks its outcome.
func checkedCall(ctx context.Context, ref testcontract.Fooer, msg string, n int) error {
	switch {
	case n%25 == 24:
		err := ref.Fail(ctx, "panic")
		if !errors.Is(err, plg.ErrCall) {
			return fmt.Errorf("%w: a panic returned %w", errUnexpected, err)
		}
	case n%10 == 9:
		var pluginErr *plg.PluginError

		err := ref.Fail(ctx, msg)
		if !errors.As(err, &pluginErr) || pluginErr.Message != msg {
			return fmt.Errorf("%w: Fail(%q) returned %w", errUnexpected, msg, err)
		}
	default:
		got, err := ref.Foo(ctx, msg)
		if err != nil || got != "hello, "+msg {
			return fmt.Errorf("%w: Foo(%q) returned %q, %w", errUnexpected, msg, got, err)
		}
	}

	return nil
}

func TestMemoryLimitFailsOnlyTheCallOverIt(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	registerFooers(t, ctr, "hog", map[string]string{"k": "hog"}, plg.WithPool(2, 2),
		plg.WithMemoryLimit(64*mib+1000)) // rounded down to whole pages
	registerFooers(t, ctr, "ref", map[string]string{"k": "fooer"})

	hog, ref := getFooer(t, ctr, "hog/k"), getFooer(t, ctr, "ref/k")

	kept, err := plg.HoldInstance(t.Context(), ctr, "hog/k")
	require.NoError(t, err)

	got, err := hog.Spin(t.Context(), 80) // on the other instance
	require.ErrorIs(t, err, plg.ErrCall, "80 MiB do not fit in 64 MiB")
	assert.Zero(t, got)
	kept.Release()

	first, err := plg.HoldInstance(t.Context(), ctr, "hog/k")
	require.NoError(t, err)

	second, err := plg.HoldInstance(t.Context(), ctr, "hog/k")
	require.NoError(t, err)

	assert.ElementsMatch(t, []uint64{kept.ID, 3}, []uint64{first.ID, second.ID},
		"the instance over its limit was replaced by a third one; the other was kept")
	assert.Equal(t, uint64(2*64*mib+128*mib), plg.MemoryInUse(ctr), "two hog instances and one ref instance")

	first.Release()
	second.Release()

	got, err = hog.Spin(t.Context(), 8)
	require.NoError(t, err)
	assert.Equal(t, 8, got, "the plugin keeps working within its limit")

	greeting, err := ref.Foo(t.Context(), "neighbour")
	require.NoError(t, err)
	assert.Equal(t, "hello, neighbour", greeting, "other plugins are unaffected")
}

func TestDescriptorMemoryLimitOnlyLowersTheRegisterLimit(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)

	tests := []struct {
		name  string
		opts  []plg.RegisterOption
		asked uint64 // the descriptor's MemoryLimit
		fits  bool   // whether 80 MiB fit
	}{
		{name: "register-limit", fits: true},
		{name: "lowered", asked: 64 * mib},
		{name: "raised", opts: []plg.RegisterOption{plg.WithMemoryLimit(64 * mib)}, asked: 256 * mib},
	}

	for _, tc := range tests {
		store := plg.NewMemoryStorage(tc.name)
		desc := plg.Descriptor{
			Key:         "k",
			Language:    plg.LanguageGo,
			Entrypoint:  ".",
			Version:     "v1",
			MemoryLimit: tc.asked,
		}
		require.NoError(t, store.Put(desc, pluginFiles("hog")))

		opts := slices.Concat(tc.opts, []plg.RegisterOption{plg.WithPool(1, 1), plg.WithStorage(store)})
		require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, tc.name, opts...))

		got, err := getFooer(t, ctr, tc.name+"/k").Spin(t.Context(), 80)
		if tc.fits {
			require.NoError(t, err, tc.name)
			assert.Equal(t, 80, got)
		} else {
			require.ErrorIs(t, err, plg.ErrCall, tc.name)
		}
	}

	assert.Contains(t, logs.String(), `"msg":"plg: plugin memory limit capped at the Register limit","key":"raised/k"`)
}

func TestMemoryBudget(t *testing.T) {
	t.Parallel()

	const budget = 3 * 128 * mib

	ctr := newContainer(t, plg.WithMemoryBudget(budget))
	register := func(name, plugin string, opts ...plg.RegisterOption) error {
		opts = append(opts, plg.WithStorage(goStorage(t, name, map[string]string{"k": plugin})))

		return plg.Register[testcontract.Fooer](t.Context(), ctr, name, opts...)
	}

	require.NoError(t, register("a", "fooer", plg.WithPool(1, 2)), "2 × 128 MiB fit")

	err := register("b", "fooer", plg.WithPool(1, 2))
	require.ErrorIs(t, err, plg.ErrMemoryBudget, "another 2 × 128 MiB do not")
	require.ErrorContains(t, err, "268435456 of the 402653184-byte budget are reserved")

	_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, "b/k")
	require.ErrorIs(t, err, plg.ErrNotFound, "nothing is published")

	require.ErrorIs(t, register("c", "createfails", plg.WithPool(1, 1)), plg.ErrLoad)
	require.NoError(t, register("b", "fooer", plg.WithPool(1, 1)),
		"a refused or failed Register gives its reservation back: the last 128 MiB fit exactly")
	require.ErrorIs(t, register("d", "fooer", plg.WithPool(1, 1), plg.WithMemoryLimit(64<<10)),
		plg.ErrMemoryBudget, "even one page is over a full budget")

	// Under load, pool "a" grows to its maximum, and the instances alive never exceed the budget.
	fooer := getFooer(t, ctr, "a/k")
	errs := make(chan error, 16)

	var wg sync.WaitGroup

	for worker := range 16 {
		wg.Go(func() {
			for call := range 20 {
				err := checkedCall(t.Context(), fooer, fmt.Sprintf("w%d-c%d", worker, call), 0)
				if err == nil && plg.MemoryInUse(ctr) > budget {
					err = fmt.Errorf("%w: %d bytes in use", errUnexpected, plg.MemoryInUse(ctr))
				}

				if err != nil {
					errs <- err

					return
				}
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	assert.LessOrEqual(t, plg.Stats(ctr, "a/k").Size, 2)
}

func TestDiscardedInstancesAreReplacedInTheBackground(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctr := fooerContainer(t, map[string]string{"ref": "fooer"}, plg.WithPool(3, 4))
		ref := getFooer(t, ctr, "fooer/ref")

		for range 3 {
			require.ErrorIs(t, ref.Fail(t.Context(), "panic"), plg.ErrCall)
		}

		synctest.Wait() // until the refiller is idle again

		assert.Equal(t, plg.PoolStats{Size: 3, Idle: 3, Refills: 3, Grows: 0}, plg.Stats(ctr, "fooer/ref"),
			"the refiller started the three replacements; no call had to")
	})
}

func TestRegistersWithTheSameMemoryLimitShareARuntime(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)

	for i, opts := range [][]plg.RegisterOption{
		nil, nil, {plg.WithMemoryLimit(96 * mib)}, {plg.WithMemoryLimit(96*mib + 1000)},
	} {
		registerFooers(t, ctr, "fooer"+strconv.Itoa(i), map[string]string{"k": "fooer"}, opts...)
	}

	assert.Equal(t, 2, strings.Count(logs.String(), `"msg":"plg: guest compiled"`),
		"the guest is compiled for the 128 MiB and the 96 MiB runtimes only")
}

// openClosingStorage closes its container when a plugin is opened, after Register listed it.
type openClosingStorage struct {
	*plg.MemoryStorage

	ctr *plg.Container
}

func (s openClosingStorage) Open(ctx context.Context, desc plg.Descriptor) (fs.FS, error) {
	err := s.ctr.Close(ctx)
	if err != nil {
		return nil, fmt.Errorf("closing: %w", err)
	}

	files, err := s.MemoryStorage.Open(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	return files, nil
}

func TestRegisterRacingCloseCreatesNoRuntime(t *testing.T) {
	t.Parallel()

	ctr := plg.New()
	store := openClosingStorage{MemoryStorage: goStorage(t, "closing", map[string]string{"k": "fooer"}), ctr: ctr}

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer", plg.WithStorage(store))
	require.ErrorIs(t, err, plg.ErrClosed)
	require.ErrorIs(t, err, plg.ErrLoad)
}

// BenchmarkCallGoParallel calls a Go plugin from GOMAXPROCS goroutines, with every protection on: with a pool
// of one instance the calls contend for it, with one instance per goroutine they run in parallel.
func BenchmarkCallGoParallel(b *testing.B) {
	benchCallParallel(b, golang())
}

func benchCallParallel(b *testing.B, lang contractLanguage) {
	b.Helper()

	for _, size := range []int{1, runtime.GOMAXPROCS(0)} {
		b.Run("pool="+strconv.Itoa(size), func(b *testing.B) {
			ctr := fooerContainerIn(b, lang, map[string]string{"ref": "fooer"}, plg.WithPool(size, size))
			ref := getFooer(b, ctr, "fooer/ref")
			ctx := b.Context()

			b.ReportAllocs()
			b.ResetTimer() // after starting the instances
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_, err := ref.Foo(ctx, "bench")
					if err != nil {
						b.Error(err)

						return
					}
				}
			})
		})
	}
}
