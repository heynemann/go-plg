//go:build integration

package plg_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	_ "github.com/heynemann/go-plg/guest/golang"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// lookalike has exactly Fooer's methods but is another interface: a plugin registered for Fooer must not be
// served as one.
type lookalike interface {
	testcontract.Fooer
}

// newContainer returns a container on the default on-disk compilation cache, so only the first test run on a
// machine compiles the Go guest cold. Its bundlings get an hour, not the 10 s default: a small plugin bundles in
// about 40 ms, but took over 10 s in a `make check` on a machine short of memory, failing a test about something
// else. So every container of these tests that may bundle TypeScript comes from it. The tests of the bundling's
// time limit set theirs (bundle_integration_test.go).
func newContainer(tb testing.TB, opts ...plg.Option) *plg.Container {
	tb.Helper()

	ctr := plg.New(opts...)
	plg.SetBundleLimits(ctr, time.Hour, plg.BundleMemoryLimit)

	tb.Cleanup(func() { assert.NoError(tb, ctr.Close(context.Background())) })

	return ctr
}

// develIssue is the warning every load in a test binary reports: the contracts are in the binary's main module,
// built from a checkout, so the plugins' pinned versions are not checked.
func develIssue() plg.Issue {
	return plg.Issue{
		Severity: plg.SeverityWarning, Code: plg.CodeContractDevel, Field: "implements",
		Message: "the service's contract module github.com/heynemann/go-plg is built from a local checkout, version " +
			"(devel): the plugin's v1.0.0 is not checked",
	}
}

// pluginFiles returns a Go test plugin's source directory.
func pluginFiles(name string) fs.FS {
	return golang().files(name)
}

// goStorage returns a memory storage holding Go test plugins (key -> testdata plugin directory).
func goStorage(tb testing.TB, name string, dirs map[string]string) *plg.MemoryStorage {
	tb.Helper()

	return golang().storage(tb, name, dirs)
}

func registerFooers(tb testing.TB, ctr *plg.Container, name string, dirs map[string]string,
	opts ...plg.RegisterOption,
) {
	tb.Helper()

	registerFooersIn(tb, golang(), ctr, name, dirs, opts...)
}

func registerFooersIn(tb testing.TB, lang contractLanguage, ctr *plg.Container, name string, dirs map[string]string,
	opts ...plg.RegisterOption,
) {
	tb.Helper()

	opts = append(opts, plg.WithStorage(lang.storage(tb, name, dirs)))
	require.NoError(tb, plg.Register[testcontract.Fooer](tb.Context(), lang.binder(ctr), name, opts...))
}

// getFooer returns the plugin with the given key.
//
//nolint:ireturn // the plugin is served as the contract interface under test
func getFooer(tb testing.TB, ctr *plg.Container, key string) testcontract.Fooer {
	tb.Helper()

	fooer, err := plg.Get[testcontract.Fooer](tb.Context(), ctr, key)
	require.NoError(tb, err)

	return fooer
}

// inspect returns what plg.Info says of the plugin with the given key.
func inspect(tb testing.TB, ctr *plg.Container, key string) plg.PluginState {
	tb.Helper()

	state, err := plg.Info(ctr, key)
	require.NoError(tb, err)

	return state
}

// poolCounts are the counts of a plugin's pool the tests follow: its instances, the calls they could take now,
// and the instances started since it warmed.
type poolCounts struct {
	Instances, IdleSlots int
	Refills, Grows       uint64
}

// countsOf returns the counts of the pool serving the plugin with the given key.
func countsOf(tb testing.TB, ctr *plg.Container, key string) poolCounts {
	tb.Helper()

	pool := inspect(tb, ctr, key).Pool

	return poolCounts{Instances: pool.Instances, IdleSlots: pool.IdleSlots, Refills: pool.Refills, Grows: pool.Grows}
}

// fooerContainer returns a container with the "fooer" name registered for the given Go plugins.
func fooerContainer(tb testing.TB, dirs map[string]string, opts ...plg.RegisterOption) *plg.Container {
	tb.Helper()

	return fooerContainerIn(tb, golang(), dirs, opts...)
}

