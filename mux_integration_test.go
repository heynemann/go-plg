//go:build integration

package plg_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/httpcap"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// scriptLanguages are the languages whose instances serve several calls at once.
func scriptLanguages() []contractLanguage {
	return []contractLanguage{luaLanguage(), typescript()}
}

// eachScript runs test as a parallel subtest per script language, through the generated binder (the contract
// suite holds the generic binder to the same behavior).
func eachScript(t *testing.T, test func(t *testing.T, lang contractLanguage)) {
	t.Helper()

	for _, lang := range scriptLanguages() {
		t.Run(lang.name(), func(t *testing.T) {
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

// holdingLogger holds every "hold" line a plugin prints until release is closed (or safety ends), so a step
// printing it runs on its instance until then; held receives a value as each one arrives.
type holdingLogger struct {
	held    chan struct{}
	release chan struct{}
	safety  <-chan struct{}
}

func (*holdingLogger) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (h *holdingLogger) Handle(_ context.Context, rec slog.Record) error {
	rec.Attrs(func(attr slog.Attr) bool {
		if attr.Key != "line" || attr.Value.String() != "hold" {
			return true
		}

		h.held <- struct{}{}

		select {
		case <-h.release:
		case <-h.safety:
		}

		return false
	})

	return nil
}

func (h *holdingLogger) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *holdingLogger) WithGroup(string) slog.Handler { return h }

// TestAMultiplexedCallReturnsAtItsDeadlineWhileAnotherCallRuns: a call waiting for its request returns at its
// deadline even while another call's step runs on the instance (held printing); that call and the instance serve
// on, and the abandoned call's late answer reaches nobody.
func TestAMultiplexedCallReturnsAtItsDeadlineWhileAnotherCallRuns(t *testing.T) {
	t.Parallel()

	eachScript(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		// The step is held until the test releases it or a safety deadline ends: a call that cannot return before
		// that step ends then fails the test instead of hanging it.
		safety, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()

		logs := &holdingLogger{held: make(chan struct{}, 1), release: make(chan struct{}), safety: safety.Done()}
		srv := newGateServer(t)
		ctr := newContainer(t, plg.WithLogger(slog.New(logs)))
		plug := muxFetcher(t, lang, ctr, "busy", srv.Server, 4)

		ctx := make(expiring)
		late := callLater(ctx, plug, "GET "+srv.URL)
		<-srv.started

		busy := callLater(t.Context(), plug, "PRINT hold")

		<-logs.held // its step runs, held printing

		close(ctx) // the first call's deadline passes while the other call's step runs

		got := <-late

		require.NoError(t, safety.Err(), "the call returned while the other call's step ran")
		require.ErrorIs(t, got.err, context.DeadlineExceeded)
		require.ErrorContains(t, got.err, "abandoned while waiting for its HTTP requests")
		assert.NotErrorAs(t, got.err, new(*plg.PanicError), "the instance did not fail")
		<-srv.abandoned // its request ended with it

		close(logs.release)

		got = <-busy
		require.NoError(t, got.err, "the other call goes on")
		assert.Equal(t, "printed", got.out)

		close(srv.release)

		got = <-callLater(t.Context(), plug, "GET "+srv.URL)
		require.NoError(t, got.err)
		assert.Equal(t, "200||ok", got.out)
		assert.Equal(t, plg.PoolStats{Size: 1, Idle: 4}, plg.Stats(ctr, "busy/fetch"), "the same instance")
		assert.Empty(t, ctr.RecentPanics(), "the late answer reached nobody")
	})
}

// refusalCounter counts the warnings logging a request refused past MaxRequestsPerCall.
type refusalCounter struct {
	n atomic.Int64
}

func (*refusalCounter) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (c *refusalCounter) Handle(_ context.Context, rec slog.Record) error {
	if rec.Level != slog.LevelWarn || rec.Message != "plg: http request refused or failed" {
		return nil
	}

	rec.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "error" && strings.Contains(attr.Value.String(), httpcap.ErrTooManyRequests.Error()) {
			c.n.Add(1)
		}

		return true
	})

	return nil
}

func (c *refusalCounter) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *refusalCounter) WithGroup(string) slog.Handler { return c }

