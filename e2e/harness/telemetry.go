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

	counts := map[Calls]int64{}

	for _, scope := range t.Metrics(tb).ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "plg.plugin.calls" {
				continue
			}

			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(tb, ok, "plg.plugin.calls is an int64 sum")

			for _, point := range sum.DataPoints {
				counts[callsOf(point.Attributes)] += point.Value
			}
		}
	}

	return counts
}

func callsOf(attrs attribute.Set) Calls {
	value := func(key attribute.Key) string {
		v, _ := attrs.Value(key)

		return v.AsString()
	}

	return Calls{
		Key: value("key"), Method: value("method"), Outcome: value("outcome"),
		Team: value("team"), Org: value("org"), Company: value("company"),
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