// fooerContainerIn returns a container with the "fooer" name registered for the given plugins of lang.
func fooerContainerIn(tb testing.TB, lang contractLanguage, dirs map[string]string, opts ...plg.RegisterOption,
) *plg.Container {
	tb.Helper()

	ctr := newContainer(tb)
	registerFooersIn(tb, lang, ctr, "fooer", dirs, opts...)

	return ctr
}

func TestCallsReachThePluginThroughItsInterface(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		ctr := fooerContainerIn(t, lang, map[string]string{"ref": "fooer", "main": "fooermain"})
		ref, main := getFooer(t, ctr, "fooer/ref"), getFooer(t, ctr, "fooer/main")

		t.Run("Foo", func(t *testing.T) {
			t.Parallel()

			got, err := ref.Foo(t.Context(), "  wasm ")
			require.NoError(t, err)
			assert.Equal(t, "hello, wasm", got, "the plugin's code (Go: the contract's too) ran inside the guest")

			got, err = main.Foo(t.Context(), "")
			require.NoError(t, err)
			assert.Equal(t, "main says hello, ", got, "a second plugin, and an empty string")
		})

		t.Run("named scalar", func(t *testing.T) {
			t.Parallel()

			for _, tc := range []struct {
				in         testcontract.Celsius
				boiling    bool
				fahrenheit float64
			}{{100, true, 212}, {99.5, false, 211.1}, {-40, false, -40}, {0, false, 32}} {
				boiling, fahrenheit, err := ref.Boil(t.Context(), tc.in)
				require.NoError(t, err)
				assert.Equal(t, tc.boiling, boiling, "%v", tc.in)
				assert.InDelta(t, tc.fahrenheit, fahrenheit, 1e-9, "%v", tc.in)
			}
		})

		t.Run("every scalar kind", func(t *testing.T) {
			t.Parallel()

			b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s, err := ref.Scalars(t.Context(),
				false, maxSafeInt-1, math.MaxInt8-1, math.MinInt16, math.MinInt32, -maxSafeInt,
				0, math.MaxUint8-1, math.MaxUint16-1, math.MaxUint32-1, maxSafeInt-1, 1.5, -0.25, "ünï")
			got := []any{b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s}

			require.NoError(t, err)
			assert.Equal(t, []any{
				true, maxSafeInt, int8(math.MaxInt8), int16(math.MinInt16 + 1), int32(math.MinInt32 + 1),
				int64(-maxSafeInt + 1), uint(1), uint8(math.MaxUint8), uint16(math.MaxUint16),
				uint32(math.MaxUint32), uint64(maxSafeInt), float32(3), -0.5, "ÜNÏ",
			}, got, "each value transformed by the plugin")
		})

		t.Run("extremes round-trip", func(t *testing.T) {
			t.Parallel()

			b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s, err := main.Scalars(t.Context(),
				true, -maxSafeInt, math.MinInt8, math.MaxInt16, math.MaxInt32, maxSafeInt,
				maxSafeInt, 0, 0, 0, 0, math.MaxFloat32, math.SmallestNonzeroFloat64, "")
			got := []any{b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s}

			require.NoError(t, err)
			assert.Equal(t, []any{
				true, -maxSafeInt, int8(math.MinInt8), int16(math.MaxInt16), int32(math.MaxInt32),
				int64(maxSafeInt), uint(maxSafeInt), uint8(0), uint16(0), uint32(0), uint64(0),
				float32(math.MaxFloat32), math.SmallestNonzeroFloat64, "",
			}, got, "each value back unchanged")
		})

		t.Run("plugin errors", func(t *testing.T) {
			t.Parallel()

			require.NoError(t, ref.Fail(t.Context(), ""))

			err := ref.Fail(t.Context(), "boom")
			require.EqualError(t, err, "boom", "the plugin's message, as an in-process implementation would return")

			var pluginErr *plg.PluginError

			require.ErrorAs(t, err, &pluginErr)
			assert.Equal(t, plg.PluginError{Key: "fooer/ref", Method: "Fail", Message: "boom"}, *pluginErr)
			require.NotErrorIs(t, err, plg.ErrCall, "an ordinary error keeps the instance")
		})
	})
}

