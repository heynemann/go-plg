//go:build integration

package plg_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// metricsReader is an in-memory OpenTelemetry reader with a provider for a container.
type metricsReader struct {
	*sdkmetric.ManualReader

	provider *sdkmetric.MeterProvider
}

func newMetricsReader(tb testing.TB) *metricsReader {
	tb.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	tb.Cleanup(func() { assert.NoError(tb, provider.Shutdown(context.Background())) })

	return &metricsReader{ManualReader: reader, provider: provider}
}

// collected is one collection of plg's instruments, by name.
type collected map[string]metricdata.Metrics

func (r *metricsReader) collect(t *testing.T) collected {
	t.Helper()

	var rm metricdata.ResourceMetrics

	require.NoError(t, r.Collect(t.Context(), &rm))

	out := collected{}

	for _, scope := range rm.ScopeMetrics {
		require.Equal(t, "github.com/heynemann/go-plg", scope.Scope.Name)

		for _, m := range scope.Metrics {
			out[m.Name] = m
		}
	}

	return out
}

// int64Value returns the value of the data point of an int64 sum (a counter or an up-down counter) with exactly
// the given attributes, failing the test if there is none.
func (c collected) int64Value(t *testing.T, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()

	sum, ok := c[name].Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s is an int64 sum", name)

	want := attribute.NewSet(attrs...)
	for _, dp := range sum.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}

	require.Failf(
		t,
		"no data point",
		"%s has no data point %v; it has %v",
		name,
		want.Encoded(attribute.DefaultEncoder()),
		points(sum.DataPoints),
	)

	return 0
}

// histogramCount returns the count of the data point of a float64 histogram with exactly the given attributes.
func (c collected) histogramCount(t *testing.T, name string, attrs ...attribute.KeyValue) uint64 {
	t.Helper()

	hist, ok := c[name].Data.(metricdata.Histogram[float64])
	require.True(t, ok, "%s is a float64 histogram", name)

	want := attribute.NewSet(attrs...)
	for _, dp := range hist.DataPoints {
		if dp.Attributes.Equals(&want) {
			assert.Positive(t, dp.Sum, "%s: latencies are positive", name)

			return dp.Count
		}
	}

	require.Failf(t, "no data point", "%s has no data point %v", name, want.Encoded(attribute.DefaultEncoder()))

	return 0
}

func points(dps []metricdata.DataPoint[int64]) []string {
	out := make([]string, 0, len(dps))
	for _, dp := range dps {
		out = append(out, dp.Attributes.Encoded(attribute.DefaultEncoder()))
	}

	return out
}

// watchedContext is a context that signals when a call first waits on it, by asking for its Done channel: a pool
// with nothing idle and no room to grow waits on Done, so asked tells the test the call is waiting.
type watchedContext struct {
	context.Context //nolint:containedctx // the context the test watches is the one it hands the call

	asked   chan struct{}
	waiting atomic.Bool
}

func watch(ctx context.Context) *watchedContext {
	return &watchedContext{Context: ctx, asked: make(chan struct{})}
}

func (w *watchedContext) Done() <-chan struct{} {
	if w.waiting.CompareAndSwap(false, true) {
		close(w.asked)
	}

	return w.Context.Done()
}

