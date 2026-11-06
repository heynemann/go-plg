package plg

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// EventKind says what happened to a plugin in an [Event].
type EventKind string

// The kinds of [Event].
const (
	// EventLoaded is a plugin version published: by Register, or by a reload that loaded a new plugin or a new
	// version. Its Report is the load's.
	EventLoaded EventKind = "loaded"
	// EventRejected is a plugin version that cannot serve: a load that failed (at Register, which then fails, or
	// on a reload, which keeps the last good version if any), an issue that blocked it (ownership included, with
	// [WithRequiredOwnership]), a failed validation, a listed plugin a reload cannot load (an invalid descriptor,
	// a key listed twice, a language whose guest is not linked), or an on-demand validation that unpublished it
	// ([Unpublish]). Its Report says why in Err.
	EventRejected EventKind = "rejected"
	// EventRetired is a plugin version no longer served: a reload replaced or removed it, an on-demand validation
	// unpublished it, or [Container.Close] closed the container. Its calls in flight finish on it.
	EventRetired EventKind = "retired"
	// EventPanicked is a [PanicError] reported, as [WithOnPanic] gets it: its Panic is the same value.
	EventPanicked EventKind = "panicked"
	// EventValidated is an on-demand validation that ran ([Validate], [ValidateAll]); its Report is the
	// validation's, its Err set when it failed. A validation that could not run, or that finished after one
	// started later, emits nothing.
	EventValidated EventKind = "validated"
	// EventSkipped is a listed plugin left out because the Register does not enable its language, at Register and
	// at each reload.
	EventSkipped EventKind = "skipped"
)

// Event is something that happened to a plugin, passed to the [WithOnEvent] function. Its Report and Panic are
// shared with the container (the reports it keeps, [Container.RecentPanics]): treat them as read-only.
type Event struct {
	// At is when it happened.
	At time.Time
	// Report is the load's (Loaded, Rejected) or the on-demand validation's (Validated, and Rejected when the
	// validation unpublished the version); nil for Retired, Panicked and Skipped.
	Report *Report
	// Panic is the reported failure (Panicked); nil for the other kinds.
	Panic *PanicError
	// Kind says what happened.
	Kind EventKind
	// Key is the plugin's key, "<name>/<descriptor key>"; for a panic where no plugin is involved, what
	// [PanicError.Key] says.
	Key string
	// Version is the plugin version, resolved; empty when it is not known yet (a plugin skipped, or rejected
	// before its files were read, without a version in its descriptor) or no version is involved (a listing
	// problem, a panic in a storage or a poll).
	Version string
	// Language is the plugin's language; empty when no plugin is involved, or its descriptor is invalid.
	Language Language
	// Storage is the name of the storage the plugin was listed by; empty when no plugin version is involved, or
	// a listing problem rejected it.
	Storage string
	// Owner is the plugin's owner, as its descriptor names it; zero when it has none or it is not known (a
	// listing problem, a failure in your own code).
	Owner Owner
}

// eventQueueSize is how many events wait for the WithOnEvent function at most; more are dropped.
const eventQueueSize = 1024

// WithOnEvent calls fn with every [Event] of the container's plugins: versions loaded, rejected, retired,
// validated on demand and skipped, and every [PanicError] reported. Events never delay what emits them (calls,
// Register, reloads, validations): one goroutine of the container delivers them, in the order they were queued,
// from a queue of up to 1024 events; an event that finds it full is dropped, counted in plg.events.dropped and
// logged at Warn. A panic in fn is recovered, logged and kept in [Container.RecentPanics] with [PhaseCallback]
// (it emits no event), and delivery goes on. [Container.Close] delivers the events queued, those of the versions
// it retires included, for as long as its ctx allows, then stops delivering: fn's ctx ends when Close's does,
// and events that happen afterwards are not delivered. [Check] emits no events. A nil fn delivers nothing.
func WithOnEvent(fn func(ctx context.Context, e Event)) Option {
	return func(c *containerConfig) {
		c.onEvent = fn
	}
}

// eventQueue delivers a container's events to its WithOnEvent function from one goroutine; its methods do
// nothing on a nil queue (no WithOnEvent). Emitting never blocks: an event that finds the queue full is dropped,
// counted and logged.
type eventQueue struct {
	fn     func(context.Context, Event)
	panics *panicReporter // guards fn, and counts drops in its meters
	logger *slog.Logger   // the container's guarded logger, for drops
	queue  chan *Event
	stop   chan struct{}      // closed by close: deliver what is queued, then return
	done   chan struct{}      // closed once the goroutine returned
	cancel context.CancelFunc // ends the goroutine's context: deliver nothing more
	// closing is set once close runs: later events are not queued.
	closing atomic.Bool
}

