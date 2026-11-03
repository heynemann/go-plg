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
	// PhaseCallback is a user callback such as the [WithOnPanic] function.
	PhaseCallback Phase = "callback"
)

// PanicError reports a failure no plugin can avoid causing and no caller can prevent: a panic recovered on the
// host (in plg, a [Storage], the HTTP capability, a callback, …) or a guest that panicked, trapped (out of
// memory, stack overflow, a runtime abort), was stopped at its context's deadline or broke the guest ABI.
// The instance involved is discarded; the host and the plugin's other instances keep running.
//
// Every PanicError is logged at Error level, passed to the [WithOnPanic] function and kept in the container's
// [Container.RecentPanics]. The same value is shared by all of them and by the error returned, so treat it as
// read-only. Extract it with [errors.As]. It wraps [ErrCall] in [PhaseCall], and Value when Value is an error,
// so errors.Is(err, context.DeadlineExceeded) reports a call stopped at its deadline.
type PanicError struct {
	// Value is the recovered value, or the guest's panic message or trap.
	Value any
	// Time is when the failure was reported.
	Time time.Time
	// Key is the plugin key, "<name>/<descriptor key>", or the Register name when no plugin is involved (a
	// storage's List, polling).
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
// metric and the WithOnPanic function. It is safe for concurrent use.
type panicReporter struct {
	logger  *slog.Logger
	onPanic func(context.Context, *PanicError)
	meters  *instruments
	history panicHistory
}

// newPanicReporter returns a reporter keeping the newest history PanicErrors (none for zero or less) and
// counting them in meters.
func newPanicReporter(logger *slog.Logger, onPanic func(context.Context, *PanicError), history int,
	meters *instruments,
) *panicReporter {
	return &panicReporter{
		logger:  logger,
		onPanic: onPanic,
		meters:  meters,
		history: panicHistory{ring: make([]atomic.Pointer[panicEntry], max(history, 0))},
	}
}

// recovered reports failure, a value recovered on the host: it sets failure's Value and records the stack of
// the calling goroutine as its Stack, so call it from the deferred function that recovered value.
func (r *panicReporter) recovered(ctx context.Context, failure *PanicError, value any) {
	failure.Value, failure.Stack = value, debug.Stack()
	r.report(ctx, failure)
}

// report stamps failure with the current time, records and logs it and passes it to the WithOnPanic function.
func (r *panicReporter) report(ctx context.Context, failure *PanicError) {
	failure.Time = time.Now()
	r.history.record(failure)
	r.meters.countPanic(ctx, failure)
	r.log(ctx, failure)

	if r.onPanic != nil {
		r.callback(ctx, failure, "OnPanic", func() { r.onPanic(ctx, failure) })
	}
}

// callback runs fn, the user's code serving failure (named method): a panic in it is recorded as a
// PanicError in [PhaseCallback] for failure's plugin and logged, but not passed to the WithOnPanic function.
func (r *panicReporter) callback(ctx context.Context, failure *PanicError, method string, fn func()) {
	defer func() {
		if v := recover(); v != nil {
			failed := &PanicError{
				Key: failure.Key, Language: failure.Language, Phase: PhaseCallback, Method: method, Value: v,
				Stack: debug.Stack(), Time: time.Now(),
			}
			r.history.record(failed)
			r.meters.countPanic(ctx, failed)

			if method != logMethod {
				r.log(ctx, failed)
			}
		}
	}()

	fn()
}

// logMethod names the logger's handler in the PanicError recorded when it panics: that logger cannot log its
// own failure, which is only recorded.
const logMethod = "slog.Handler.Handle"

// log logs failure at Error level. The logger's handler is the user's code too.
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

	r.callback(ctx, failure, logMethod, func() {
		r.logger.LogAttrs(ctx, slog.LevelError, "plg: plugin failure", attrs...)
	})
}

// panicHistory keeps the newest PanicErrors in a ring, without locks: a writer takes the next sequence number
// and stores its entry in that number's slot, unless a newer entry holds it already; a reader keeps only the
// entries whose slot still holds the sequence number it expects, so entries overwritten or not yet stored
// meanwhile are left out.
type panicHistory struct {
	ring []atomic.Pointer[panicEntry]
	next atomic.Uint64 // sequence number of the next entry
}

type panicEntry struct {
	pe  *PanicError
	seq uint64
}

func (h *panicHistory) record(pe *PanicError) {
	if len(h.ring) == 0 {
		return
	}

	h.store(h.next.Add(1)-1, pe)
}

// store puts pe, numbered seq, in its slot. A writer delayed between taking its number and storing may find
// the slot taken by an entry recorded after its own, one ring length later: it leaves that newer entry in place
// (its own is out of the history already), so that the history holds the newest entries once writers are done.
func (h *panicHistory) store(seq uint64, pe *PanicError) {
	slot, entry := &h.ring[seq%uint64(len(h.ring))], &panicEntry{pe: pe, seq: seq}

	for {
		held := slot.Load()
		if held != nil && held.seq > seq {
			return
		}

		if slot.CompareAndSwap(held, entry) {
			return
		}
	}
}

// recent returns the newest entries, oldest first.
func (h *panicHistory) recent() []*PanicError {
	size := uint64(len(h.ring))
	end := h.next.Load()

	if size == 0 || end == 0 {
		return nil
	}

	start := end - min(end, size)
	out := make([]*PanicError, 0, end-start)

	for seq := start; seq < end; seq++ {
		entry := h.ring[seq%size].Load()
		if entry != nil && entry.seq == seq {
			out = append(out, entry.pe)
		}
	}

	return out
}

// RecentPanics returns the newest PanicErrors reported in the container, oldest first: at most the
// [WithPanicHistory] size (64 by default). Entries reported while it runs may be left out. It is safe for
// concurrent use; treat the returned values as read-only.
func (c *Container) RecentPanics() []*PanicError {
	return c.panics.history.recent()
}
