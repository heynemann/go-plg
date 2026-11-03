package plg

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/heynemann/go-plg/internal/httpcap"
)

// meterName is the instrumentation scope of plg's instruments.
const meterName = "github.com/heynemann/go-plg"

// WithMeterProvider sets the OpenTelemetry meter provider the container's instruments come from. The default,
// also used for nil, is the global provider (otel.GetMeterProvider) as it is when [New] runs: a global provider
// installed later is picked up as the OpenTelemetry API delegates to it.
//
// Instruments (attributes in braces; key is the plugin key, "<name>/<descriptor key>", or the Register name
// where no plugin is involved):
//   - plg.plugin.calls {key, language, method, outcome}: calls to a plugin method, outcome being ok, error (the
//     plugin's own), conversion ([ErrConversion]), panic (a [PanicError]) or unavailable (no instance: the
//     call's context ended while waiting for one, the container closed);
//   - plg.plugin.call.duration {key, language, method, outcome}: their latency in seconds, waiting for an
//     instance included;
//   - plg.plugin.call.errors {key, language, method, outcome}: the calls whose outcome is not ok;
//   - plg.plugin.panics {key, phase}: every [PanicError] reported (guest panics and traps included);
//   - plg.pool.instances and plg.pool.instances.in_use {key}: a plugin's instances alive (or starting) and those
//     serving at least one call;
//   - plg.pool.calls.in_flight {key}: the calls its instances serve; more than in use only with
//     [WithConcurrentCalls], where an instance serves several calls at once;
//   - plg.pool.waits {key}: calls that found every instance busy and the pool at its maximum, and waited;
//   - plg.pool.refills {key, outcome}: instances the background refiller started (ok) or failed to (failed);
//   - plg.plugin.memory {key}: the linear memory of a plugin's live instances, in bytes;
//   - plg.plugin.loads {key, language, outcome}: plugin versions loaded (ok) or failing to (failed), by
//     Register and reloads;
//   - plg.plugin.reload.failures {key}: plugins a reload could not load (a new one skipped, an updated one kept
//     at its last good version), and reloads that could not list their storages (key is the Register name);
//   - plg.plugin.skipped {key, language}: listed plugins left out because the Register does not enable their
//     language, at each Register and reload;
//   - plg.http.requests {key, outcome[, status]}: the plugins' HTTP requests, outcome being ok (status is the
//     response's status code), refused (by the policy) or failed (in transit, timed out, too large a response);
//   - plg.http.duration {key, outcome}: their latency in seconds;
//   - plg.http.request.size and plg.http.response.size {key}: the bytes of the encoded requests the plugins
//     sent and of the encoded responses they received.
//
// The gauges (instances, in use, calls in flight, memory) cover the plugin versions the container serves; a
// version replaced by a reload is no longer observed while it drains. Measurements on the call path use attribute
// sets built once per plugin version, so recording them does not allocate beyond what the provider's SDK does. A
// panic in the provider's instruments is reported as a [PanicError] in [PhaseCallback] and never fails a call.
func WithMeterProvider(provider metric.MeterProvider) Option {
	return func(c *containerConfig) {
		c.meterProvider = provider
	}
}

// instruments are a container's OpenTelemetry instruments.
type instruments struct {
	calls, callErrors, panics, waits, refills, loads, reloadFailures, skipped metric.Int64Counter
	httpRequests, httpSent, httpReceived                                      metric.Int64Counter
	callDuration, httpDuration                                                metric.Float64Histogram
	instances, inUse, inFlight, memory                                        metric.Int64ObservableUpDownCounter
}

// durationBounds are the histogram boundaries of call and HTTP latencies, in seconds: from 10 µs (a Lua or
// TypeScript call) to 10 s (an HTTP request at its default timeout).
func durationBounds() []float64 {
	return []float64{
		0.00001, 0.000025, 0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25,
		0.5, 1, 2.5, 5, 10,
	}
}