func TestInstanceStateLivesAcrossCalls(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		ctr := fooerContainerIn(t, lang, map[string]string{"ref": "fooer"})
		ref := getFooer(t, ctr, "fooer/ref")
		served := instanceID(t, ctr, "fooer/ref")

		for want := 1; want <= 3; want++ {
			got, err := ref.Count(t.Context())
			require.NoError(t, err)
			assert.Equal(t, want, got)
		}

		require.Error(t, ref.Fail(t.Context(), "an ordinary error"))
		assert.Equal(t, served, instanceID(t, ctr, "fooer/ref"), "a plugin error keeps the instance")

		got, err := ref.Count(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 4, got, "and its state")
	})
}

func TestPluginPanicDiscardsTheInstance(t *testing.T) {
	t.Parallel()

	ctr := fooerContainer(t, map[string]string{"ref": "fooer"})
	ref := getFooer(t, ctr, "fooer/ref")
	served := instanceID(t, ctr, "fooer/ref")

	n, err := ref.Count(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, n)

	err = ref.Fail(t.Context(), "panic")
	require.ErrorIs(t, err, plg.ErrCall)
	require.ErrorContains(t, err, "plugin panicked on purpose")
	assert.NotEqual(t, served, instanceID(t, ctr, "fooer/ref"))

	n, err = ref.Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the next call runs on a fresh instance, with a fresh Create")
}

func TestDeadlineStopsAnInfiniteLoopAndDiscardsTheInstance(t *testing.T) {
	t.Parallel()

	ctr := fooerContainer(t, map[string]string{"hog": "hog"}, plg.WithPool(1, 1))
	hog := getFooer(t, ctr, "fooer/hog")
	served := instanceID(t, ctr, "fooer/hog")

	n, err := hog.Count(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, n)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err = hog.Fail(ctx, "loop") // for {}
	require.ErrorIs(t, err, plg.ErrCall)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the guest was stopped preemptively")

	n, err = hog.Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the next call succeeds, on a fresh instance")
	assert.NotEqual(t, served, instanceID(t, ctr, "fooer/hog"))
}

func TestCallContext(t *testing.T) {
	t.Parallel()

	ref := getFooer(t, fooerContainer(t, map[string]string{"ref": "fooer"}), "fooer/ref")

	var nilCtx context.Context

	_, err := ref.Foo(nilCtx, "x")
	require.ErrorContains(t, err, "nil context")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = ref.Foo(ctx, "x")
	require.ErrorIs(t, err, context.Canceled)

	n, err := ref.Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "rejected calls never reached the instance")
}

func TestCallsWaitOnTheirContextWhileThePoolIsFull(t *testing.T) {
	t.Parallel()

	ctr := fooerContainer(t, map[string]string{"ref": "fooer"}, plg.WithPool(1, 1))
	ref := getFooer(t, ctr, "fooer/ref")

	release, err := plg.Hold(t.Context(), ctr, "fooer/ref")
	require.NoError(t, err)

	ctx, cancel := context.WithCancelCause(t.Context())
	done := make(chan error)

	go func() {
		_, err := ref.Foo(ctx, "x")
		done <- err
	}()

	cancel(context.DeadlineExceeded)
	require.ErrorIs(t, <-done, context.DeadlineExceeded, "a call to a busy instance honours its context")

	go func() {
		_, err := ref.Foo(t.Context(), "x")
		done <- err
	}()

	release()
	require.NoError(t, <-done, "a waiting call proceeds once the instance is free")
}

func TestRegisterFailsFastAndPublishesNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		plugin string
		msg    string
	}{
		{"createfails", "database unreachable"},
		{"createpanics", "Create panicked on purpose"},
		{"nocreate", "func Create()"},
		{"badcreate", "Create has the wrong type"},
		{"syntaxerror", "plugin.go"},
		{"wronginterface", "half"},
		{"nilcreate", "Create returned a nil testcontract.Fooer"},
		{"forbidden", `"os"`},
	}

	for _, guest := range goGuests(t) {
		t.Run(guest.name, func(t *testing.T) {
			t.Parallel()

			ctr := newContainer(t)

			for _, tc := range tests {
				t.Run(tc.plugin, func(t *testing.T) {
					t.Parallel()

					// "a-good" loads first, then the broken plugin fails the whole registration.
					store := goStorage(t, "store-"+tc.plugin, map[string]string{"a-good": "fooer", "b-bad": tc.plugin})
					opts := append([]plg.RegisterOption{plg.WithStorage(store)}, guest.opts...)
					err := plg.Register[testcontract.Fooer](t.Context(), ctr, tc.plugin, opts...)
					require.ErrorIs(t, err, plg.ErrLoad)
					require.ErrorContains(t, err, tc.plugin+"/b-bad")
					require.ErrorContains(t, err, "store-"+tc.plugin)
					require.ErrorContains(t, err, tc.msg)

					_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, tc.plugin+"/a-good")
					require.ErrorIs(t, err, plg.ErrNotFound, "nothing is published")

					store.Delete("b-bad")
					require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, tc.plugin, opts...),
						"a failed Register leaves its name free")

					got, err := getFooer(t, ctr, tc.plugin+"/a-good").Foo(t.Context(), "again")
					require.NoError(t, err)
					assert.Equal(t, "hello, again", got)
				})
			}
		})
	}
}

func TestEntrypoints(t *testing.T) {
	t.Parallel()

	src, err := fs.ReadFile(pluginFiles("fooer"), "plugin.go")
	require.NoError(t, err)

	files := fstest.MapFS{
		"pkg/plugin.go": {Data: src},
		"README":        {Data: []byte("not Go")},
	}

	tests := []struct {
		entrypoint string
		err        string
	}{
		{entrypoint: "pkg"},
		{entrypoint: "pkg/plugin.go"},
		{entrypoint: "pkg/missing.go", err: "file does not exist"},
		{entrypoint: "../pkg", err: "entrypoint is not a valid path"},
		{entrypoint: "README", err: "neither a directory nor a .go file"},
	}

	ctr := newContainer(t)

	for i, tc := range tests {
		t.Run(tc.entrypoint, func(t *testing.T) {
			t.Parallel()

			store := plg.NewMemoryStorage("mem")
			desc := plg.Descriptor{
				Key: "k", Implements: implementsOf[testcontract.Fooer](), Language: plg.LanguageGo,
				Entrypoint: tc.entrypoint,
			}
			require.NoError(t, store.Put(&desc, files))

			name := "entry" + strconv.Itoa(i)

			err := plg.Register[testcontract.Fooer](t.Context(), ctr, name, plg.WithStorage(store))
			if tc.err != "" {
				require.ErrorIs(t, err, plg.ErrLoad)
				require.ErrorContains(t, err, tc.err)

				return
			}

			require.NoError(t, err)

			got, err := getFooer(t, ctr, name+"/k").Foo(t.Context(), "dir")
			require.NoError(t, err)
			assert.Equal(t, "hello, dir", got)
		})
	}
}

func TestRegisterReportsStorageOpenFailures(t *testing.T) {
	t.Parallel()

	store := &failingStorage{MemoryStorage: goStorage(t, "flaky", map[string]string{"k": "fooer"}), failOpen: true}

	err := plg.Register[testcontract.Fooer](t.Context(), newContainer(t), "fooer", plg.WithStorage(store))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorIs(t, err, errStorage)
	require.ErrorContains(t, err, "fooer/k")
}

