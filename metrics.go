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
// where no plugin is involved). Every instrument with a key also has the attributes team, org and company: the
// plugin's [Owner] Team.Name, Org and Company, empty when not set or when no plugin is involved. People's names,
// addresses and labels never become attributes:
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
//   - plg.plugin.validations {key, language, trigger, outcome}: validations ([WithValidation]) run at a load
//     (trigger load), by [Check] (trigger check) or on demand ([Validate], [ValidateAll]; trigger on_demand),
//     outcome being passed, failed (an issue of severity error, or Validate failed: see [WithValidation]) or
//     unavailable (it could not run: its context ended, the container closed, or beyond the reservations no
//     budget room);
//   - plg.plugin.validation.issues {key, language, trigger, severity}: the issues the validations that ran
//     found, severity being error (any severity plg does not know included), warning or info;
//   - plg.http.requests {key, outcome[, status]}: the plugins' HTTP requests, outcome being ok (status is the
//     response's status code), refused (by the policy) or failed (in transit, timed out, too large a response);
//   - plg.http.duration {key, outcome}: their latency in seconds;
//   - plg.http.request.size and plg.http.response.size {key}: the bytes of the encoded requests the plugins
//     sent and of the encoded responses they received;
//   - plg.events.dropped {key, kind}: [WithOnEvent] events dropped because they found the queue full, kind
//     being the [EventKind].
//
// The gauges (instances, in use, calls in flight, memory) cover the plugin versions the container serves; a
// version replaced by a reload is no longer observed while it drains. Measurements on the call path use attribute
// sets built once per plugin version, so recording them does not allocate beyond what the provider's SDK does. A
// panic in the provider (creating an instrument, recording or observing a measurement, registering or
// unregistering the gauges' callback) is reported as a [PanicError] in [PhaseCallback] and never fails New,
// Register, a reload, a call or Close; the instruments it left uncreated do nothing.
func WithMeterProvider(provider metric.MeterProvider) Option {
	return func(c *containerConfig) {
		c.meterProvider = provider
	}
}