// newEventQueue starts the queue delivering to fn, or returns nil for a nil fn.
func newEventQueue(fn func(context.Context, Event), panics *panicReporter, logger *slog.Logger) *eventQueue {
	if fn == nil {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	q := &eventQueue{
		fn: fn, panics: panics, logger: logger, queue: make(chan *Event, eventQueueSize),
		stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel,
	}

	go q.run(ctx)

	return q
}

// run delivers the queued events until close stops it: it then delivers what is queued while ctx lasts.
func (q *eventQueue) run(ctx context.Context) {
	defer close(q.done)

	for {
		select {
		case e := <-q.queue:
			q.deliver(ctx, e)
		case <-q.stop:
			for ctx.Err() == nil {
				select {
				case e := <-q.queue:
					q.deliver(ctx, e)
				default:
					return
				}
			}

			return
		}
	}
}

// deliver passes e to fn behind the callback boundary, unless ctx ended.
func (q *eventQueue) deliver(ctx context.Context, e *Event) {
	if ctx.Err() != nil {
		return
	}

	q.panics.callback(ctx, e.Key, e.Language, onEventMethod, func() { q.fn(ctx, *e) })
}

// close stops queueing events and waits on ctx for the goroutine to deliver what is queued. When ctx ends first,
// the goroutine's context ends too: it delivers nothing more, and returns once fn, if running, does.
func (q *eventQueue) close(ctx context.Context) error {
	if q == nil {
		return nil
	}

	q.closing.Store(true)
	close(q.stop)

	select {
	case <-q.done:
		q.cancel()

		return nil
	case <-ctx.Done():
		q.cancel()

		return fmt.Errorf("delivering events: %w", ctx.Err())
	}
}

// emit queues e, or drops it when the queue is full.
func (q *eventQueue) emit(ctx context.Context, e *Event) {
	if q.closing.Load() {
		return
	}

	select {
	case q.queue <- e:
	default:
		q.dropped(ctx, e)
	}
}

// dropped counts and logs event, which found the queue full.
func (q *eventQueue) dropped(ctx context.Context, event *Event) {
	tags := event.Owner.metricAttrs()

	q.panics.callback(ctx, event.Key, event.Language, meterMethod, func() {
		q.panics.meters.eventsDropped.Add(ctx, 1, metric.WithAttributes(attribute.String("key", event.Key),
			attribute.String("kind", string(event.Kind)), tags[0], tags[1], tags[2]))
	})

	q.logger.LogAttrs(ctx, slog.LevelWarn, "plg: event dropped: the WithOnEvent queue is full",
		slog.String("key", event.Key), event.Owner.logAttr(), slog.String("kind", string(event.Kind)),
		slog.String("version", event.Version))
}

// generation emits an event of kind about gen, a published plugin version, with report.
func (q *eventQueue) generation(ctx context.Context, kind EventKind, gen *plugin, report *Report) {
	if q == nil {
		return
	}

	q.emit(ctx, &Event{
		At: time.Now(), Report: report, Kind: kind, Key: gen.key, Version: gen.desc.Version,
		Language: gen.desc.Language, Storage: gen.storage, Owner: *gen.desc.owner(),
	})
}

// source emits an event of kind about src, a listed plugin keyed key, with report.
func (q *eventQueue) source(ctx context.Context, kind EventKind, key string, src *source, report *Report) {
	if q == nil {
		return
	}

	q.emit(ctx, &Event{
		At: time.Now(), Report: report, Kind: kind, Key: key, Version: src.desc.Version,
		Language: src.desc.Language, Storage: src.storeName, Owner: *src.desc.owner(),
	})
}

// problem emits the [EventRejected] event of a listed plugin that cannot be loaded, report saying why.
func (q *eventQueue) problem(ctx context.Context, report *Report) {
	if q == nil {
		return
	}

	q.emit(ctx, &Event{At: report.At, Report: report, Kind: EventRejected, Key: report.Key})
}

// panicked emits the [EventPanicked] event of a reported failure, unless [Check] reported it.
func (q *eventQueue) panicked(ctx context.Context, failure *PanicError) {
	if q == nil || ctx.Value(checkKey{}) != nil {
		return
	}

	event := &Event{
		At: failure.Time, Panic: failure, Kind: EventPanicked, Key: failure.Key, Language: failure.Language,
		Owner: failure.Owner,
	}

	if failure.tags != nil {
		event.Version, event.Storage = failure.tags.version, failure.tags.storage
	}

	q.emit(ctx, event)
}

// checkKey marks the context of a [Check], whose PanicErrors emit no events.
type checkKey struct{}