func TestRegisterReportsUnreadablePluginFiles(t *testing.T) {
	t.Parallel()

	src, err := fs.ReadFile(pluginFiles("fooer"), "plugin.go")
	require.NoError(t, err)

	files := unreadableFS{
		files:  fstest.MapFS{"pkg/plugin.go": {Data: src}, "pkg/secret.go": {Data: []byte("package fooer\n")}},
		broken: "pkg/secret.go",
	}

	ctr := newContainer(t)

	// With a version or without one, Register first copies the plugin's files: it hashes, checks and loads the
	// copy.
	for version, step := range map[string]string{"": "versionless", "v1": "versioned"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()

			store := listStorage{name: "s", files: files, descs: []plg.Descriptor{
				{
					Key: "k", Implements: implementsOf[testcontract.Fooer](), Language: plg.LanguageGo,
					Entrypoint: "pkg/plugin.go", Version: version,
				},
			}}

			err := plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer"+version, plg.WithStorage(store))
			require.ErrorIs(t, err, plg.ErrLoad)
			require.ErrorIs(t, err, errStorage)
			require.ErrorContains(t, err, "copying the plugin's files: reading pkg/secret.go")
		})
	}
}

func TestGetErrors(t *testing.T) {
	t.Parallel()

	ctr := fooerContainer(t, map[string]string{"ref": "fooer"})

	for _, key := range []string{"fooer/nope", "fooer", "fooer/", "", "ref", "other/ref"} {
		_, err := plg.Get[testcontract.Fooer](t.Context(), ctr, key)
		require.ErrorIs(t, err, plg.ErrNotFound, "%q", key)
	}

	_, err := plg.Get[lookalike](t.Context(), ctr, "fooer/ref")
	require.ErrorIs(t, err, plg.ErrTypeMismatch, "the adapter would satisfy lookalike, but the key is a Fooer")
	require.ErrorContains(t, err, "testcontract.Fooer")

	_, err = plg.Get[string](t.Context(), ctr, "fooer/ref")
	require.ErrorIs(t, err, plg.ErrTypeMismatch)
}

func TestAllIteratesInKeyOrderOverAFixedSnapshot(t *testing.T) {
	t.Parallel()

	ctr := fooerContainer(t, map[string]string{"c": "fooermain", "a": "fooer", "b": "fooermain"})

	var keys, greetings []string

	for key, fooer := range plg.All[testcontract.Fooer](t.Context(), ctr) {
		if len(keys) == 0 {
			// Published mid-iteration: "zz/..." sorts last, so a live iteration would reach it.
			registerFooers(t, ctr, "zz", map[string]string{"late": "fooer"})
		}

		keys = append(keys, key)
		got, err := fooer.Foo(t.Context(), key)
		require.NoError(t, err)

		greetings = append(greetings, got)
	}

	assert.Equal(t, []string{"fooer/a", "fooer/b", "fooer/c"}, keys)
	assert.Equal(t, []string{"hello, fooer/a", "main says hello, fooer/b", "main says hello, fooer/c"}, greetings,
		"each key yields its own plugin")

	keys = keys[:0]
	for key := range plg.All[testcontract.Fooer](t.Context(), ctr) {
		keys = append(keys, key)
	}

	assert.Equal(t, []string{"fooer/a", "fooer/b", "fooer/c", "zz/late"}, keys, "a new iteration sees the new plugin")

	for key := range plg.All[testcontract.Fooer](t.Context(), ctr) {
		assert.Equal(t, "fooer/a", key)

		break
	}

	for key := range plg.All[lookalike](t.Context(), ctr) {
		assert.Fail(t, "All yields only plugins registered for the interface", key)
	}
}

// TestConcurrentGetAndCalls runs Get, All, calls and Registers concurrently. With a pool of one instance,
// calls are serialized: the Count results are exactly 1..n, each seen once.
func TestConcurrentGetAndCalls(t *testing.T) {
	t.Parallel()

	const workers, rounds = 8, 10

	ctr := fooerContainer(t, map[string]string{"ref": "fooer"}, plg.WithPool(1, 1))
	counts := make(chan int, workers*rounds)
	errs := make(chan error, workers)

	var wg sync.WaitGroup

	for worker := range workers {
		wg.Go(func() { errs <- hammer(t.Context(), ctr, "extra"+strconv.Itoa(worker), rounds, counts) })
	}

	wg.Wait()
	close(errs)
	close(counts)

	for err := range errs {
		require.NoError(t, err)
	}

	got := slices.Sorted(func(yield func(int) bool) {
		for n := range counts {
			if !yield(n) {
				return
			}
		}
	})

	want := make([]int, workers*rounds)
	for i := range want {
		want[i] = i + 1
	}

	assert.Equal(t, want, got)
}

