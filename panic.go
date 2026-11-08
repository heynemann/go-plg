package plg

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"
)

// Phase is the step of a plugin's life in which a [PanicError] happened.
type Phase string

// Phases of a [PanicError].
const (
	// PhaseLoad is the host loading a plugin: copying and checking its source and starting its first instances.
	PhaseLoad Phase = "load"
	// PhaseCreate is the guest loading the plugin's source and running its Create, in one guest step.
	PhaseCreate Phase = "create"
	// PhaseCall is a call to a plugin method, including the host encoding its arguments and decoding its
	// results.
	PhaseCall Phase = "call"
	// PhaseHost is the host serving a plugin's call to the Host interface.
	PhaseHost Phase = "host"
	// PhaseHTTP is the host serving a plugin's HTTP request.
	PhaseHTTP Phase = "http"
	// PhaseStorage is a [Storage] method (Name, List or Open).
	PhaseStorage Phase = "storage"
	// PhaseClose is an instance being closed, the plugin's optional Close included, or a plugin version that a
	// reload replaced or removed being closed.
	PhaseClose Phase = "close"
	// PhasePoll is a reload of a Register call's plugins, polled ([WithPollInterval]) or asked for
	// ([Container.Reload], [ReloadInterface]), outside its storage calls and plugin loads.
	PhasePoll Phase = "poll"
	// PhaseRefill is the background goroutine keeping a plugin's pool at its minimum.
	PhaseRefill Phase = "refill"
	// PhaseCallback is the user's code plg calls: the [WithOnPanic] function, the logger's handler or an
	// instrument of the [WithMeterProvider] provider (Method "OnPanic", "slog.Handler.Handle" or
	// "metric.MeterProvider").
	PhaseCallback Phase = "callback"
)

// PanicError reports a failure no plugin can avoid causing and no caller can prevent: a panic recovered on the
// host (in plg, a [Storage], the HTTP capability, a callback, …) or a guest that panicked, trapped (out of
// memory, stack overflow, a runtime abort), was stopped at its context's deadline or broke the guest ABI.
// The instance involved is discarded; the host and the plugin's other instances keep running.
//
// Every PanicError is logged at Error level, passed to the [WithOnPanic] function, kept in the container's
// [Container.RecentPanics] and counted toward the plugin its Key names ([PluginState.Panics]). The same value is
// shared by all of them and by the error returned, so treat it as read-only. Extract it with [errors.As]. It wraps
// [ErrCall] in [PhaseCall], and Value when Value is an error, so errors.Is(err, context.DeadlineExceeded) reports
// a call stopped at its deadline.
type PanicError struct {
	// Value is the recovered value, or the guest's panic message or trap.
	Value any
	// tags are the failing plugin version's, for its [EventPanicked] event's version and storage; nil when no
	// version is involved.
	tags *ownerTags
	// Time is when the failure was reported.
	Time time.Time
	// Key is the plugin key, "<name>/<descriptor key>", or the Register name when no plugin is involved (a
	// storage's List, polling); it is empty for the container's own calls to its meter provider ([New] creating
	// the instruments, [Container.Close] unregistering them) and to the logger's handler on records about no plugin
	// (a guest compiled, a cache warning).
	Key string
	// Language is the plugin's language, empty when no plugin is involved.
	Language Language
	// Phase is where the failure happened.
	Phase Phase
	// Method is the plugin, Host or Storage method running, if any.
	Method string
	// Guest is what the guest wrote to its stderr during the failing step (bounded to its last 32 KiB): the
	// interpreted source position of every frame a Go plugin's panic went through, or the Go runtime's fatal
	// error and traceback after a trap. It is empty for host-side failures, and for a guest stopped at its
	// deadline unless the plugin wrote something itself: the WebAssembly runtime reports no stack for a stop.
	Guest string
	// Stack is the host goroutine's stack for failures recovered on the host.
	Stack []byte
	// Owner is the owner of the plugin that failed (see [Owner]); it is zero when no plugin is involved, the
	// plugin has no owner, or the failure is in your own code (the logger's handler, the meter provider, the
	// WithOnPanic function).
	Owner Owner
}

// Error describes the failure: "plg: <key>: <phase> [<method>] failed: <value>".
func (e *PanicError) Error() string {
	var msg strings.Builder

	msg.WriteString("plg: ")
	msg.WriteString(e.Key)
	msg.WriteString(": ")
	msg.WriteString(string(e.Phase))

	if e.Method != "" {
		msg.WriteString(" ")
		msg.WriteString(e.Method)
	}

	msg.WriteString(" failed: ")
	fmt.Fprint(&msg, e.Value)

	return msg.String()
}

