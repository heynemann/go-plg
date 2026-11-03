//go:build integration

package plg_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/httpcap"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// scriptLanguages are the languages whose instances serve several calls at once.
func scriptLanguages() []contractLanguage {
	return []contractLanguage{luaLanguage(), typescript()}
}

// eachScript runs test as a parallel subtest per script language.
func eachScript(t *testing.T, test func(t *testing.T, lang contractLanguage)) {
	t.Helper()

	for _, lang := range scriptLanguages() {
		t.Run(string(lang.lang), func(t *testing.T) {
			t.Parallel()

			test(t, lang)
		})
	}
}

// barrierServer holds every request until n requests have arrived, then answers each with its id query
// parameter (and later requests at once): a plugin instance serving one call at a time can never get there.
func barrierServer(t *testing.T, n int64) *httptest.Server {
	t.Helper()

	var arrived atomic.Int64

	all := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if arrived.Add(1) == n {
			close(all)
		}

		select {
		case <-all:
			//nolint:gosec // G705: a test server echoing its own test's request
			_, err := io.WriteString(writer, req.URL.Query().Get("id"))
			assert.NoError(t, err)
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

// gateServer answers "ok" to every request once release is closed; started receives a value as each request
// arrives, abandoned as each one the client gives up on ends.
type gateServer struct {
	*httptest.Server

	started   chan struct{}
	abandoned chan struct{}
	release   chan struct{}
}

func newGateServer(t *testing.T) *gateServer {
	t.Helper()

	srv := &gateServer{
		started: make(chan struct{}, 16), abandoned: make(chan struct{}, 16), release: make(chan struct{}),
	}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.started <- struct{}{}

		select {
		case <-srv.release:
			_, err := io.WriteString(w, "ok")
			assert.NoError(t, err)
		case <-r.Context().Done():
			srv.abandoned <- struct{}{}
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

// muxFetcher registers lang's fetcher as name/fetch in ctr with one instance serving up to calls at once,
// allowed to reach srv.
//
//nolint:ireturn // the plugin is served as the contract interface under test
func muxFetcher(t *testing.T, lang contractLanguage, ctr *plg.Container, name string, srv *httptest.Server,
	calls int, opts ...plg.RegisterOption,
) testcontract.Fooer {
	t.Helper()

	opts = append(opts, plg.WithPool(1, 1), plg.WithConcurrentCalls(calls),
		plg.WithHTTP(openPolicy(hostOf(t, srv.URL))))

	return fetcherIn(t, lang, ctr, name, opts...)
}

// callResult is what a plugin call started in the background returned.
type callResult struct {
	err error
	out string
}

// callLater calls plug.Foo(ctx, cmd) in the background.
func callLater(ctx context.Context, plug testcontract.Fooer, cmd string) <-chan callResult {
	done := make(chan callResult, 1)

	go func() {
		out, err := plug.Foo(ctx, cmd)
		done <- callResult{out: out, err: err}
	}()

	return done
}

func TestConcurrentCallsShareOneInstance(t *testing.T) {
	t.Parallel()

	eachScript(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		const calls = 8

		srv := barrierServer(t, calls)
		ctr := newContainer(t)
		plug := muxFetcher(t, lang, ctr, "mux", srv, calls)

		// The barrier opens only once every call's request is in flight: a safety deadline turns a regression
		// into a failure instead of a hang.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()

		results := make([]<-chan callResult, calls)
		for i := range results {
			results[i] = callLater(ctx, plug, fmt.Sprintf("GET %s/?id=call-%d", srv.URL, i))
		}

		for i, result := range results {
			got := <-result
			require.NoError(t, got.err)
			assert.Equal(t, fmt.Sprintf("200||call-%d", i), got.out, "each call gets its own answer")
		}

		stats := plg.Stats(ctr, "mux/fetch")
		assert.Equal(t, plg.PoolStats{Size: 1, Idle: calls}, stats, "one instance served them all")
		assert.Less(t, plg.InstanceMemory(ctr, "mux/fetch"), uint64(8<<20), "the memory of one instance")
	})
}

func TestConcurrentCallsKeepTheirOwnDeadlines(t *testing.T) {
	t.Parallel()

	eachScript(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		srv := newGateServer(t)
		ctr := newContainer(t)
		plug := muxFetcher(t, lang, ctr, "deadline", srv.Server, 4)

		ctx := make(expiring)
		late := callLater(ctx, plug, "GET "+srv.URL)
		<-srv.started

		onTime := callLater(t.Context(), plug, "GET "+srv.URL)
		<-srv.started

		close(ctx) // the first call's deadline passes while it waits for its request

		got := <-late
		require.ErrorIs(t, got.err, context.DeadlineExceeded)
		require.ErrorContains(t, got.err, "abandoned while waiting for its HTTP requests")
		assert.NotErrorAs(t, got.err, new(*plg.PanicError), "the instance did not fail")
		<-srv.abandoned // its request ended with it

		close(srv.release)

		got = <-onTime
		require.NoError(t, got.err, "the other call on the instance goes on")
		assert.Equal(t, "200||ok", got.out)

		got = <-callLater(t.Context(), plug, "GET "+srv.URL)
		require.NoError(t, got.err)
		assert.Equal(t, plg.PoolStats{Size: 1, Idle: 4}, plg.Stats(ctr, "deadline/fetch"), "the same instance")
	})
}

func TestAFailingCallDiscardsItsInstanceWithItsCalls(t *testing.T) {
	t.Parallel()

	failures := []struct {
		want    error
		name    string
		cmd     string
		timeout time.Duration
	}{
		{
			name:    "stopped at its deadline",
			cmd:     "LOOP -",
			timeout: 100 * time.Millisecond,
			want:    context.DeadlineExceeded,
		},
		{name: "out of memory", cmd: "HOG -"},
	}

	eachScript(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		for _, failure := range failures {
			t.Run(failure.name, func(t *testing.T) {
				t.Parallel()

				srv := newGateServer(t)
				ctr := newContainer(t)
				limit := plg.WithMemoryLimit(16 << 20)
				broken := muxFetcher(t, lang, ctr, "broken", srv.Server, 4, limit)
				other := muxFetcher(t, lang, ctr, "other", srv.Server, 4, limit)

				lost := []<-chan callResult{
					callLater(t.Context(), broken, "GET "+srv.URL),
					callLater(t.Context(), broken, "GET "+srv.URL),
				}
				kept := []<-chan callResult{
					callLater(t.Context(), other, "GET "+srv.URL),
					callLater(t.Context(), other, "GET "+srv.URL),
				}

				for range 4 {
					<-srv.started
				}

				ctx := t.Context()

				if failure.timeout > 0 {
					var cancel context.CancelFunc

					ctx, cancel = context.WithTimeout(ctx, failure.timeout)
					defer cancel()
				}

				_, err := broken.Foo(ctx, failure.cmd)
				culprit, ok := errors.AsType[*plg.PanicError](err)
				require.True(t, ok, "the failing call fails as it would alone: %v", err)
				assert.Equal(t, plg.PhaseCall, culprit.Phase)

				if failure.want != nil {
					require.ErrorIs(t, err, failure.want)
				}

				for _, result := range lost {
					got := <-result
					require.ErrorIs(t, got.err, plg.ErrInstanceLost, "the calls on its instance fail with it")
					require.ErrorContains(t, got.err, "another call on its instance failed")
					assert.NotErrorAs(t, got.err, new(*plg.PanicError), "reported once, by the failing call")
					<-srv.abandoned
				}

				close(srv.release)

				for _, result := range kept {
					got := <-result
					require.NoError(t, got.err, "calls on other instances are unaffected")
					assert.Equal(t, "200||ok", got.out)
				}

				out, err := broken.Foo(t.Context(), "GET "+srv.URL)
				require.NoError(t, err, "a new instance serves the next call")
				assert.Equal(t, "200||ok", out)
				assert.Len(t, ctr.RecentPanics(), 1)
			})
		}
	})
}

func TestConcurrentCallsCountTheirOwnRequests(t *testing.T) {
	t.Parallel()

	eachScript(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		srv := barrierServer(t, 2)
		policy := openPolicy(hostOf(t, srv.URL))
		policy.MaxRequestsPerCall = 3
		plug := fetcherIn(t, lang, newContainer(t), "count", plg.WithPool(1, 1), plg.WithConcurrentCalls(2),
			plg.WithHTTP(policy))

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute) // as in TestConcurrentCallsShareOneInstance
		defer cancel()

		// Both calls' first requests meet at the barrier, so the six requests overlap on one instance.
		first, second := callLater(
			ctx,
			plug,
			"REPEAT 3 "+srv.URL+"/?id=a",
		), callLater(
			ctx,
			plug,
			"REPEAT 3 "+srv.URL+"/?id=b",
		)

		for _, got := range []callResult{<-first, <-second} {
			require.NoError(t, got.err, "each call has its own MaxRequestsPerCall")
		}

		_, err := plug.Foo(t.Context(), "REPEAT 4 "+srv.URL)
		require.ErrorContains(t, err, httpcap.ErrTooManyRequests.Error())
	})
}