// instruments are a container's OpenTelemetry instruments.
type instruments struct {
	calls, callErrors, panics, waits, refills, loads, reloadFailures, skipped metric.Int64Counter
	validations, validationIssues, eventsDropped                              metric.Int64Counter
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
	inst.validations = counter("plg.plugin.validations", "{validation}", "Plugin validations run.")
	inst.validationIssues = counter("plg.plugin.validation.issues", "{issue}", "Issues plugin validations found.")
	inst.httpRequests = counter("plg.http.requests", "{request}", "HTTP requests made by plugins.")
	inst.httpSent = counter("plg.http.request.size", "By", "Bytes of the HTTP requests plugins sent.")
	inst.httpReceived = counter("plg.http.response.size", "By", "Bytes of the HTTP responses plugins received.")
	inst.httpDuration = histogram("plg.http.duration", "Latency of HTTP requests made by plugins.")
	inst.eventsDropped = counter("plg.events.dropped", "{event}", "Events dropped: the WithOnEvent queue was full.")
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
// registers the callback observing its pools, then has the panic reporter count panics with them. Failures are
// logged; the instruments concerned do nothing. A panic in the provider meanwhile is reported as a [PanicError]
// in [PhaseCallback], and the instruments it left uncreated do nothing too.
func (c *Container) instrument(cfg *containerConfig) {
	provider := cfg.meterProvider
	if provider == nil {
		provider = otel.GetMeterProvider()
	}

	var err, regErr error

	c.panics.callback(context.Background(), "", "", meterMethod, func() {
		c.meters, err = newInstruments(provider)
		c.observation, regErr = provider.Meter(meterName).RegisterCallback(c.observe, c.meters.instances,
			c.meters.inUse, c.meters.inFlight, c.meters.memory)
	})

	if c.meters == nil { // the provider panicked creating them: no-op instruments, which never fail
		c.meters, err = newInstruments(noop.NewMeterProvider())
	}

	if regErr != nil || c.observation == nil { // failed, or panicked
		c.observation = noop.Registration{}
	}

	c.panics.meters = c.meters

	err = errors.Join(err, regErr)
	if err != nil {
		c.logger.LogAttrs(context.Background(), slog.LevelWarn, "plg: creating metric instruments failed",
			slog.String("error", err.Error()))
	}
}

// unobserve unregisters the callback observing the pools. A panic in the provider is reported as a [PanicError]
// in [PhaseCallback].
func (c *Container) unobserve(ctx context.Context) error {
	var err error

	c.panics.callback(ctx, "", "", meterMethod, func() { err = c.observation.Unregister() })

	return err //nolint:wrapcheck // Close wraps every failure it returns
}

// observe reports the gauges of every plugin version the container serves.
func (c *Container) observe(ctx context.Context, obs metric.Observer) error {
	for _, gen := range c.catalogue.Load().plugins {
		gen.pool.observe(ctx, obs)
	}

	return nil
}

// observe reports the pool's gauges: its instances, those in use, the calls they serve and their memory. A panic
// in the observer is reported as a [PanicError] in [PhaseCallback].
func (p *pool) observe(ctx context.Context, obs metric.Observer) {
	inst, opts := p.meters.inst, p.meters.observe

	p.measure(ctx, func() {
		obs.ObserveInt64(inst.instances, int64(p.size.Load()), opts...)
		obs.ObserveInt64(inst.inUse, int64(p.inUse.Load()), opts...)
		obs.ObserveInt64(inst.inFlight, int64(p.busy.Load()), opts...)
		obs.ObserveInt64(inst.memory, p.memory.Load(), opts...)
	})
}

// count adds one to counter with opts. A panic in the instrument is reported as a [PanicError] in
// [PhaseCallback].
func (p *pool) count(ctx context.Context, counter metric.Int64Counter, opts []metric.AddOption) {
	p.measure(ctx, func() { counter.Add(ctx, 1, opts...) })
}

// called records a call in the plugin's call metrics, as [pluginMeters.called] does; a panic in the instruments
// is reported as a [PanicError] in [PhaseCallback].
func (p *pool) called(ctx context.Context, index uint32, elapsed time.Duration, err error) {
	p.measure(ctx, func() { p.meters.called(ctx, index, elapsed, err) })
}

// countPanic counts a reported PanicError; the panic reporter guards it.
func (i *instruments) countPanic(ctx context.Context, pe *PanicError) {
	owner := pe.Owner.metricAttrs()
	i.panics.Add(ctx, 1, metric.WithAttributes(attribute.String("key", pe.Key),
		attribute.String("phase", string(pe.Phase)), owner[0], owner[1], owner[2]))
}

// The lifecycle measurements below report a panic in their instrument as a [PanicError] in [PhaseCallback], as
// every measurement does, so that it cannot fail a Register or a reload round. Take them once what they count
// has an owner that cleans it up: a version counted as loaded is already the Register's, or its round's, to
// close.

// countLoad counts a version of the plugin key, owned by owner, loaded, or failing to load when err is not nil.
func (c *Container) countLoad(ctx context.Context, key string, lang Language, owner *Owner, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}

	tags := owner.metricAttrs()

	c.panics.callback(ctx, key, lang, meterMethod, func() {
		c.meters.loads.Add(ctx, 1, metric.WithAttributes(attribute.String("key", key),
			attribute.String("language", string(lang)), attribute.String("outcome", outcome), tags[0], tags[1],
			tags[2]))
	})
}

// countReloadFailure counts a plugin, owned by owner, or a whole reload (key being the Register name, owner an
// empty one), a reload failed to load.
func (c *Container) countReloadFailure(ctx context.Context, key string, owner *Owner) {
	tags := owner.metricAttrs()

	c.panics.callback(ctx, key, "", meterMethod, func() {
		c.meters.reloadFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("key", key), tags[0], tags[1],
			tags[2]))
	})
}

// countSkipped counts a listed plugin, owned by owner, skipped for its language.
func (c *Container) countSkipped(ctx context.Context, key string, lang Language, owner *Owner) {
	tags := owner.metricAttrs()

	c.panics.callback(ctx, key, lang, meterMethod, func() {
		c.meters.skipped.Add(ctx, 1, metric.WithAttributes(attribute.String("key", key),
			attribute.String("language", string(lang)), tags[0], tags[1], tags[2]))
	})
}

// The triggers of a validation, its metrics' trigger attribute.
const (
	triggerLoad     = "load"
	triggerOnDemand = "on_demand"
	triggerCheck    = "check"
)

// The outcomes of a validation, its metrics' outcome attribute.
const (
	validationPassed      = "passed"
	validationFailed      = "failed"
	validationUnavailable = "unavailable"
)

