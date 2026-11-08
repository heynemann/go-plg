package plg

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/httpcap"
	"github.com/heynemann/go-plg/internal/wire"
)

// nopCall is a call without arguments or results, allocating nothing.
type nopCall struct{}

func (nopCall) AppendArgs(b []byte) ([]byte, error) { return b, nil }

func (nopCall) DecodeResults(*wire.Reader) error { return nil }

// tracedPlugin returns a generation of plugin "test/k" serving testIface, owned by team t of org o of company c,
// whose single instance answers with call, traced by tracing (nil traces nothing), and its Do method.
func tracedPlugin(t *testing.T, reporter *panicReporter, tracing *tracing, call func() (engine.Result, error),
) (*plugin, *method) {
	t.Helper()

	iface := reflect.TypeFor[testIface]()
	meth, err := newMethod(new(wire.Planner), iface, 0, 0)
	require.NoError(t, err)

	instances := warmPool(t, &fakeSpawner{call: call}, &poolConfig{panics: reporter, minSize: 1, maxSize: 1})
	t.Cleanup(func() { assert.NoError(t, instances.close(context.Background())) })

	plug := &plugin{key: "test/k", desc: Descriptor{Language: LanguageGo}, pool: instances}

	if tracing != nil {
		owner := newOwnerTags(&Owner{Team: Team{Name: "t"}, Org: "o", Company: "c"}, "v1", "s")
		plug.spans = newPluginSpans(tracing, "test/k", LanguageGo, owner, iface, nil)
	}

	return plug, meth
}

func answering(res engine.Result, err error) func() (engine.Result, error) {
	return func() (engine.Result, error) { return res, err }
}

// TestNoopTracingDoesNotAllocate: with a no-op provider (or the API's default global one, before an SDK replaces
// it), a call allocates exactly what it does untraced (`make allocs` runs it uninstrumented); a recording
// provider allocates, so the comparison measures the spans.
//
//nolint:paralleltest // testing.AllocsPerRun panics in parallel tests
func TestNoopTracingDoesNotAllocate(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("allocations are measured without coverage instrumentation, by make allocs")
	}

	reporter := reporting(slog.DiscardHandler)
	recording := sdktrace.NewTracerProvider()

	t.Cleanup(func() { assert.NoError(t, recording.Shutdown(context.Background())) })

	failures := 0
	allocs := func(tracing *tracing) float64 {
		plug, meth := tracedPlugin(t, reporter, tracing, answering(engine.Result{Status: wire.StatusOK}, nil))

		return testing.AllocsPerRun(100, func() {
			if plug.call(t.Context(), meth, nopCall{}) != nil {
				failures++
			}
		})
	}

	untraced := allocs(nil)
	assert.InDelta(t, untraced, allocs(newTracing(noop.NewTracerProvider(), reporter)), 0, "noop")

	deprecated := trace.NewNoopTracerProvider() //nolint:staticcheck // SA1019: the deprecated no-op one is still one
	assert.InDelta(t, untraced, allocs(newTracing(deprecated, reporter)), 0, "deprecated noop")

	// The API's default global provider as the global one: what newTracing makes of it, with the global
	// provider unchanged since (whichever it is in this process).
	idle := &tracing{tracer: recording.Tracer("test"), idle: otel.GetTracerProvider(), panics: reporter}
	assert.InDelta(t, untraced, allocs(idle), 0, "the API's default global provider, before an SDK replaces it")
	assert.Greater(t, allocs(newTracing(recording, reporter)), untraced, "a recording provider")
	assert.Zero(t, failures)
}

func TestCallSpansRecordTheirOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err     error
		name    string
		outcome string
		res     engine.Result
	}{
		{name: "ok", res: engine.Result{Status: wire.StatusOK}, outcome: "ok"},
		{name: "plugin error", res: engine.Result{Status: wire.StatusError, Message: "no"}, outcome: "error"},
		{name: "conversion", res: engine.Result{Status: wire.StatusConvert, Message: "big"}, outcome: "conversion"},
		{name: "guest panic", res: engine.Result{Status: wire.StatusPanic, Message: "boom"}, outcome: "panic"},
		{name: "unavailable", err: engine.ErrStopped, outcome: "unavailable"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

			t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })

			reporter := reporting(slog.DiscardHandler)
			plug, meth := tracedPlugin(t, reporter, newTracing(provider, reporter), answering(tc.res, tc.err))

			ctx, parent := provider.Tracer("test").Start(t.Context(), "request")
			err := plug.call(ctx, meth, nopCall{})

			parent.End()

			spans := exporter.GetSpans()
			require.Len(t, spans, 2, "the call's and its parent")

			span := spans[0]
			assert.Equal(t, "plg.call test/k.Do", span.Name)
			assert.Equal(t, parent.SpanContext().SpanID(), span.Parent.SpanID())
			assert.ElementsMatch(t, []string{
				"key=test/k", "version=v1", "language=go", "method=Do", "outcome=" + tc.outcome, "team=t", "org=o",
				"company=c",
			}, attrStrings(&span))

			if tc.outcome == "ok" {
				require.NoError(t, err)
				assert.Equal(t, sdktrace.Status{Code: codes.Unset}, span.Status)
				assert.Empty(t, span.Events)

				return
			}

			require.Error(t, err)
			assert.Equal(t, sdktrace.Status{Code: codes.Error, Description: err.Error()}, span.Status)
			require.Len(t, span.Events, 1)
			assert.Equal(t, "exception", span.Events[0].Name)
		})
	}
}

// attrStrings returns a span's attributes as key=value strings.
func attrStrings(span *tracetest.SpanStub) []string {
	out := make([]string, 0, len(span.Attributes))
	for _, kv := range span.Attributes {
		out = append(out, string(kv.Key)+"="+kv.Value.String())
	}

	return out
}

// panickingTracerProvider panics where it is told to: creating a tracer, starting a span or ending one.
type panickingTracerProvider struct {
	noop.TracerProvider

	where string
}

//nolint:ireturn // the OpenTelemetry API's interface
func (p panickingTracerProvider) Tracer(string, ...trace.TracerOption) trace.Tracer {
	if p.where == "Tracer" {
		panic("tracer bug")
	}

	return panickingTracer{where: p.where}
}

type panickingTracer struct {
	noop.Tracer

	where string
}

//nolint:ireturn // the OpenTelemetry API's interface
func (t panickingTracer) Start(ctx context.Context, _ string, _ ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	if t.where == "Start" {
		panic("tracer bug")
	}

	return ctx, panickingSpan{}
}

type panickingSpan struct {
	noop.Span
}

func (panickingSpan) End(...trace.SpanEndOption) {
	panic("tracer bug")
}

// TestTracerProviderPanicsAreContained: a tracer provider panicking creates no tracer, or fails no call; each
// panic is a PanicError in PhaseCallback naming the provider.
func TestTracerProviderPanicsAreContained(t *testing.T) {
	t.Parallel()

	t.Run("Tracer", func(t *testing.T) {
		t.Parallel()

		reporter := reporting(slog.DiscardHandler)
		assert.False(t, newTracing(panickingTracerProvider{where: "Tracer"}, reporter).on(), "no tracer: off")
		requireTracerPanics(t, reporter)
	})

	for _, where := range []string{"Start", "End"} {
		t.Run(where, func(t *testing.T) {
			t.Parallel()

			reporter := reporting(slog.DiscardHandler)
			tracing := newTracing(panickingTracerProvider{where: where}, reporter)
			plug, meth := tracedPlugin(t, reporter, tracing, answering(engine.Result{Status: wire.StatusOK}, nil))
			require.NoError(t, plug.call(t.Context(), meth, nopCall{}), "the call goes on")

			ctx, ended := plug.spans.httpRequest(t.Context())
			if where == "Start" {
				assert.Nil(t, ended, "no span to end")
				assert.Equal(t, t.Context(), ctx)
			} else {
				assert.NotPanics(t, func() { ended(httpcap.Report{Method: "GET"}) })
			}

			requireTracerPanics(t, reporter)
		})
	}
}

