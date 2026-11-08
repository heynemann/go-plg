//go:build integration

package plg_test

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// spanRecorder is an OpenTelemetry SDK tracer provider exporting every span to memory as it ends.
type spanRecorder struct {
	provider *sdktrace.TracerProvider
	exporter *tracetest.InMemoryExporter
}

// newSpanRecorder returns a span recorder, shut down when the test ends (after the containers created later are
// closed).
func newSpanRecorder(tb testing.TB) *spanRecorder {
	tb.Helper()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	tb.Cleanup(func() { assert.NoError(tb, provider.Shutdown(context.Background())) })

	return &spanRecorder{provider: provider, exporter: exporter}
}

// named returns the ended spans named name in the trace with the given ID, in the order they ended.
func (r *spanRecorder) named(id trace.TraceID, name string) []tracetest.SpanStub {
	var out []tracetest.SpanStub

	spans := r.exporter.GetSpans()
	for i := range spans {
		if spans[i].Name == name && spans[i].SpanContext.TraceID() == id {
			out = append(out, spans[i])
		}
	}

	return out
}

// attrs returns a span's attributes as strings.
func attrs(span *tracetest.SpanStub) map[string]string {
	out := map[string]string{}

	for _, kv := range span.Attributes {
		out[string(kv.Key)] = kv.Value.String()
	}

	return out
}

// requireChild requires span to be a child of parent.
func requireChild(t *testing.T, parent trace.SpanContext, span *tracetest.SpanStub) {
	t.Helper()

	require.Equal(t, parent.TraceID(), span.SpanContext.TraceID(), "%s is in its parent's trace", span.Name)
	require.Equal(t, parent.SpanID(), span.Parent.SpanID(), "%s is a child of its parent", span.Name)
}

// TestCallSpansCarryThePluginAndItsOwner calls each language's owned hostile plugin under a request's span: each
// call is a child span with the plugin's key, version, language, method, outcome and owner, failures have the
// error status, and what the plugin writes is logged with the IDs of the call's span. One container serves
// every language, each registered under a name of its own.
func TestCallSpansCarryThePluginAndItsOwner(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	spans := newSpanRecorder(t)
	// slog's JSON handler serializes its writes; the test reads once the container is closed.
	ctr := newContainer(t, plg.WithTracerProvider(spans.provider),
		plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	langs := contractLanguages()
	printed := make([]trace.SpanContext, len(langs)) // the span of each language's call writing output

	t.Cleanup(func() { // once the subtests ended
		require.NoError(t, ctr.Close(context.Background())) // every record is written

		keys := map[string]trace.SpanContext{}
		for i, lang := range langs {
			keys["owned"+lang.dir+"/hostile"] = printed[i]
		}

		assertOutputTraced(t, logs.Bytes(), keys)
	})

	for i, lang := range langs {
		t.Run(string(lang.lang), func(t *testing.T) {
			t.Parallel()

			name := "owned" + lang.dir
			key := name + "/hostile"
			require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, name,
				plg.WithStorage(ownedPlugin(t, lang, fullOwner)), plg.WithPool(1, 1),
				plg.WithMemoryLimit(hostileLimit)))

			owned := getFooer(t, ctr, key)
			state, err := plg.Info(ctr, key)
			require.NoError(t, err)
			require.NotEmpty(t, state.Version)

			ctx, request := spans.provider.Tracer("test").Start(t.Context(), "request")
			_, err = owned.Foo(ctx, "x")
			require.NoError(t, err)
			require.Error(t, owned.Fail(ctx, "owned failure"))
			require.Error(t, owned.Fail(ctx, "print"), "the plugin writes to stdout, then fails")
			request.End()

			parent := request.SpanContext()
			plugin := map[string]string{
				"key": key, "version": state.Version, "language": string(lang.lang),
				"team": "payments", "org": "commerce", "company": "Acme Inc.",
			}
			with := func(method, outcome string) map[string]string {
				want := map[string]string{"method": method, "outcome": outcome}
				maps.Copy(want, plugin)

				return want
			}

			foo := spans.named(parent.TraceID(), "plg.call "+key+".Foo")
			require.Len(t, foo, 1)
			requireChild(t, parent, &foo[0])

			assert.Equal(t, with("Foo", "ok"), attrs(&foo[0]))
			assert.Equal(t, codes.Unset, foo[0].Status.Code)
			assert.Equal(t, trace.SpanKindInternal, foo[0].SpanKind)

			fail := spans.named(parent.TraceID(), "plg.call "+key+".Fail")
			require.Len(t, fail, 2)
			requireChild(t, parent, &fail[0])

			assert.Equal(t, with("Fail", "error"), attrs(&fail[0]))
			assert.Equal(t, sdktrace.Status{Code: codes.Error, Description: "owned failure"}, fail[0].Status)
			require.Len(t, fail[0].Events, 1)
			assert.Equal(t, "exception", fail[0].Events[0].Name)

			requireChild(t, parent, &fail[1])
			assert.NotEqual(t, "ok", attrs(&fail[1])["outcome"])
			assert.Equal(t, codes.Error, fail[1].Status.Code)

			printed[i] = fail[1].SpanContext
		})
	}
}