// newInstruments creates the instruments from provider's meter. An instrument the provider fails to create is
// replaced by a no-op one; the failures are returned, joined.
func newInstruments(provider metric.MeterProvider) (*instruments, error) {
	meter := provider.Meter(meterName)
	inst := &instruments{}

	var errs []error

	counter := func(name, unit, desc string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))

		return orNoop[metric.Int64Counter](c, err, noop.Int64Counter{}, &errs)
	}
	histogram := func(name, desc string) metric.Float64Histogram {
		h, err := meter.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(desc),
			metric.WithExplicitBucketBoundaries(durationBounds()...))

		return orNoop[metric.Float64Histogram](h, err, noop.Float64Histogram{}, &errs)
	}
	gauge := func(name, unit, desc string) metric.Int64ObservableUpDownCounter {
		g, err := meter.Int64ObservableUpDownCounter(name, metric.WithUnit(unit), metric.WithDescription(desc))

		return orNoop[metric.Int64ObservableUpDownCounter](g, err, noop.Int64ObservableUpDownCounter{}, &errs)
	}

	inst.calls = counter("plg.plugin.calls", "{call}", "Calls to plugin methods.")
	inst.callErrors = counter("plg.plugin.call.errors", "{call}", "Calls to plugin methods that failed.")
	inst.callDuration = histogram("plg.plugin.call.duration", "Latency of calls to plugin methods.")
	inst.panics = counter("plg.plugin.panics", "{panic}", "Panics and traps reported as PanicErrors.")
	inst.waits = counter("plg.pool.waits", "{wait}", "Calls that waited for a busy pool at its maximum.")
	inst.refills = counter("plg.pool.refills", "{instance}", "Instances started by the pool refiller.")
	inst.loads = counter("plg.plugin.loads", "{load}", "Plugin versions loaded.")
	inst.reloadFailures = counter("plg.plugin.reload.failures", "{failure}", "Plugins a reload could not load.")
	inst.skipped = counter("plg.plugin.skipped", "{plugin}", "Plugins skipped: their language is not enabled.")
	inst.httpRequests = counter("plg.http.requests", "{request}", "HTTP requests made by plugins.")
	inst.httpSent = counter("plg.http.request.size", "By", "Bytes of the HTTP requests plugins sent.")
	inst.httpReceived = counter("plg.http.response.size", "By", "Bytes of the HTTP responses plugins received.")
	inst.httpDuration = histogram("plg.http.duration", "Latency of HTTP requests made by plugins.")
	inst.instances = gauge("plg.pool.instances", "{instance}", "Instances of a plugin alive or starting.")
	inst.inUse = gauge("plg.pool.instances.in_use", "{instance}", "Instances of a plugin serving a call.")
	inst.inFlight = gauge("plg.pool.calls.in_flight", "{call}", "Calls a plugin's instances are serving.")
	inst.memory = gauge("plg.plugin.memory", "By", "Linear memory of a plugin's live instances.")

	return inst, errors.Join(errs...)
}

// orNoop returns inst, or fallback when creating inst failed.
//
//nolint:ireturn // the instrument interfaces of the OpenTelemetry API are what the container holds
func orNoop[I any](inst I, err error, fallback I, errs *[]error) I {
	if err != nil {
		*errs = append(*errs, err)

		return fallback
	}

	return inst
}

// instrument creates the container's instruments from its configured provider, the global one by default, and
// registers the callback observing its pools. Failures are logged; the instruments concerned do nothing.
func (c *Container) instrument(cfg *containerConfig) {
	provider := cfg.meterProvider
	if provider == nil {
		provider = otel.GetMeterProvider()
	}

	var err, regErr error

	c.meters, err = newInstruments(provider)

	c.observation, regErr = provider.Meter(meterName).RegisterCallback(c.observe, c.meters.instances,
		c.meters.inUse, c.meters.inFlight, c.meters.memory)
	if regErr != nil {
		c.observation = noop.Registration{}
	}

	err = errors.Join(err, regErr)
	if err != nil {
		c.logger.LogAttrs(context.Background(), slog.LevelWarn, "plg: creating metric instruments failed",
			slog.String("error", err.Error()))
	}
}