// TestMetricsObserveEveryInstrument runs real Go plugins through every path an instrument measures, with an
// in-memory reader, and checks each instrument's data points and their attributes.
func TestMetricsObserveEveryInstrument(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	reader := newMetricsReader(t)
	api := newAPIServer(t)
	ctr := newContainer(t, plg.WithMeterProvider(reader.provider))

	// "m": two Go plugins with one instance each, and a Lua plugin the Register leaves out.
	store := goStorage(t, "m", map[string]string{"ref": "fooer", "crash": "fooer"})
	lua := plg.Descriptor{Key: "lua", Language: plg.LanguageLua, Entrypoint: "main.lua", Version: "v1"}
	require.NoError(t, store.Put(lua, fstest.MapFS{"main.lua": {Data: []byte("return {}")}}))
	require.NoError(t, plg.Register[testcontract.Fooer](ctx, ctr, "m", plg.WithStorage(store), plg.WithPool(1, 1),
		plg.WithGolangPlugins()))

	fetch := fetcherIn(t, golang(), ctr, "web", plg.WithHTTP(openPolicy(hostOf(t, api.URL))))
	broken := goStorage(t, "broken", map[string]string{"bad": "createfails"})
	require.ErrorIs(t, plg.Register[testcontract.Fooer](ctx, ctr, "broken", plg.WithStorage(broken)), plg.ErrLoad)

	ref, crash := getFooer(t, ctr, "m/ref"), getFooer(t, ctr, "m/crash")

	// Calls: ok, the plugin's error, a conversion error and a guest panic (on its own plugin: its instance's
	// replacement, by the refiller or the next caller, must not blur m/ref's pool metrics).
	_, err := ref.Foo(ctx, "metrics")
	require.NoError(t, err)
	require.Error(t, ref.Fail(ctx, "plugin error"))

	//nolint:dogsled // Scalars returns fourteen values; only its error matters here
	_, _, _, _, _, _, _, _, _, _, _, _, _, _, err = ref.Scalars(ctx, false, maxSafeInt+1, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, "")
	require.ErrorIs(t, err, plg.ErrConversion)
	require.ErrorIs(t, crash.Fail(ctx, "panic"), plg.ErrCall)

	// HTTP: one response, one refusal.
	_, err = fetch.Foo(ctx, "GET "+api.URL+"/v1/items")
	require.NoError(t, err)

	_, err = fetch.Foo(ctx, "GET http://not-allowed.example/")
	require.Error(t, err)

	// Pool: with m/ref's only instance held, a call waits until its context ends (unavailable), and another
	// waits until the held instance is discarded and the refiller starts the one it gets.
	held, err := plg.HoldInstance(ctx, ctr, "m/ref")
	require.NoError(t, err)

	busy := reader.collect(t)
	assert.Equal(t, int64(1), busy.int64Value(t, "plg.pool.instances.in_use", attribute.String("key", "m/ref")))

	canceled, cancel := context.WithCancel(ctx)
	waiter := watch(canceled)
	done := make(chan error)

	go func() {
		_, err := ref.Foo(waiter, "late")
		done <- err
	}()

	<-waiter.asked
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	waiter = watch(ctx)

	go func() {
		_, err := ref.Foo(waiter, "refilled")
		done <- err
	}()

	<-waiter.asked
	held.Discard()
	require.NoError(t, <-done, "the call got the instance the refiller started")

	// Reloads: m/ref's update fails to load and keeps its last good version; the Lua plugin is skipped again.
	update := plg.Descriptor{Key: "ref", Language: plg.LanguageGo, Entrypoint: "plugin.go", Version: "v2"}
	require.NoError(t, store.Put(update, pluginFiles("createfails")))
	require.Error(t, plg.ReloadInterface(ctx, ctr, "m"))

	got := reader.collect(t)

	keys := make([]string, 0, len(got))
	for name := range got {
		keys = append(keys, name)
	}

	slices.Sort(keys)
	assert.Equal(t, []string{
		"plg.http.duration", "plg.http.request.size", "plg.http.requests", "plg.http.response.size",
		"plg.plugin.call.duration", "plg.plugin.call.errors", "plg.plugin.calls", "plg.plugin.loads",
		"plg.plugin.memory", "plg.plugin.panics", "plg.plugin.reload.failures", "plg.plugin.skipped",
		"plg.pool.instances", "plg.pool.instances.in_use", "plg.pool.refills", "plg.pool.waits",
	}, keys, "every instrument")

	key := func(k string) attribute.KeyValue { return attribute.String("key", k) }
	goLang := attribute.String("language", "go")
	method := func(m string) attribute.KeyValue { return attribute.String("method", m) }
	outcome := func(o string) attribute.KeyValue { return attribute.String("outcome", o) }

	for _, tc := range []struct {
		method, outcome string
		calls           int64
	}{
		{"Foo", "ok", 2}, {"Fail", "error", 1}, {"Scalars", "conversion", 1}, {"Foo", "unavailable", 1},
	} {
		attrs := []attribute.KeyValue{key("m/ref"), goLang, method(tc.method), outcome(tc.outcome)}
		assert.Equal(t, tc.calls, got.int64Value(t, "plg.plugin.calls", attrs...), "%+v", tc)
		assert.EqualValues(t, tc.calls, got.histogramCount(t, "plg.plugin.call.duration", attrs...), "%+v", tc)

		if tc.outcome != "ok" {
			assert.Equal(t, tc.calls, got.int64Value(t, "plg.plugin.call.errors", attrs...), "%+v", tc)
		}
	}

	crashed := []attribute.KeyValue{key("m/crash"), goLang, method("Fail"), outcome("panic")}
	assert.Equal(t, int64(1), got.int64Value(t, "plg.plugin.calls", crashed...))
	assert.Equal(t, int64(1), got.int64Value(t, "plg.plugin.call.errors", crashed...))
	assert.Equal(t, int64(1), got.int64Value(t, "plg.plugin.panics", key("m/crash"), attribute.String("phase", "call")))

	assert.Equal(t, int64(2), got.int64Value(t, "plg.pool.waits", key("m/ref")))
	assert.Equal(t, int64(1), got.int64Value(t, "plg.pool.refills", key("m/ref"), outcome("ok")))
	assert.Equal(t, int64(1), got.int64Value(t, "plg.pool.instances", key("m/ref")))
	assert.Zero(t, got.int64Value(t, "plg.pool.instances.in_use", key("m/ref")))

	memory := got.int64Value(t, "plg.plugin.memory", key("m/ref"))
	assert.Positive(t, memory)
	assert.Zero(t, memory%(64<<10), "whole wasm pages")
	assert.LessOrEqual(t, memory, int64(128<<20), "one instance, within its limit")

	assert.Equal(t, int64(1), got.int64Value(t, "plg.plugin.loads", key("m/ref"), goLang, outcome("ok")))
	assert.Equal(t, int64(1), got.int64Value(t, "plg.plugin.loads", key("m/ref"), goLang, outcome("failed")))
	assert.Equal(t, int64(1), got.int64Value(t, "plg.plugin.loads", key("web/fetch"), goLang, outcome("ok")))
	assert.Equal(t, int64(1), got.int64Value(t, "plg.plugin.loads", key("broken/bad"), goLang, outcome("failed")))
	assert.Equal(t, int64(1), got.int64Value(t, "plg.plugin.reload.failures", key("m/ref")))
	assert.Equal(t, int64(2), got.int64Value(t, "plg.plugin.skipped", key("m/lua"),
		attribute.String("language", "lua")), "at Register and at the reload")

	web := key("web/fetch")
	assert.Equal(t, int64(1), got.int64Value(t, "plg.http.requests", web, outcome("ok"), attribute.Int("status", 200)))
	assert.Equal(t, int64(1), got.int64Value(t, "plg.http.requests", web, outcome("refused")))
	assert.Equal(t, uint64(1), got.histogramCount(t, "plg.http.duration", web, outcome("ok")))
	assert.Equal(t, uint64(1), got.histogramCount(t, "plg.http.duration", web, outcome("refused")))
	assert.Positive(t, got.int64Value(t, "plg.http.request.size", web))
	assert.Greater(t, got.int64Value(t, "plg.http.response.size", web), int64(len("GET /v1/items fetcher")),
		"the encoded response holds the body")
}

// BenchmarkCallGoMetered is BenchmarkCallGo with an OpenTelemetry SDK meter provider collecting every
// measurement, against BenchmarkCallGo's default global (no-op) provider: the cost of metrics on a call.
func BenchmarkCallGoMetered(b *testing.B) {
	reader := newMetricsReader(b)
	ctr := newContainer(b, plg.WithMeterProvider(reader.provider))
	registerFooers(b, ctr, "fooer", map[string]string{"ref": "fooer"})

	ref := getFooer(b, ctr, "fooer/ref")
	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		_, err := ref.Foo(ctx, "bench")
		if err != nil {
			b.Fatal(err)
		}
	}
}