// assertOutputTraced checks that logs (JSON lines) hold guest output of every plugin key in spans, all of it
// carrying the trace and span IDs of the key's span.
func assertOutputTraced(t *testing.T, logs []byte, spans map[string]trace.SpanContext) {
	t.Helper()

	lines := bufio.NewScanner(bytes.NewReader(logs))
	lines.Buffer(nil, 1<<20)

	outputs := map[string]int{}

	for lines.Scan() {
		var rec struct {
			Msg     string `json:"msg"`
			Key     string `json:"key"`
			TraceID string `json:"trace_id"` //nolint:tagliatelle // the log record's attribute names
			SpanID  string `json:"span_id"`  //nolint:tagliatelle // the log record's attribute names
			Line    string `json:"line"`
		}

		require.NoError(t, sonic.Unmarshal(lines.Bytes(), &rec))

		if rec.Msg != "plg: guest output" {
			continue
		}

		span, ok := spans[rec.Key]
		require.True(t, ok, "output of %s", rec.Key)

		outputs[rec.Key]++

		assert.Equal(t, span.TraceID().String(), rec.TraceID, "%s: %q", rec.Key, rec.Line)
		assert.Equal(t, span.SpanID().String(), rec.SpanID, "%s: %q", rec.Key, rec.Line)
	}

	require.NoError(t, lines.Err())

	for key := range spans {
		assert.Positive(t, outputs[key], "%s's output was logged", key)
	}
}

// TestHTTPRequestsAreSpansCarryingTraceparent: each request a plugin makes is a client span, a child of its call's,
// and reaches the server with that span's traceparent; a refused request is a span with the error status. Script
// plugins run multiplexed, so the spans cross the multiplexer too. One container serves every language.
func TestHTTPRequestsAreSpansCarryingTraceparent(t *testing.T) {
	t.Parallel()

	spans := newSpanRecorder(t)
	ctr := newContainer(t, plg.WithTracerProvider(spans.provider))

	for _, lang := range contractLanguages() {
		t.Run(string(lang.lang), func(t *testing.T) {
			t.Parallel()

			traceparents := make(chan string, 1)
			api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
				traceparents <- req.Header.Get("Traceparent")

				writer.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(api.Close)

			name := "web" + lang.dir
			plug := fetcherIn(t, lang, ctr, name, plg.WithHTTP(openPolicy(hostOf(t, api.URL))), plg.WithPool(1, 1),
				plg.WithConcurrentCalls(2))

			ctx, request := spans.provider.Tracer("test").Start(t.Context(), "request")
			_, err := plug.Foo(ctx, "GET "+api.URL+"/v1/items?token=secret")
			require.NoError(t, err)

			_, err = plug.Foo(ctx, "GET http://refused.example/v1")
			require.Error(t, err, "the host is not allowed")
			request.End()

			parent := request.SpanContext()
			calls := spans.named(parent.TraceID(), "plg.call "+name+"/fetch.Foo")
			requests := spans.named(parent.TraceID(), "plg.http GET")

			require.Len(t, calls, 2)
			require.Len(t, requests, 2)
			requireChild(t, parent, &calls[0])
			requireChild(t, calls[0].SpanContext, &requests[0])

			assert.Equal(t, trace.SpanKindClient, requests[0].SpanKind)
			assert.Equal(t, map[string]string{
				"key": name + "/fetch", "version": "v1", "language": string(lang.lang), "team": "", "org": "",
				"company": "", "http.request.method": "GET", "url.full": api.URL + "/v1/items", "outcome": "ok",
				"http.response.status_code": "204",
			}, attrs(&requests[0]), "the URL without its query")
			assert.Equal(t, codes.Unset, requests[0].Status.Code)
			assert.Equal(t, "00-"+parent.TraceID().String()+"-"+requests[0].SpanContext.SpanID().String()+"-01",
				<-traceparents, "the server got the request's span context")

			requireChild(t, parent, &calls[1])
			requireChild(t, calls[1].SpanContext, &requests[1])
			assert.Equal(t, "refused", attrs(&requests[1])["outcome"])
			assert.NotContains(t, attrs(&requests[1]), "http.response.status_code")
			assert.Equal(t, codes.Error, requests[1].Status.Code)
			assert.Contains(t, requests[1].Status.Description, "host not allowed")
			assert.Equal(t, codes.Error, calls[1].Status.Code, "the plugin failed with the refusal")
		})
	}
}

// spanHost is the test Host whose Value returns the ID of the span in its context.
type spanHost struct {
	*testHost
}

func (spanHost) Value(ctx context.Context) (string, error) {
	return trace.SpanContextFromContext(ctx).SpanID().String(), nil
}