// observe reports the gauges of every plugin version the container serves.
func (c *Container) observe(_ context.Context, obs metric.Observer) error {
	for _, gen := range c.catalogue.Load().plugins {
		gen.pool.observe(obs)
	}

	return nil
}

// observe reports the pool's gauges: its instances, those in use, the calls they serve and their memory.
func (p *pool) observe(obs metric.Observer) {
	inst, opts := p.meters.inst, p.meters.observe

	obs.ObserveInt64(inst.instances, int64(p.size.Load()), opts...)
	obs.ObserveInt64(inst.inUse, int64(p.inUse.Load()), opts...)
	obs.ObserveInt64(inst.inFlight, int64(p.busy.Load()), opts...)
	obs.ObserveInt64(inst.memory, p.memory.Load(), opts...)
}

// count adds one to counter with opts. A panic in the instrument is reported as a [PanicError] in
// [PhaseCallback].
func (p *pool) count(ctx context.Context, counter metric.Int64Counter, opts []metric.AddOption) {
	p.guard(ctx, meterMethod, func() { counter.Add(ctx, 1, opts...) })
}

// called records a call in the plugin's call metrics, as [pluginMeters.called] does; a panic in the instruments
// is reported as a [PanicError] in [PhaseCallback].
func (p *pool) called(ctx context.Context, index uint32, elapsed time.Duration, err error) {
	p.guard(ctx, meterMethod, func() { p.meters.called(ctx, index, elapsed, err) })
}

// countPanic counts a reported PanicError.
func (i *instruments) countPanic(ctx context.Context, pe *PanicError) {
	i.panics.Add(ctx, 1, metric.WithAttributes(attribute.String("key", pe.Key),
		attribute.String("phase", string(pe.Phase))))
}

// countLoad counts a plugin version loaded, or failing to load when err is not nil.
func (i *instruments) countLoad(ctx context.Context, key string, lang Language, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}

	i.loads.Add(ctx, 1, metric.WithAttributes(attribute.String("key", key),
		attribute.String("language", string(lang)), attribute.String("outcome", outcome)))
}

// countReloadFailure counts a plugin, or a whole reload (key being the Register name), a reload failed to load.
func (i *instruments) countReloadFailure(ctx context.Context, key string) {
	i.reloadFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("key", key)))
}

// countSkipped counts a listed plugin skipped for its language.
func (i *instruments) countSkipped(ctx context.Context, key string, lang Language) {
	i.skipped.Add(ctx, 1, metric.WithAttributes(attribute.String("key", key),
		attribute.String("language", string(lang))))
}

// callOutcome classifies how a call ended, for the call instruments.
type callOutcome uint8

const (
	callOK callOutcome = iota
	callError
	callConversion
	callPanic
	callUnavailable
	callOutcomes // the number of outcomes
)

// String is the outcome attribute's value.
func (o callOutcome) String() string {
	return [callOutcomes]string{"ok", "error", "conversion", "panic", "unavailable"}[o]
}

// outcomeOf classifies a call's error.
func outcomeOf(err error) callOutcome {
	switch {
	case err == nil:
		return callOK
	case holds[*PanicError](err):
		return callPanic
	case holds[*PluginError](err):
		return callError
	case errors.Is(err, ErrConversion):
		return callConversion
	default:
		return callUnavailable
	}
}

// holds reports whether err's chain holds an E, without allocating.
func holds[E error](err error) bool {
	_, ok := errors.AsType[E](err)

	return ok
}

// measure is one attribute set as the options of each kind of measurement.
type measure struct {
	add    []metric.AddOption
	record []metric.RecordOption
}