func TestTypeScriptCallAwaitingNothingFails(t *testing.T) {
	t.Parallel()

	srv := newGateServer(t)
	ctr := newContainer(t)
	plug := muxFetcher(t, typescript(), ctr, "hang", srv.Server, 4)

	waiting := callLater(t.Context(), plug, "GET "+srv.URL)
	<-srv.started

	hung := callLater(t.Context(), plug, "HANG -")

	close(srv.release)
	require.NoError(t, (<-waiting).err)

	got := <-hung

	var failed *plg.PluginError

	require.ErrorAs(t, got.err, &failed)
	require.ErrorContains(t, got.err, "the returned promise never settles",
		"once no request is left that could settle it")
	assert.Equal(t, plg.PoolStats{Size: 1, Idle: 4}, plg.Stats(ctr, "hang/fetch"), "the instance serves on")
}

func TestLuaCallsYieldOnlyForTheirRequests(t *testing.T) {
	t.Parallel()

	srv := barrierServer(t, 1)
	plug := muxFetcher(t, luaLanguage(), newContainer(t), "yield", srv, 4)

	out, err := plug.Foo(t.Context(), "SORTGET "+srv.URL+"/?id=sorted")
	require.NoError(t, err, "where the call cannot yield, the request blocks the instance")
	assert.Equal(t, "200||sorted", out)

	_, err = plug.Foo(t.Context(), "YIELD -")

	var failed *plg.PluginError

	require.ErrorAs(t, err, &failed)
	require.ErrorContains(t, err, "attempt to yield from outside a coroutine", "as without WithConcurrentCalls")
}