// TestHostMethodsAreChildSpans: each Host method a plugin calls is a span, a child of the call's, which the Host
// method's context carries; a Host method failing has the error status, and so does the call it fails. One
// container serves every language.
func TestHostMethodsAreChildSpans(t *testing.T) {
	t.Parallel()

	spans := newSpanRecorder(t)
	ctr := newContainer(t, plg.WithTracerProvider(spans.provider))

	for _, lang := range contractLanguages() {
		t.Run(string(lang.lang), func(t *testing.T) {
			t.Parallel()

			name := "hosted" + lang.dir
			key := name + "/ref"
			require.NoError(t, plg.Register[testcontract.Hosted](t.Context(), ctr, name,
				plg.WithStorage(lang.storage(t, name, map[string]string{"ref": "hosted"})), plg.WithPool(1, 1),
				plg.WithHost[testcontract.Host](spanHost{newTestHost()})))

			hosted, err := plg.Get[testcontract.Hosted](t.Context(), ctr, key)
			require.NoError(t, err)

			ctx, request := spans.provider.Tracer("test").Start(t.Context(), "request")
			seen, err := hosted.Value(ctx)
			require.NoError(t, err)

			_, err = hosted.Describe(ctx, "nobody")
			require.Error(t, err)
			request.End()

			parent := request.SpanContext()
			value := spans.named(parent.TraceID(), "plg.call "+key+".Value")
			host := spans.named(parent.TraceID(), "plg.host testcontract.Host.Value")

			require.Len(t, value, 1)
			require.Len(t, host, 1)
			requireChild(t, parent, &value[0])
			requireChild(t, value[0].SpanContext, &host[0])

			assert.Equal(t, host[0].SpanContext.SpanID().String(), seen, "the Host method's context holds its span")
			assert.Equal(t, map[string]string{
				"key": key, "version": "v1", "language": string(lang.lang), "team": "", "org": "", "company": "",
				"method": "testcontract.Host.Value", "outcome": "ok",
			}, attrs(&host[0]))
			assert.Equal(t, codes.Unset, host[0].Status.Code)

			describe := spans.named(parent.TraceID(), "plg.call "+key+".Describe")
			lookup := spans.named(parent.TraceID(), "plg.host testcontract.Host.Lookup")

			require.Len(t, describe, 1)
			require.Len(t, lookup, 1)
			requireChild(t, describe[0].SpanContext, &lookup[0])

			assert.Equal(t, "error", attrs(&lookup[0])["outcome"])
			assert.Equal(t, codes.Error, lookup[0].Status.Code)
			assert.Contains(t, lookup[0].Status.Description, `"nobody"`)
			assert.Equal(t, codes.Error, describe[0].Status.Code, "the call failed with the Host's error")
		})
	}
}

// tracerProvider is a tracer provider a benchmark runs under, built for b.
type tracerProvider struct {
	provider func(b *testing.B) trace.TracerProvider
	name     string
}

// tracerProviders returns the global default (no SDK installed), a no-op provider, and an SDK provider recording
// every span (exporting none).
func tracerProviders() []tracerProvider {
	return []tracerProvider{
		{name: "tracer=default", provider: func(*testing.B) trace.TracerProvider { return nil }},
		{name: "tracer=noop", provider: func(*testing.B) trace.TracerProvider { return noop.NewTracerProvider() }},
		{name: "tracer=sdk", provider: func(b *testing.B) trace.TracerProvider {
			b.Helper()

			provider := sdktrace.NewTracerProvider()

			b.Cleanup(func() { assert.NoError(b, provider.Shutdown(context.Background())) })

			return provider
		}},
	}
}

// BenchmarkCallLuaTraced is BenchmarkCallLua under each of tracerProviders: a no-op provider adds no allocation to
// a call.
func BenchmarkCallLuaTraced(b *testing.B) {
	for _, tc := range tracerProviders() {
		b.Run(tc.name, func(b *testing.B) {
			ctr := newContainer(b, plg.WithTracerProvider(tc.provider(b)))
			registerFooersIn(b, luaLanguage(), ctr, "fooer", map[string]string{"ref": "fooer"})

			ref := getFooer(b, ctr, "fooer/ref")
			ctx := b.Context()

			b.ReportAllocs()

			for b.Loop() {
				_, err := ref.Foo(ctx, "bench")
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCallLuaTracedParallel is BenchmarkCallLuaTraced from GOMAXPROCS goroutines, each with an instance of its
// own (compare BenchmarkCallLuaParallel/pool=N): the calls start and end their spans, and look up the global
// provider, concurrently.
func BenchmarkCallLuaTracedParallel(b *testing.B) {
	size := runtime.GOMAXPROCS(0)

	for _, tc := range tracerProviders() {
		b.Run(tc.name, func(b *testing.B) {
			ctr := newContainer(b, plg.WithTracerProvider(tc.provider(b)))
			dirs := map[string]string{"ref": "fooer"}
			registerFooersIn(b, luaLanguage(), ctr, "fooer", dirs, plg.WithPool(size, size))

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
