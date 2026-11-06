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

// pointSet is the attribute set of a data point: attrs, plus the empty team, org and company attributes of a
// plugin without an owner when attrs name a plugin key but no team.
func pointSet(attrs []attribute.KeyValue) attribute.Set {
	set := attribute.NewSet(attrs...)
	if set.HasValue("key") && !set.HasValue("team") {
		set = attribute.NewSet(append(slices.Clone(attrs), attribute.String("team", ""), attribute.String("org", ""),
			attribute.String("company", ""))...)
	}

	return set
}

// int64Value returns the value of the data point of an int64 sum (a counter or an up-down counter) with exactly
// the given attributes (see pointSet), failing the test if there is none.
func (c collected) int64Value(t *testing.T, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()

	sum, ok := c[name].Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s is an int64 sum", name)

	want := pointSet(attrs)
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

// histogramCount returns the count of the data point of a float64 histogram with exactly the given attributes (see
// pointSet).
func (c collected) histogramCount(t *testing.T, name string, attrs ...attribute.KeyValue) uint64 {
	t.Helper()

	hist, ok := c[name].Data.(metricdata.Histogram[float64])
	require.True(t, ok, "%s is a float64 histogram", name)

	want := pointSet(attrs)
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
	require.NoError(t, store.Put(&lua, fstest.MapFS{"main.lua": {Data: []byte("return {}")}}))
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
	assert.Equal(t, int64(1), busy.int64Value(t, "plg.pool.calls.in_flight", attribute.String("key", "m/ref")))

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
	require.NoError(t, store.Put(&update, pluginFiles("createfails")))
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
		"plg.pool.calls.in_flight", "plg.pool.instances", "plg.pool.instances.in_use", "plg.pool.refills",
		"plg.pool.waits",
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
	assert.Zero(t, got.int64Value(t, "plg.pool.calls.in_flight", key("m/ref")))

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

// TestMetricsCoverEveryLanguage: the layer every guest shares measures a plugin's load, calls, HTTP requests and
// memory alike, whatever its language.
func TestMetricsCoverEveryLanguage(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		ctx := t.Context()
		reader := newMetricsReader(t)
		api := newAPIServer(t)
		ctr := lang.binder(newContainer(t, plg.WithMeterProvider(reader.provider)))

		store := lang.storage(t, "m", map[string]string{"ref": "fooer"})
		require.NoError(t, plg.Register[testcontract.Fooer](ctx, ctr, "m", plg.WithStorage(store)))

		ref := getFooer(t, ctr, "m/ref")
		_, err := ref.Foo(ctx, "metrics")
		require.NoError(t, err)
		require.Error(t, ref.Fail(ctx, "plugin error"))

		fetch := fetcherIn(t, lang, ctr, "web", plg.WithHTTP(openPolicy(hostOf(t, api.URL))))
		_, err = fetch.Foo(ctx, "GET "+api.URL+"/v1/items")
		require.NoError(t, err)

		got := reader.collect(t)
		mref, language := attribute.String("key", "m/ref"), attribute.String("language", string(lang.lang))

		assert.Equal(
			t,
			int64(1),
			got.int64Value(t, "plg.plugin.loads", mref, language, attribute.String("outcome", "ok")),
		)
		assert.Equal(
			t,
			int64(1),
			got.int64Value(t, "plg.plugin.calls", mref, language, attribute.String("method", "Foo"),
				attribute.String("outcome", "ok")),
		)
		assert.Equal(t, int64(1), got.int64Value(t, "plg.plugin.call.errors", mref, language,
			attribute.String("method", "Fail"), attribute.String("outcome", "error")))
		assert.Equal(t, int64(1), got.int64Value(t, "plg.http.requests", attribute.String("key", "web/fetch"),
			attribute.String("outcome", "ok"), attribute.Int("status", 200)))

		memory := got.int64Value(t, "plg.plugin.memory", mref)
		assert.Positive(t, memory)
		assert.Zero(t, memory%(64<<10), "whole wasm pages")
	})
}

// goInstance is the memory limit of one Go plugin instance: the Register default.
const goInstance = 128 * mib