func TestGoPluginsServeOneCallPerInstance(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	opts := []plg.RegisterOption{plg.WithPool(1, 1), plg.WithConcurrentCalls(8)}
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "go",
		append(opts, plg.WithStorage(golang().storage(t, "go", map[string]string{"ref": "fooer"})))...))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "lua",
		append(opts, plg.WithStorage(luaLanguage().storage(t, "lua", map[string]string{"ref": "fooer"})))...))

	assert.Equal(t, plg.PoolStats{Size: 1, Idle: 1}, plg.Stats(ctr, "go/ref"))
	assert.Equal(t, plg.PoolStats{Size: 1, Idle: 8}, plg.Stats(ctr, "lua/ref"))
}

// The I/O-bound workload of BenchmarkHTTPLatency: latencyCallers callers each make one plugin call at a time,
// and every call makes one request to a server answering after latencyDelay.
const (
	latencyDelay   = 100 * time.Millisecond
	latencyCallers = 256
)

// latencyServer answers every request with "ok" after delay (or gives up when the client does).
func latencyServer(tb testing.TB, delay time.Duration) *httptest.Server {
	tb.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
			_, err := io.WriteString(w, "ok")
			assert.NoError(tb, err)
		case <-r.Context().Done():
		}
	}))
	tb.Cleanup(srv.Close)

	return srv
}

// BenchmarkHTTPLatency measures an I/O-bound plugin: latencyCallers concurrent callers, each call awaiting one
// request to a server answering after latencyDelay. It reports the throughput (calls/s; ns/op is its inverse),
// the peak growth of the live Go heap while serving (live-MiB: instances, their linear memory and everything in
// flight) and the instances' linear memory (wasm-MiB), blocking (one call per instance) with pools of several
// sizes, and multiplexed (WithConcurrentCalls) with as many calls per instance as it takes for latencyCallers.
func BenchmarkHTTPLatency(b *testing.B) {
	for _, lang := range []contractLanguage{luaLanguage(), typescript()} {
		for _, size := range []int{1, 16, 64, latencyCallers} {
			b.Run(string(lang.lang)+"/blocking/pool="+strconv.Itoa(size), func(b *testing.B) {
				benchLatency(b, lang, plg.WithPool(size, size))
			})
		}

		for _, size := range []int{1, 4, 16} {
			calls := latencyCallers / size
			name := string(lang.lang) + "/multiplexed/pool=" + strconv.Itoa(size) + "/calls=" + strconv.Itoa(calls)

			b.Run(name, func(b *testing.B) {
				benchLatency(b, lang, plg.WithPool(size, size), plg.WithConcurrentCalls(calls))
			})
		}
	}
}

