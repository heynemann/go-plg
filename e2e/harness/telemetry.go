package harness

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Telemetry is an in-process OpenTelemetry SDK for the platform: spans exported, as each ends, to an in-memory
// exporter, and metrics read on demand through a manual reader.
type Telemetry struct {
	// Spans holds every span ended.
	Spans *tracetest.InMemoryExporter
	// TracerProvider is the platform's tracer provider.
	TracerProvider *sdktrace.TracerProvider
	// Reader reads the metrics.
	Reader *sdkmetric.ManualReader
	// MeterProvider is the platform's meter provider.
	MeterProvider *sdkmetric.MeterProvider
}

// NewTelemetry returns telemetry whose providers shut down when tb ends.
func NewTelemetry(tb testing.TB) *Telemetry {
	tb.Helper()

	spans := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	telemetry := &Telemetry{
		Spans:          spans,
		TracerProvider: sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans)),
		Reader:         reader,
		MeterProvider:  sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
	}

	tb.Cleanup(func() {
		ctx := context.WithoutCancel(tb.Context())
		require.NoError(tb, telemetry.TracerProvider.Shutdown(ctx))
		require.NoError(tb, telemetry.MeterProvider.Shutdown(ctx))
	})

	return telemetry
}

// Trace returns the ended spans of the trace id.
func (t *Telemetry) Trace(id trace.TraceID) tracetest.SpanStubs {
	return t.spans(func(span *tracetest.SpanStub) bool { return span.SpanContext.TraceID() == id })
}

// SpansNamed returns the ended spans of the name.
func (t *Telemetry) SpansNamed(name string) tracetest.SpanStubs {
	return t.spans(func(span *tracetest.SpanStub) bool { return span.Name == name })
}

// Metrics collects the metrics recorded so far.
func (t *Telemetry) Metrics(tb testing.TB) metricdata.ResourceMetrics {
	tb.Helper()

	var metrics metricdata.ResourceMetrics

	require.NoError(tb, t.Reader.Collect(tb.Context(), &metrics))

	return metrics
}

// Calls is a count of plugin calls (plg.plugin.calls) by what plg attributes it to.
type Calls struct {
	// Key is the plugin's key ("conditions/country").
	Key string
	// Method is the method called.
	Method string
	// Outcome is ok, error, conversion, panic or unavailable.
	Outcome string
	// Team, Org and Company are the plugin's owner's.
	Team, Org, Company string
}

// Calls returns plg's call counts (plg.plugin.calls) recorded so far.
func (t *Telemetry) Calls(tb testing.TB) map[Calls]int64 {
	tb.Helper()

	return t.Measured(tb, "plg.plugin.calls")
}

// Measured returns how many measurements plg's instrument of the name (one of the call instruments, with the
// attributes of Calls: plg.plugin.calls, plg.plugin.call.errors, plg.plugin.call.duration) recorded so far, by
// what they are attributed to: a counter's sum, a histogram's count.
func (t *Telemetry) Measured(tb testing.TB, name string) map[Calls]int64 {
	tb.Helper()

	counts := map[Calls]int64{}

	t.points(tb, name, func(attrs attribute.Set, n int64) { counts[callsOf(attrs)] += n })

	return counts
}

// Panics is a count of plg.PanicErrors (plg.plugin.panics) by what plg attributes it to.
type Panics struct {
	// Key is the plugin's key.
	Key string
	// Phase is where the panic happened (call, create, …).
	Phase string
	// Team, Org and Company are the plugin's owner's.
	Team, Org, Company string
}

// Panics returns plg's panic counts (plg.plugin.panics) recorded so far.
func (t *Telemetry) Panics(tb testing.TB) map[Panics]int64 {
	tb.Helper()

	counts := map[Panics]int64{}

	t.points(tb, "plg.plugin.panics", func(attrs attribute.Set, n int64) {
		counts[Panics{
			Key: value(attrs, "key"), Phase: value(attrs, "phase"),
			Team: value(attrs, "team"), Org: value(attrs, "org"), Company: value(attrs, "company"),
		}] += n
	})

	return counts
}

// points calls visit with each data point of plg's instrument of the name recorded so far: its attributes, and a
// counter's sum or a histogram's count.
func (t *Telemetry) points(tb testing.TB, name string, visit func(attrs attribute.Set, n int64)) {
	tb.Helper()

	for _, scope := range t.Metrics(tb).ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == name {
				visitPoints(tb, &metric, visit)
			}
		}
	}
}

// visitPoints calls visit with each data point of metric, a counter of int64 or a histogram of float64.
func visitPoints(tb testing.TB, metric *metricdata.Metrics, visit func(attrs attribute.Set, n int64)) {
	tb.Helper()

	switch data := metric.Data.(type) {
	case metricdata.Sum[int64]:
		for _, point := range data.DataPoints {
			visit(point.Attributes, point.Value)
		}
	case metricdata.Histogram[float64]:
		for _, point := range data.DataPoints {
			visit(point.Attributes, int64(point.Count)) //nolint:gosec // G115: a count of calls
		}
	default:
		require.Failf(tb, "unexpected instrument", "%s is a %T", metric.Name, metric.Data)
	}
}

// value returns the string attribute key's value; "" when absent.
func value(attrs attribute.Set, key attribute.Key) string {
	v, _ := attrs.Value(key)

	return v.AsString()
}

func callsOf(attrs attribute.Set) Calls {
	return Calls{
		Key: value(attrs, "key"), Method: value(attrs, "method"), Outcome: value(attrs, "outcome"),
		Team: value(attrs, "team"), Org: value(attrs, "org"), Company: value(attrs, "company"),
	}
}

// spans returns the ended spans keep keeps.
func (t *Telemetry) spans(keep func(*tracetest.SpanStub) bool) tracetest.SpanStubs {
	var spans tracetest.SpanStubs

	ended := t.Spans.GetSpans()
	for i := range ended {
		if keep(&ended[i]) {
			spans = append(spans, ended[i])
		}
	}

	return spans
}