// Unwrap returns [ErrCall] for a failed call, and Value when it is an error.
func (e *PanicError) Unwrap() []error {
	var errs []error

	if e.Phase == PhaseCall {
		errs = append(errs, ErrCall)
	}

	if err, ok := e.Value.(error); ok {
		errs = append(errs, err)
	}

	return errs
}

// panicReporter reports a container's PanicErrors: an Error-level log, the panic history, the plg.plugin.panics
// metric and the WithOnPanic function. It holds the user's logger, which plg logs to only through it: the
// reports here, and every other record through a guarded logger ([panicReporter.guardedLogger]). It is safe for
// concurrent use.
type panicReporter struct {
	logger  *slog.Logger
	onPanic func(context.Context, *PanicError)
	meters  *instruments // nil counts nothing
	events  *eventQueue  // the container's events; nil emits nothing
	// tally counts each PanicError kept in the history toward the plugin it names ([Info]); nil counts nothing.
	// Set once, before the reporter is shared.
	tally   func(*PanicError)
	history ring[*PanicError]
	// unguarded makes the guarded loggers log straight to logger, without the callback boundary, so that tests
	// can prove that a panic escaping where plg logs leaves nothing behind.
	unguarded atomic.Bool
}

// newPanicReporter returns a reporter logging to logger, keeping the newest history PanicErrors (none for zero or
// less) and counting them in meters (nothing for nil).
func newPanicReporter(logger *slog.Logger, onPanic func(context.Context, *PanicError), history int,
	meters *instruments,
) *panicReporter {
	return &panicReporter{
		logger:  logger,
		onPanic: onPanic,
		meters:  meters,
		history: newRing[*PanicError](history),
	}
}

// recovered reports failure, a value recovered on the host: it sets failure's Value and records the stack of
// the calling goroutine as its Stack, so call it from the deferred function that recovered value.
func (r *panicReporter) recovered(ctx context.Context, failure *PanicError, value any) {
	failure.Value, failure.Stack = value, debug.Stack()
	r.report(ctx, failure)
}

// report stamps failure with the current time, records, counts and logs it, passes it to the WithOnPanic
// function and emits its [EventPanicked] event. The meter provider and the logger's handler are the user's code
// too.
func (r *panicReporter) report(ctx context.Context, failure *PanicError) {
	failure.Time = time.Now()
	r.keep(ctx, failure)

	if r.meters != nil {
		r.callback(ctx, failure.Key, failure.Language, meterMethod, func() { r.meters.countPanic(ctx, failure) })
	}

	r.callback(ctx, failure.Key, failure.Language, logMethod, func() { r.log(ctx, failure) })

	if r.onPanic != nil {
		r.callback(ctx, failure.Key, failure.Language, "OnPanic", func() { r.onPanic(ctx, failure) })
	}

	r.events.panicked(ctx, failure)
}

// callback runs fn, the user's code (named method) serving the plugin key in lang: a panic in it is recorded,
// counted and logged as a PanicError in [PhaseCallback], but not passed to the WithOnPanic function. A meter
// provider or a logger that panics cannot report its own failure, which the other still reports; a panic while
// reporting it is only recorded, so that a meter provider and a logger that both panic end there.
func (r *panicReporter) callback(ctx context.Context, key string, lang Language, method string, fn func()) {
	defer func() {
		if v := recover(); v != nil {
			failed := callbackFailure(key, lang, method, v)
			r.keep(ctx, failed)

			if method != meterMethod && r.meters != nil {
				r.quietly(ctx, failed, meterMethod, func() { r.meters.countPanic(ctx, failed) })
			}

			if method != logMethod {
				r.quietly(ctx, failed, logMethod, func() { r.log(ctx, failed) })
			}
		}
	}()

	fn()
}

// quietly runs fn, the user's code (named method) reporting failed: a panic in it is only recorded.
func (r *panicReporter) quietly(ctx context.Context, failed *PanicError, method string, fn func()) {
	defer func() {
		if v := recover(); v != nil {
			r.keep(ctx, callbackFailure(failed.Key, failed.Language, method, v))
		}
	}()

	fn()
}

// keep records failure in the history and, unless [Check] reported it, counts it toward the plugin it names.
func (r *panicReporter) keep(ctx context.Context, failure *PanicError) {
	r.history.record(failure)

	if r.tally != nil && ctx.Value(checkKey{}) == nil {
		r.tally(failure)
	}
}