// validationOutcome classifies a validation that ended with err, run under ctx: unavailable when it could not
// run (ctx ended, the container closed, no budget room), else failed or passed.
func (c *Container) validationOutcome(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return validationPassed
	case ctx.Err() != nil || c.closed.Load() || errors.Is(err, ErrMemoryBudget):
		return validationUnavailable
	default:
		return validationFailed
	}
}

// countValidation counts a validation run with the given trigger for launch's plugin version, which ended with
// err, and the issues it found by severity (none for one that could not run); it returns the validation's
// outcome. It counts nothing when reg does not validate its plugins.
func (c *Container) countValidation(ctx context.Context, reg *registration, launch *launcher, trigger string,
	err error, issues []Issue,
) string {
	if reg.validation == nil {
		return ""
	}

	outcome := c.validationOutcome(ctx, err)

	var bySeverity [3]int64 // error, warning, info

	if outcome != validationUnavailable {
		for _, issue := range issues {
			switch {
			case issue.Severity.blocks():
				bySeverity[0]++
			case issue.Severity == SeverityWarning:
				bySeverity[1]++
			default:
				bySeverity[2]++
			}
		}
	}

	tags := launch.owner.metrics
	key, lang := attribute.String("key", launch.key), attribute.String("language", string(launch.lang))
	when := attribute.String("trigger", trigger)

	c.panics.callback(ctx, launch.key, launch.lang, meterMethod, func() {
		c.meters.validations.Add(ctx, 1, metric.WithAttributes(key, lang, when,
			attribute.String("outcome", outcome), tags[0], tags[1], tags[2]))

		for i, severity := range [...]Severity{SeverityError, SeverityWarning, SeverityInfo} {
			if bySeverity[i] > 0 {
				c.meters.validationIssues.Add(ctx, bySeverity[i], metric.WithAttributes(key, lang, when,
					attribute.String("severity", string(severity)), tags[0], tags[1], tags[2]))
			}
		}
	})

	return outcome
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
	inst         *instruments
	plugin       []attribute.KeyValue                // {key, team, org, company}, in every set below
	statuses     *xsync.Map[int, []metric.AddOption] // {key, outcome=ok, status}, by status code
	key          []metric.AddOption                  // {key}
	observe      []metric.ObserveOption              // {key}
	refillOK     []metric.AddOption                  // {key, outcome=ok}
	refillFailed []metric.AddOption                  // {key, outcome=failed}
	calls        [][callOutcomes]measure             // {key, language, method, outcome}, by method index
	http         [3]measure                          // {key, outcome}, by httpcap.Outcome
}

// newPluginMeters returns the measurements of the version of plugin key in lang serving iface.
func newPluginMeters(inst *instruments, key string, lang Language, owner *ownerTags, iface reflect.Type,
) *pluginMeters {
	keyAttr, langAttr := attribute.String("key", key), attribute.String("language", string(lang))
	plugin := []attribute.KeyValue{keyAttr, owner.metrics[0], owner.metrics[1], owner.metrics[2]}
	with := func(attrs ...attribute.KeyValue) measure { return measureOf(append(attrs, plugin...)...) }

	meters := &pluginMeters{
		inst:         inst,
		plugin:       plugin,
		key:          with().add,
		observe:      []metric.ObserveOption{metric.WithAttributeSet(attribute.NewSet(plugin...))},
		refillOK:     with(attribute.String("outcome", "ok")).add,
		refillFailed: with(attribute.String("outcome", "failed")).add,
		calls:        make([][callOutcomes]measure, iface.NumMethod()),
		statuses:     xsync.NewMap[int, []metric.AddOption](),
	}

	for i := range meters.calls {
		methodAttr := attribute.String("method", iface.Method(i).Name)
		for o := range callOutcomes {
			meters.calls[i][o] = with(langAttr, methodAttr, attribute.String("outcome", o.String()))
		}
	}

	for o, name := range httpOutcomes {
		meters.http[o] = with(attribute.String("outcome", name))
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

// httpOutcomes are the outcome attribute values of an HTTP request's measurements and span, by httpcap.Outcome.
var httpOutcomes = [...]string{"ok", "refused", "failed"} //nolint:gochecknoglobals // immutable attribute values

// requested records an HTTP request of the plugin.
func (m *pluginMeters) requested(ctx context.Context, report *httpcap.Report) {
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
		attrs := append([]attribute.KeyValue{attribute.String("outcome", "ok"), attribute.Int("status", code)},
			m.plugin...)

		return measureOf(attrs...).add, false
	})

	return opts
}