// TestAnAbandonedCallRetryingForeverIsDiscardedAfterItsGrace: a call abandoned at its deadline that retries its
// failed request forever, each retry refused at once (past MaxRequestsPerCall), runs on within one grace in all,
// not one per step: then its instance is discarded, the other call on it failing with ErrInstanceLost, and its
// refused requests (a warning and a metric each) stop with it.
func TestAnAbandonedCallRetryingForeverIsDiscardedAfterItsGrace(t *testing.T) {
	t.Parallel()

	eachScript(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		// Without a total grace the abandoned call retries forever and the other call waits for its request
		// until a safety deadline ends, failing the test instead of hanging it.
		safety, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()

		logs, reader, srv := &refusalCounter{}, newMetricsReader(t), newGateServer(t)
		ctr := newContainer(t, plg.WithLogger(slog.New(logs)), plg.WithMeterProvider(reader.provider))
		policy := openPolicy(hostOf(t, srv.URL))
		policy.MaxRequestsPerCall = 1 // the call's retries are refused at once
		plug := fetcherIn(t, lang, ctr, "poll", plg.WithPool(1, 1), plg.WithConcurrentCalls(2), plg.WithHTTP(policy))
		refusedRequests := func() int64 {
			return reader.collect(t).int64Value(t, "plg.http.requests", attribute.String("key", "poll/fetch"),
				attribute.String("outcome", "refused"))
		}

		ctx := make(expiring)
		late := callLater(ctx, plug, "POLL "+srv.URL)
		<-srv.started

		other := callLater(safety, plug, "GET "+srv.URL)
		<-srv.started

		abandoned := time.Now()

		close(ctx) // the first call's deadline passes while it waits for its request
		require.ErrorIs(t, (<-late).err, context.DeadlineExceeded)

		got := <-other
		elapsed := time.Since(abandoned)

		require.ErrorIs(t, got.err, plg.ErrInstanceLost, "the abandoned call's grace spent, its instance is discarded")
		require.ErrorContains(t, got.err, "another call on its instance failed")
		assert.GreaterOrEqual(t, elapsed, plg.MuxGrace, "the abandoned call had its grace")

		panics := ctr.RecentPanics()
		require.Len(t, panics, 1, "the call nobody waits for is reported")
		require.ErrorIs(t, panics[0], context.DeadlineExceeded)

		refused := logs.n.Load()
		assert.Positive(t, refused, "the abandoned call retried")
		assert.Equal(t, refused, refusedRequests(), "a warning and a metric each")

		close(srv.release)

		// A new instance serves the next call: the pool closes the broken one first, its owner gone.
		got = <-callLater(t.Context(), plug, "GET "+srv.URL)
		require.NoError(t, got.err)
		assert.Equal(t, "200||ok", got.out)
		assert.Equal(t, refused, logs.n.Load(), "no warning after the discard")
		assert.Equal(t, refused, refusedRequests(), "no metric after the discard")
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

// The FLOOD of TestMultiplexedRefusalsAreNotCopied: floodRequests POSTs of floodBytes each.
const (
	floodRequests = 512
	floodBytes    = 512 << 10
)

// TestMultiplexedRefusalsAreNotCopied floods the host with large requests the policy refuses (HTTP disabled,
// too large, past MaxRequestsPerCall), all started at once by TypeScript, one after the other by Lua: each is
// refused with the error the blocking path gives, without ever being copied into host memory, so the host
// allocates well under half the requests' total size (256 MiB; about 40 MiB) where it used to allocate more.
//
//nolint:paralleltest // it measures the host's allocations, so no other test may allocate meanwhile
func TestMultiplexedRefusalsAreNotCopied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	tooLarge := openPolicy(hostOf(t, srv.URL))
	tooLarge.MaxRequestBytes = 64 << 10
	tooLarge.MaxRequestsPerCall = floodRequests

	tests := []struct {
		name string
		want string // the FLOOD's result, up to a request size
		opts []plg.RegisterOption
	}{
		{name: "disabled", want: "0|" + httpcap.ErrDisabled.Error()},
		{name: "too large", want: "0|" + httpcap.ErrRequestTooLarge.Error(), opts: []plg.RegisterOption{
			plg.WithHTTP(tooLarge),
		}},
		{
			name: "too many",
			want: fmt.Sprintf("%d|%s: limit %[1]d", httpcap.DefaultMaxRequestsPerCall, httpcap.ErrTooManyRequests),
			opts: []plg.RegisterOption{plg.WithHTTP(openPolicy(hostOf(t, srv.URL)))},
		},
	}

	for _, lang := range scriptLanguages() {
		for _, tc := range tests {
			t.Run(lang.name()+"/"+tc.name, func(t *testing.T) {
				ctr := httpContainer(t, io.Discard)
				flood := fmt.Sprintf("FLOOD %d %d %s", floodRequests, floodBytes, srv.URL)
				plug := fetcherIn(t, lang, ctr, "flood",
					append([]plg.RegisterOption{plg.WithPool(1, 1), plg.WithConcurrentCalls(2)}, tc.opts...)...)

				var before, after runtime.MemStats

				runtime.ReadMemStats(&before)

				out, err := plug.Foo(t.Context(), flood)

				runtime.ReadMemStats(&after)
				require.NoError(t, err)
				assert.True(t, strings.HasPrefix(out, tc.want), "%q starts with %q", out, tc.want)

				allocated := after.TotalAlloc - before.TotalAlloc
				t.Logf("%s: the host allocated %d bytes", out, allocated)
				assert.Less(t, allocated, uint64(floodRequests*floodBytes/2),
					"refused requests are never copied into host memory")

				blocking, err := fetcherIn(t, lang, ctr, "blocking", tc.opts...).Foo(t.Context(), flood)
				require.NoError(t, err)
				assert.Equal(t, blocking, out, "the refusals of the blocking path")
			})
		}
	}
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