var errUnexpected = errors.New("unexpected result")

// hammer gets the "fooer/ref" plugin, counts and greets through it and iterates All, rounds times, then
// registers name, publishing a new snapshot while other goroutines read.
func hammer(ctx context.Context, ctr *plg.Container, name string, rounds int, counts chan<- int) error {
	for range rounds {
		fooer, err := plg.Get[testcontract.Fooer](ctx, ctr, "fooer/ref")
		if err != nil {
			return fmt.Errorf("get: %w", err)
		}

		n, err := fooer.Count(ctx)
		if err != nil {
			return fmt.Errorf("count: %w", err)
		}

		counts <- n

		got, err := fooer.Foo(ctx, "race")
		if err != nil {
			return fmt.Errorf("foo: %w", err)
		}

		if got != "hello, race" {
			return fmt.Errorf("%w: Foo returned %q", errUnexpected, got)
		}

		for key := range plg.All[testcontract.Fooer](ctx, ctr) {
			if key != "fooer/ref" {
				return fmt.Errorf("%w: All yielded %q", errUnexpected, key)
			}
		}
	}

	err := plg.Register[testcontract.Fooer](ctx, ctr, name, plg.WithStorage(plg.NewMemoryStorage("empty")))
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	return nil
}

func TestCloseWaitsForCallsAndRejectsLaterUse(t *testing.T) {
	t.Parallel()

	ctr := plg.New()
	registerFooers(t, ctr, "fooer", map[string]string{"ref": "fooer"})

	ref := getFooer(t, ctr, "fooer/ref")

	release, err := plg.Hold(t.Context(), ctr, "fooer/ref")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = ctr.Close(ctx)
	require.ErrorIs(t, err, context.Canceled, "Close waits on ctx for the call in flight")
	release()

	_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, "fooer/ref")
	require.ErrorIs(t, err, plg.ErrClosed)

	_, err = ref.Foo(t.Context(), "x")
	require.ErrorIs(t, err, plg.ErrClosed)

	err = plg.Register[testcontract.Fooer](t.Context(), ctr, "again", plg.WithStorage(plg.NewMemoryStorage("m")))
	require.ErrorIs(t, err, plg.ErrClosed)

	require.NoError(t, ctr.Close(t.Context()), "closing twice does nothing")
}

func TestCloseClosesIdleInstances(t *testing.T) {
	t.Parallel()

	ctr := plg.New()
	registerFooers(t, ctr, "fooer", map[string]string{"ref": "fooer", "main": "fooermain"})

	ref := getFooer(t, ctr, "fooer/ref")
	require.NoError(t, ctr.Close(t.Context()))

	_, err := ref.Count(t.Context())
	require.ErrorIs(t, err, plg.ErrClosed)
}

