package plg

import (
	"context"
	"net/http"
	"reflect"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/heynemann/go-plg/internal/httpcap"
	"github.com/heynemann/go-plg/internal/wire"
)

// WithTracerProvider sets the OpenTelemetry tracer provider the container's spans come from. The default, also
// used for nil, is the global provider (otel.GetTracerProvider) as it is when [New] runs; while that is still the
// OpenTelemetry API's own default, calls trace nothing, and once an SDK is installed as the global provider the
// container's spans go to it.
//
// Spans (instrumentation scope github.com/heynemann/go-plg), each a child of the span in the context it starts
// from, so a plugin call joins the trace of the request that made it:
//   - plg.call <key>.<method>, one per call to a plugin method, from the call's context, waiting for an instance
//     included; attributes key (the plugin key, "<name>/<descriptor key>"), version, language, method, outcome
//     (as plg.plugin.calls's, see [WithMeterProvider]) and the owner's team, org and company ([Owner] Team.Name,
//     Org and Company, empty when not set); a call whose outcome is not ok has the error status, with the
//     call's error as its description and an exception event;
//   - plg.host <Host interface>.<method>, a child of the call's span, for each [WithHost] method the plugin
//     calls (the Host method's context carries it); attributes as the call's, method being the Host method, and
//     outcome ok, error (the method returned one), conversion (its results could not be converted) or panic,
//     with the error status unless ok;
//   - plg.http <HTTP method>, of kind client, a child of the call's span, for each HTTP request the plugin
//     makes ([WithHTTP]), refused requests included; attributes key, version, language, team, org, company,
//     http.request.method, url.full (scheme, host and path: never credentials or the query), outcome (ok,
//     refused or failed, as plg.http.requests's) and http.response.status_code once answered; a request
//     refused, failed or answered with a status of 400 or more has the error status.
//
// Every request a plugin makes carries the W3C traceparent (and tracestate) header of the span in its context:
// its plg.http span, or without tracing the caller's span, if any; they replace the plugin's own. The plugin's
// own output ("plg: guest output" records, see [WithLogger]) is logged with the context of the step that wrote it
// and, when that context holds a span, with the attributes trace_id and span_id (the call's span).
//
// A no-op provider (go.opentelemetry.io/otel/trace/noop, or the deprecated trace.NewNoopTracerProvider) traces
// nothing at no cost: a call allocates nothing for tracing. A panic in the provider (creating the tracer, starting
// or ending a span, setting its attributes) is reported as a [PanicError] in [PhaseCallback] (Method
// trace.TracerProvider) and never fails New, a call, a Host method or a request; a span whose start panicked is
// left out.
func WithTracerProvider(provider trace.TracerProvider) Option {
	return func(c *containerConfig) {
		c.tracerProvider = provider
	}
}

// traceMethod names the tracer provider in the PanicErrors of its panics.
const traceMethod = "trace.TracerProvider"

// tracing is a container's tracer. It is immutable.
type tracing struct {
	tracer trace.Tracer // nil: tracing is off (a no-op provider, or one that panicked creating it)
	// idle is the OpenTelemetry API's default global provider, when New found it as the global one: until the
	// global provider is replaced (an SDK installed), tracer, which delegates to the replacement, could only
	// make non-recording spans, so calls trace nothing. Nil when the provider is any other.
	idle   trace.TracerProvider
	panics *panicReporter
}

// The types of tracer providers that matter to newTracing: the OpenTelemetry API's default global provider, which
// delegates to the provider installed later, and its deprecated no-op one (trace.NewNoopTracerProvider).
const (
	apiDefaultProvider     = "go.opentelemetry.io/otel/internal/global.tracerProvider"
	deprecatedNoopProvider = "go.opentelemetry.io/otel/trace.noopTracerProvider"
)