func measureOf(attrs ...attribute.KeyValue) measure {
	opt := metric.WithAttributeSet(attribute.NewSet(attrs...))

	return measure{add: []metric.AddOption{opt}, record: []metric.RecordOption{opt}}
}

// pluginMeters are the attribute sets of one plugin version's measurements, built when it is loaded so that
// recording them allocates nothing. It is immutable but for the cache of HTTP status codes, an xsync.Map.
type pluginMeters struct {
	keyAttr      attribute.KeyValue
	inst         *instruments
	statuses     *xsync.Map[int, []metric.AddOption] // {key, outcome=ok, status}, by status code
	key          []metric.AddOption                  // {key}
	observe      []metric.ObserveOption              // {key}
	refillOK     []metric.AddOption                  // {key, outcome=ok}
	refillFailed []metric.AddOption                  // {key, outcome=failed}
	calls        [][callOutcomes]measure             // {key, language, method, outcome}, by method index
	http         [3]measure                          // {key, outcome}, by httpcap.Outcome
}

// newPluginMeters returns the measurements of the version of plugin key in lang serving iface.
func newPluginMeters(inst *instruments, key string, lang Language, iface reflect.Type) *pluginMeters {
	keyAttr, langAttr := attribute.String("key", key), attribute.String("language", string(lang))
	meters := &pluginMeters{
		inst:         inst,
		keyAttr:      keyAttr,
		key:          measureOf(keyAttr).add,
		observe:      []metric.ObserveOption{metric.WithAttributeSet(attribute.NewSet(keyAttr))},
		refillOK:     measureOf(keyAttr, attribute.String("outcome", "ok")).add,
		refillFailed: measureOf(keyAttr, attribute.String("outcome", "failed")).add,
		calls:        make([][callOutcomes]measure, iface.NumMethod()),
		statuses:     xsync.NewMap[int, []metric.AddOption](),
	}

	for i := range meters.calls {
		methodAttr := attribute.String("method", iface.Method(i).Name)
		for o := range callOutcomes {
			meters.calls[i][o] = measureOf(keyAttr, langAttr, methodAttr, attribute.String("outcome", o.String()))
		}
	}

	for o, name := range []string{"ok", "refused", "failed"} {
		meters.http[o] = measureOf(keyAttr, attribute.String("outcome", name))
	}

	return meters
}

// called records a call to the method with the given index that took elapsed and failed with err, if not nil.
func (m *pluginMeters) called(ctx context.Context, index uint32, elapsed time.Duration, err error) {
	outcome := outcomeOf(err)
	opts := &m.calls[index][outcome]

	m.inst.calls.Add(ctx, 1, opts.add...)
	m.inst.callDuration.Record(ctx, elapsed.Seconds(), opts.record...)

	if outcome != callOK {
		m.inst.callErrors.Add(ctx, 1, opts.add...)
	}
}

// requested records an HTTP request of the plugin.
func (m *pluginMeters) requested(ctx context.Context, report httpcap.Report) {
	requests := m.http[report.Outcome].add
	if report.Outcome == httpcap.OutcomeOK {
		requests = m.status(report.Status)
	}

	m.inst.httpRequests.Add(ctx, 1, requests...)
	m.inst.httpDuration.Record(ctx, report.Duration.Seconds(), m.http[report.Outcome].record...)
	m.inst.httpSent.Add(ctx, int64(report.Sent), m.key...)

	if report.Received > 0 {
		m.inst.httpReceived.Add(ctx, int64(report.Received), m.key...)
	}
}

// status returns the attribute set of the plugin's requests answered with code, cached once built: the fast path
// is a lookup, without the closure that building takes.
func (m *pluginMeters) status(code int) []metric.AddOption {
	if opts, ok := m.statuses.Load(code); ok {
		return opts
	}

	opts, _ := m.statuses.LoadOrCompute(code, func() ([]metric.AddOption, bool) {
		return measureOf(m.keyAttr, attribute.String("outcome", "ok"), attribute.Int("status", code)).add, false
	})

	return opts
}