// TestRegisterContainsAPanickingMeterProvider registers Go plugins while every measurement panics: each count of a
// load, ok or failed, is a PanicError in PhaseCallback; a Register whose plugins load succeeds, and one whose
// plugin cannot be created returns ErrLoad and leaks nothing: no instance, no version's reservation, not its name.
func TestRegisterContainsAPanickingMeterProvider(t *testing.T) {
	t.Parallel()

	broken := new(atomic.Bool)
	broken.Store(true)

	ctr := newContainer(t, plg.WithMeterProvider(brokenMeters{broken: broken}))
	register := func(name string, versions map[string]string) error {
		store := plg.NewMemoryStorage(name)
		for key, version := range versions {
			putVersioned(t, store, key, version)
		}

		var err error

		require.NotPanics(t, func() {
			err = plg.Register[testcontract.Fooer](t.Context(), ctr, name, plg.WithStorage(store), plg.WithPool(1, 1))
		}, "registering %s", name)

		return err
	}
	inUse := func(t *testing.T, instances uint64, msg string) {
		t.Helper()
		assert.Equal(t, instances*goInstance, plg.MemoryInUse(ctr), "%s: instances alive", msg)
		assert.Equal(t, instances*goInstance, plg.MemoryReserved(ctr), "%s: versions held", msg)
	}

	require.NoError(t, register("ok", map[string]string{"a": "v1", "b": "v1"}))
	assert.Equal(t, "v1", inspect(t, ctr, "ok/a").Version)
	assert.Equal(t, "v1", inspect(t, ctr, "ok/b").Version)
	inUse(t, 2, "both published")
	assert.Equal(t, []string{"ok/a", "ok/b"}, meterPanicKeys(t, ctr.RecentPanics()), "counting each load")

	require.ErrorIs(t, register("bad", map[string]string{"a": "v1", "b": "create-fails"}), plg.ErrLoad)
	assert.Nil(t, plg.Generation(ctr, "bad/a"), "nothing published")
	inUse(t, 2, "bad/a closed")
	assert.Equal(t, []string{"ok/a", "ok/b", "bad/a", "bad/b"}, meterPanicKeys(t, ctr.RecentPanics()),
		"counting bad/a's load, then bad/b's failure")

	require.NoError(t, register("bad", map[string]string{"a": "v1"}), "the failed Register gave its name back")
	inUse(t, 3, "bad/a published")
}

// TestReloadContainsAPanickingMeterProvider reloads a Go plugin once every measurement panics: counting the new
// version's load, and a broken update's failed load and reload failure, are PanicErrors in PhaseCallback; the
// update is served, the broken one is not, nothing leaks (instances, versions' reservations) and the reloader keeps
// serving rounds.
func TestReloadContainsAPanickingMeterProvider(t *testing.T) {
	t.Parallel()

	broken := new(atomic.Bool)
	ctr := newContainer(t, plg.WithMeterProvider(brokenMeters{broken: broken}))
	store := plg.NewMemoryStorage("metered")
	putVersioned(t, store, "k", "v1")
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "metered", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	broken.Store(true)

	served := func(t *testing.T, version string) {
		t.Helper()
		require.NoError(t, plg.AwaitRetired(t.Context(), ctr))
		assert.Equal(t, version, inspect(t, ctr, "metered/k").Version)
		assert.Equal(t, uint64(goInstance), plg.MemoryInUse(ctr), "%s's one instance alone", version)
		assert.Equal(t, uint64(goInstance), plg.MemoryReserved(ctr), "%s alone", version)
	}

	putVersioned(t, store, "k", "v2")
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "metered"))
	served(t, "v2")
	assert.Equal(t, []string{"metered/k"}, meterPanicKeys(t, ctr.RecentPanics()), "counting the load")

	putVersioned(t, store, "k", "create-fails")
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "metered"), plg.ErrLoad)
	served(t, "v2")
	assert.Equal(t, []string{"metered/k", "metered/k", "metered/k"}, meterPanicKeys(t, ctr.RecentPanics()),
		"counting the failed load and the reload failure")

	putVersioned(t, store, "k", "v3")
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "metered"), "the reloader serves the next round")
	served(t, "v3")
	assert.Equal(t, "v3:x", greet(t, getFooer(t, ctr, "metered/k"), "x"))
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