// requireTracerPanics requires reporter to have recorded panics, every one the tracer provider's.
func requireTracerPanics(t *testing.T, reporter *panicReporter) {
	t.Helper()

	recent := reporter.history.recent()
	require.NotEmpty(t, recent)

	for _, failed := range recent {
		assert.Equal(t, PhaseCallback, failed.Phase)
		assert.Equal(t, "trace.TracerProvider", failed.Method)
		assert.Equal(t, "tracer bug", failed.Value)
	}
}

func TestHostOutcome(t *testing.T) {
	t.Parallel()

	answer := func(status wire.Status, msg string) []byte {
		return wire.AppendMessage([]byte{byte(status)}, msg)
	}

	for _, tc := range []struct {
		err           error
		name, outcome string
		message       string
		answer        []byte
	}{
		{name: "ok", answer: []byte{byte(wire.StatusOK), 1, 2}, outcome: "ok"},
		{name: "error", answer: answer(wire.StatusError, "no user"), outcome: "error", message: "no user"},
		{name: "conversion", answer: answer(wire.StatusConvert, "too big"), outcome: "conversion", message: "too big"},
		{name: "panic", answer: answer(wire.StatusPanic, "bug"), outcome: "panic", message: "bug"},
		{name: "not served", err: errHostArgs, outcome: "error", message: errHostArgs.Error()},
		{name: "no answer", outcome: "error", message: "empty answer"},
	} {
		got := hostOutcome(tc.answer, tc.err)
		assert.Equal(t, tc.outcome, got.outcome.String(), tc.name)
		assert.Equal(t, tc.message, got.message, tc.name)
	}

	got := hostOutcome([]byte{byte(wire.StatusError), 9}, nil)
	assert.Equal(t, callError, got.outcome)
	assert.Contains(t, got.message, "undecodable message: ", "a truncated message")
}

// TestTracingFollowsTheGlobalProvider checks, in a process of its own (the global provider can be replaced only
// once per process), that the default tracing is the global provider: off while it is the API's default, on
// once an SDK replaces it.
func TestTracingFollowsTheGlobalProvider(t *testing.T) {
	t.Parallel()

	if os.Getenv(GlobalTracingChild) == "" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], //nolint:gosec // G204: this test binary, re-run
			"-test.run=^TestTracingFollowsTheGlobalProvider$", "-test.count=1", "-test.v")

		cmd.Env = append(os.Environ(), GlobalTracingChild+"=1")

		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		assert.Contains(t, string(out), "--- PASS: TestTracingFollowsTheGlobalProvider", "the child ran the test")

		return
	}

	reporter := reporting(slog.DiscardHandler)
	tracing := newTracing(nil, reporter)
	require.NotNil(t, tracing.idle, "the API's default global provider is recognised")
	assert.False(t, tracing.on())

	plug, meth := tracedPlugin(t, reporter, tracing, answering(engine.Result{Status: wire.StatusOK}, nil))
	require.NoError(t, plug.call(t.Context(), meth, nopCall{}))

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	otel.SetTracerProvider(provider)

	assert.True(t, tracing.on(), "the SDK replaced the default")
	require.NoError(t, plug.call(t.Context(), meth, nopCall{}))

	spans := exporter.GetSpans()
	require.Len(t, spans, 1, "only the call made once the SDK was installed")
	assert.Equal(t, "plg.call test/k.Do", spans[0].Name)

	assert.Nil(t, newTracing(nil, reporter).idle, "the SDK is no default")
	assert.Nil(t, newTracing(noop.NewTracerProvider(), reporter).tracer, "a no-op provider is off")
}