// TestClosedContainersKeepNoGuestMemory closes containers that stay referenced, each with a plugin taken from it,
// as a service keeping the containers it replaced (or a test keeping its cleanups) would: a closed container
// holds none of its compiled guest, so each keeps well under 256 KiB of live heap. It used to keep the decoded
// guest module: about 30 MiB for Go, 1.5 MiB for TypeScript, 0.5 MiB for Lua.
//
// Each container lives in a synctest bubble, which ends only once every goroutine started in it has exited. Every
// call into an instance starts a wazero goroutine holding the instance (it closes the instance if the call's
// context ends) that exits whenever the scheduler gets to it, after the call; one not yet run when the heap is
// measured would keep the closed instance's memory and decoded guest for that collection (a TypeScript
// container's then measured 1.13 MB).
//
//nolint:paralleltest // it measures the process heap, which no other test may move meanwhile
func TestClosedContainersKeepNoGuestMemory(t *testing.T) {
	const containers, bound = 4, 256 << 10

	for _, lang := range contractLanguages() {
		t.Run(string(lang.lang), func(t *testing.T) {
			closeContainers := func(n int) []any {
				kept := make([]any, 0, 2*n)

				for range n {
					synctest.Test(t, func(t *testing.T) {
						ctr := newContainer(t)
						registerFooersIn(t, lang, ctr, "fooer", map[string]string{"ref": "fooer"}, plg.WithPool(1, 1))

						ref := getFooer(t, ctr, "fooer/ref")
						got, err := ref.Foo(t.Context(), "x")
						require.NoError(t, err)
						require.Equal(t, "hello, x", got)
						require.NoError(t, ctr.Close(t.Context()))

						kept = append(kept, ctr, ref)
					})
				}

				return kept
			}

			warm := closeContainers(1) // the process's one-time costs, like inflating the Go guest
			before := heapInUse()
			kept := closeContainers(containers)
			each := (heapInUse() - before) / containers

			runtime.KeepAlive(warm)
			runtime.KeepAlive(kept)
			t.Logf("each closed %s container keeps %d bytes", lang.lang, each)
			assert.Less(t, each, int64(bound), "a closed container keeps no compiled guest")
		})
	}
}

// TestGetAndAllDoNotAllocate is the allocation budget of Get and All (`make allocs`, part of `make check`).
// Coverage instrumentation adds a counter update to every block, which keeps All from inlining and so makes
// the loop body escape: allocations are only meaningful in an uninstrumented build (the race detector is fine).
//
//nolint:paralleltest // testing.AllocsPerRun panics in parallel tests
func TestGetAndAllDoNotAllocate(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("allocations are measured without coverage instrumentation, by make allocs")
	}

	ctr := fooerContainer(t, map[string]string{"ref": "fooer", "main": "fooermain"})
	ctx := t.Context()

	var failures int

	allocs := testing.AllocsPerRun(100, func() {
		fooer, err := plg.Get[testcontract.Fooer](ctx, ctr, "fooer/ref")
		if err != nil || fooer == nil {
			failures++
		}
	})
	assert.Zero(t, allocs, "Get")

	seen := 0
	allocs = testing.AllocsPerRun(100, func() {
		for key, fooer := range plg.All[testcontract.Fooer](ctx, ctr) {
			if key == "" || fooer == nil {
				failures++
			}

			seen++
		}
	})
	assert.Zero(t, allocs, "All")
	assert.Equal(t, 2*101, seen, "AllocsPerRun runs once more to warm up")
	assert.Zero(t, failures)
}

func benchContainer(b *testing.B) *plg.Container {
	b.Helper()

	return fooerContainer(b, map[string]string{"ref": "fooer", "main": "fooermain"})
}

func BenchmarkGet(b *testing.B) {
	ctr := benchContainer(b)
	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		_, err := plg.Get[testcontract.Fooer](ctx, ctr, "fooer/ref")
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetParallel(b *testing.B) {
	ctr := benchContainer(b)
	ctx := b.Context()

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := plg.Get[testcontract.Fooer](ctx, ctr, "fooer/ref")
			if err != nil {
				b.Error(err)

				return
			}
		}
	})
}

func BenchmarkAll(b *testing.B) {
	ctr := benchContainer(b)
	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		n := 0
		for range plg.All[testcontract.Fooer](ctx, ctr) {
			n++
		}

		if n != 2 {
			b.Fatal(n)
		}
	}
}

func BenchmarkAllParallel(b *testing.B) {
	ctr := benchContainer(b)
	ctx := b.Context()

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := 0
			for range plg.All[testcontract.Fooer](ctx, ctr) {
				n++
			}

			if n != 2 {
				b.Error(n)

				return
			}
		}
	})
}

// BenchmarkCallGo is one call into a Go plugin, with every protection on: the memory limit, the preemptive
// stop on ctx and the guest's recover boundary.
func BenchmarkCallGo(b *testing.B) {
	benchCall(b, golang())
}