// guardedLogger returns the user's logger with its handler behind the callback boundary, for records about the
// plugin key in lang: a panic in the handler (Enabled or Handle) is reported as a PanicError in [PhaseCallback]
// (Method slog.Handler.Handle) and the record is dropped; what was logging carries on. Every record plg logs, but
// the reports of PanicErrors themselves, goes through a guarded logger: the container's own, without a key (a
// panic on a record with a "key" attribute is reported under that key), and each plugin version's, which its
// instances log their output through, inside the guest's steps, and its pool its failures.
func (r *panicReporter) guardedLogger(key string, lang Language) *slog.Logger {
	return slog.New(&guardedHandler{handler: r.logger.Handler(), panics: r, key: key, lang: lang})
}

// pluginLogger is guardedLogger for the records about the plugin version id: each carries its owner's log group.
func (r *panicReporter) pluginLogger(id pluginID) *slog.Logger {
	logger := r.guardedLogger(id.key, id.lang)
	if len(id.owner.log.Value.Group()) > 0 {
		logger = slog.New(logger.Handler().WithAttrs([]slog.Attr{id.owner.log}))
	}

	return logger
}

// guardedHandler is a slog handler behind the callback boundary; see [panicReporter.guardedLogger].
type guardedHandler struct {
	handler slog.Handler
	panics  *panicReporter
	key     string
	lang    Language
}

func (h *guardedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.panics.unguarded.Load() {
		return h.handler.Enabled(ctx, level)
	}

	enabled := false

	h.panics.callback(ctx, h.key, h.lang, logMethod, func() { enabled = h.handler.Enabled(ctx, level) })

	return enabled
}

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (h *guardedHandler) Handle(ctx context.Context, rec slog.Record) error {
	if h.panics.unguarded.Load() {
		return h.handler.Handle(ctx, rec) //nolint:wrapcheck // the user's handler's own error, as slog returns it
	}

	key := h.key
	if key == "" {
		rec.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "key" {
				key = attr.Value.String()
			}

			return key == ""
		})
	}

	var err error

	h.panics.callback(ctx, key, h.lang, logMethod, func() { err = h.handler.Handle(ctx, rec) })

	return err //nolint:wrapcheck // the user's handler's own error, as slog returns it
}

func (h *guardedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &guardedHandler{handler: h.handler.WithAttrs(attrs), panics: h.panics, key: h.key, lang: h.lang}
}

func (h *guardedHandler) WithGroup(name string) slog.Handler {
	return &guardedHandler{handler: h.handler.WithGroup(name), panics: h.panics, key: h.key, lang: h.lang}
}

// callbackFailure returns the PanicError of a panic, v, in the user's code named method serving the plugin key in
// lang. Call it while the panic is being recovered, so that its Stack is where it happened.
func callbackFailure(key string, lang Language, method string, v any) *PanicError {
	return &PanicError{
		Key: key, Language: lang, Phase: PhaseCallback, Method: method, Value: v, Stack: debug.Stack(),
		Time: time.Now(),
	}
}

// Methods of the user's code that plg reports by name in the PanicError recorded when it panics.
const (
	logMethod     = "slog.Handler.Handle"  // the logger's handler
	meterMethod   = "metric.MeterProvider" // an instrument of the meter provider
	onEventMethod = "OnEvent"              // the WithOnEvent function
)

// log logs failure at Error level.
func (r *panicReporter) log(ctx context.Context, failure *PanicError) {
	attrs := []slog.Attr{
		slog.String("key", failure.Key), slog.String("phase", string(failure.Phase)),
		slog.String("value", fmt.Sprint(failure.Value)),
	}

	if failure.Language != "" {
		attrs = append(attrs, slog.String("language", string(failure.Language)))
	}

	if failure.Method != "" {
		attrs = append(attrs, slog.String("method", failure.Method))
	}

	if failure.Guest != "" {
		attrs = append(attrs, slog.String("guest", failure.Guest))
	}

	if failure.Stack != nil {
		attrs = append(attrs, slog.String("stack", string(failure.Stack)))
	}

	if owner := failure.Owner.logAttr(); len(owner.Value.Group()) > 0 {
		attrs = append(attrs, owner)
	}

	r.logger.LogAttrs(ctx, slog.LevelError, "plg: plugin failure", attrs...)
}

// RecentPanics returns the newest PanicErrors reported in the container, oldest first: at most the
// [WithPanicHistory] size (64 by default). Entries reported while it runs may be left out. It is safe for
// concurrent use; treat the returned values as read-only.
func (c *Container) RecentPanics() []*PanicError {
	return c.panics.history.recent()
}