func benchLatency(b *testing.B, lang contractLanguage, opts ...plg.RegisterOption) {
	b.Helper()

	srv := latencyServer(b, latencyDelay)
	baseline := heapInUse()

	ctr := plg.New()

	b.Cleanup(func() { assert.NoError(b, ctr.Close(context.Background())) })

	store := lang.storage(b, "web", map[string]string{"fetch": "fetcher"})
	opts = append(opts, plg.WithStorage(store), plg.WithHTTP(openPolicy(hostOf(b, srv.URL))))
	require.NoError(b, plg.Register[testcontract.Fooer](b.Context(), ctr, "web", opts...))

	plug := getFooer(b, ctr, "web/fetch")
	cmd := "GET " + srv.URL

	peak := sampleHeap(b)

	b.ReportAllocs()
	b.ResetTimer()

	var (
		left    atomic.Int64
		callers sync.WaitGroup
	)

	left.Store(int64(b.N))

	start := time.Now()

	for range latencyCallers {
		callers.Go(func() {
			for left.Add(-1) >= 0 {
				_, err := plug.Foo(b.Context(), cmd)
				if err != nil {
					b.Error(err)

					return
				}
			}
		})
	}

	callers.Wait()

	elapsed := time.Since(start)

	b.StopTimer()

	const mib = 1 << 20

	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "calls/s")
	b.ReportMetric(float64(peak()-baseline)/mib, "live-MiB")
	b.ReportMetric(float64(plg.InstanceMemory(ctr, "web/fetch"))/mib, "wasm-MiB")
}

// sampleHeap measures the live heap (after a collection) every 50 ms until the benchmark ends; the returned
// function stops sampling and returns the largest sample.
func sampleHeap(b *testing.B) func() int64 {
	b.Helper()

	var (
		most int64
		done = make(chan struct{})
		quit = make(chan struct{})
	)

	go func() {
		defer close(done)

		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			most = max(most, heapInUse())

			select {
			case <-ticker.C:
			case <-quit:
				return
			}
		}
	}()

	stop := sync.OnceValue(func() int64 {
		close(quit)
		<-done

		return most
	})

	b.Cleanup(func() { stop() })

	return stop
}

// BenchmarkCallLuaMultiplexed is BenchmarkCallLua on an instance serving several calls at once
// (WithConcurrentCalls): each call is handed to the instance's owner goroutine and its answer handed back.
func BenchmarkCallLuaMultiplexed(b *testing.B) {
	benchCallMultiplexed(b, luaLanguage())
}

// BenchmarkCallLuaMultiplexedParallel calls one such instance from GOMAXPROCS goroutines.
func BenchmarkCallLuaMultiplexedParallel(b *testing.B) {
	benchCallMultiplexedParallel(b, luaLanguage())
}

// BenchmarkCallTypeScriptMultiplexed is BenchmarkCallTypeScript on an instance serving several calls at once.
func BenchmarkCallTypeScriptMultiplexed(b *testing.B) {
	benchCallMultiplexed(b, typescript())
}

// BenchmarkCallTypeScriptMultiplexedParallel calls one such instance from GOMAXPROCS goroutines.
func BenchmarkCallTypeScriptMultiplexedParallel(b *testing.B) {
	benchCallMultiplexedParallel(b, typescript())
}

// multiplexedFooer returns lang's fooer plugin on one instance serving up to GOMAXPROCS calls at once.
//
//nolint:ireturn // the plugin is served as the contract interface under test
func multiplexedFooer(b *testing.B, lang contractLanguage) testcontract.Fooer {
	b.Helper()

	ctr := fooerContainerIn(b, lang, map[string]string{"ref": "fooer"}, plg.WithPool(1, 1),
		plg.WithConcurrentCalls(runtime.GOMAXPROCS(0)))

	return getFooer(b, ctr, "fooer/ref")
}

func benchCallMultiplexed(b *testing.B, lang contractLanguage) {
	b.Helper()

	ref := multiplexedFooer(b, lang)
	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		_, err := ref.Foo(ctx, "bench")
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchCallMultiplexedParallel(b *testing.B, lang contractLanguage) {
	b.Helper()

	ref := multiplexedFooer(b, lang)
	ctx := b.Context()

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := ref.Foo(ctx, "bench")
			if err != nil {
				b.Error(err)

				return
			}
		}
	})
}