// newTracing returns the tracing of the provider given to [WithTracerProvider], the global one for nil. A panic
// creating the tracer is reported and leaves tracing off.
func newTracing(provider trace.TracerProvider, panics *panicReporter) *tracing {
	if provider == nil {
		provider = otel.GetTracerProvider()
	}

	traced := &tracing{panics: panics}

	switch typeName(provider) {
	case typeName(noop.TracerProvider{}), deprecatedNoopProvider:
		return traced
	case apiDefaultProvider:
		traced.idle = provider
	default:
	}

	panics.callback(context.Background(), "", "", traceMethod, func() { traced.tracer = provider.Tracer(meterName) })

	return traced
}

// typeName returns the package path and name of v's type, a pointer's element type for a pointer.
func typeName(v any) string {
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	return typ.PkgPath() + "." + typ.Name()
}

// on reports whether calls are traced: tracing is not off, and the API's default global provider found by New,
// if it was, has been replaced since.
func (t *tracing) on() bool {
	return t.tracer != nil && (t.idle == nil || otel.GetTracerProvider() != t.idle)
}

// start starts a span named name with opts, a child of the span in ctx, and returns ctx holding it; a panic in
// the provider is reported under the plugin key in lang, and the span is left out (nil).
//
//nolint:ireturn // the span is the OpenTelemetry API's interface
func (t *tracing) start(ctx context.Context, key string, lang Language, name string, opts []trace.SpanStartOption,
) (context.Context, trace.Span) {
	started, span := ctx, trace.Span(nil)

	t.panics.callback(ctx, key, lang, traceMethod, func() {
		started, span = t.tracer.Start(ctx, name, opts...) //nolint:fatcontext // runs once: started is the result
	})

	if span == nil {
		return ctx, nil
	}

	return started, span
}

// end runs fn, setting the span's last attributes and status, then ends span; a panic in the provider is
// reported under the plugin key in lang.
func (t *tracing) end(ctx context.Context, key string, lang Language, span trace.Span, fn func()) {
	t.panics.callback(ctx, key, lang, traceMethod, func() {
		fn()
		span.End()
	})
}

// spanStart is how one kind of span starts: its name and options (attributes, kind), built once.
type spanStart struct {
	name string
	opts []trace.SpanStartOption
}

// pluginSpans are a plugin version's span names and attributes, built when it loads so that starting a span
// builds neither. It is immutable; a nil *pluginSpans traces nothing.
type pluginSpans struct {
	tracing *tracing
	key     string
	lang    Language
	calls   []spanStart // by method index
	hosts   []spanStart // by Host method index; empty without WithHost
	http    []trace.SpanStartOption
}

// newPluginSpans returns the spans of the version of plugin key in lang, owned by owner, serving iface with
// the Host hst (nil without one).
func newPluginSpans(tracing *tracing, key string, lang Language, owner *ownerTags, iface reflect.Type, hst *host,
) *pluginSpans {
	plugin := []attribute.KeyValue{
		attribute.String("key", key), attribute.String("version", owner.version),
		attribute.String("language", string(lang)), owner.metrics[0], owner.metrics[1], owner.metrics[2],
	}
	with := func(name, method string) spanStart {
		attrs := append([]attribute.KeyValue{attribute.String("method", method)}, plugin...)

		return spanStart{name: name, opts: []trace.SpanStartOption{trace.WithAttributes(attrs...)}}
	}

	spans := &pluginSpans{
		tracing: tracing,
		key:     key,
		lang:    lang,
		calls:   make([]spanStart, iface.NumMethod()),
		http:    []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(plugin...)},
	}

	for i := range spans.calls {
		method := iface.Method(i).Name
		spans.calls[i] = with("plg.call "+key+"."+method, method)
	}

	if hst != nil {
		spans.hosts = make([]spanStart, len(hst.methods))

		for i, meth := range hst.methods {
			method := hst.iface.String() + "." + meth.name
			spans.hosts[i] = with("plg.host "+method, method)
		}
	}

	return spans
}

// active reports whether the version's spans are traced.
func (s *pluginSpans) active() bool {
	return s != nil && s.tracing.on()
}

// call starts the span of a call to the method with the given index, returning ctx holding it, or ctx and nil
// when not tracing.
//
//nolint:ireturn // the span is the OpenTelemetry API's interface
func (s *pluginSpans) call(ctx context.Context, index uint32) (context.Context, trace.Span) {
	if !s.active() {
		return ctx, nil
	}

	start := &s.calls[index]

	return s.tracing.start(ctx, s.key, s.lang, start.name, start.opts)
}

// called ends a call's span (nil: none), which ended with err.
func (s *pluginSpans) called(ctx context.Context, span trace.Span, err error) {
	if span == nil {
		return
	}

	s.tracing.end(ctx, s.key, s.lang, span, func() {
		span.SetAttributes(attribute.String("outcome", outcomeOf(err).String()))

		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
		}
	})
}

// host starts the span of a call to the Host method with the given index, as call does.
//
//nolint:ireturn // the span is the OpenTelemetry API's interface
func (s *pluginSpans) host(ctx context.Context, index uint32) (context.Context, trace.Span) {
	if !s.active() || int(index) >= len(s.hosts) {
		return ctx, nil
	}

	start := &s.hosts[index]

	return s.tracing.start(ctx, s.key, s.lang, start.name, start.opts)
}

// hostAnswered ends a Host method's span (nil: none) from its status-prefixed answer, or the failure to serve it.
func (s *pluginSpans) hostAnswered(ctx context.Context, span trace.Span, answer []byte, err error) {
	if span == nil {
		return
	}

	s.tracing.end(ctx, s.key, s.lang, span, func() {
		got := hostOutcome(answer, err)
		span.SetAttributes(attribute.String("outcome", got.outcome.String()))

		if got.outcome != callOK {
			span.SetStatus(codes.Error, got.message)
		}
	})
}

// hostResult is how a Host method call ended, and its message unless it succeeded.
type hostResult struct {
	message string
	outcome callOutcome
}

// hostOutcome classifies a Host method's answer, or the failure to serve it (err).
func hostOutcome(answer []byte, err error) hostResult {
	if err != nil {
		return hostResult{outcome: callError, message: err.Error()}
	}

	if len(answer) == 0 {
		return hostResult{outcome: callError, message: "empty answer"}
	}

	var got hostResult

	switch wire.Status(answer[0]) { //nolint:exhaustive // the host answers with these statuses only
	case wire.StatusOK:
		return hostResult{outcome: callOK}
	case wire.StatusConvert:
		got.outcome = callConversion
	case wire.StatusPanic:
		got.outcome = callPanic
	default:
		got.outcome = callError
	}

	reader := wire.NewReader(answer[1:])

	got.message, err = reader.Text()
	if err != nil { // the host encodes every message it answers with
		got.message = "undecodable message: " + err.Error()
	}

	return got
}

// httpRequest starts the span of an HTTP request (see [httpcap.Hooks]): it returns ctx holding it and the
// function ending it, or ctx and nil when not tracing.
func (s *pluginSpans) httpRequest(ctx context.Context) (context.Context, httpcap.SpanEnd) {
	if !s.active() {
		return ctx, nil
	}

	ctx, span := s.tracing.start(ctx, s.key, s.lang, "plg.http", s.http)
	if span == nil {
		return ctx, nil
	}

	return ctx, func(report httpcap.Report) { s.httpAnswered(ctx, span, &report) }
}

// httpAnswered ends an HTTP request's span with its report.
func (s *pluginSpans) httpAnswered(ctx context.Context, span trace.Span, report *httpcap.Report) {
	s.tracing.end(ctx, s.key, s.lang, span, func() {
		if report.Method != "" {
			span.SetName("plg.http " + report.Method)
			span.SetAttributes(attribute.String("http.request.method", report.Method))
		}

		if report.URL != "" {
			span.SetAttributes(attribute.String("url.full", report.URL))
		}

		span.SetAttributes(attribute.String("outcome", httpOutcomes[report.Outcome]))

		switch {
		case report.Err != nil:
			span.SetStatus(codes.Error, report.Err.Error())

		case report.Status >= http.StatusBadRequest: // client and server errors are a client span's errors
			span.SetStatus(codes.Error, "")

		default: // answered below 400: unset
		}

		if report.Outcome == httpcap.OutcomeOK {
			span.SetAttributes(attribute.Int("http.response.status_code", report.Status))
		}
	})
}
