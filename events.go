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
	// Version is the plugin version, resolved; empty when it is not known yet (a plugin skipped, rejected by a
	// listing problem or before its files were read, without a version in its descriptor) or no version is
	// involved (a panic in a storage or a poll).
	Version string
	// Language is the plugin's language; empty when no plugin is involved, or its descriptor names none.
	Language Language
	// Storage is the name of the storage the plugin was listed by (for a key listed twice, the second); empty when
	// no plugin is involved.
	Storage string
	// Owner is the plugin's owner, as its descriptor names it; zero when it has none, when the owner is what a
	// listing problem found invalid, or when no plugin is involved (a failure in your own code).
	Owner Owner
}

// eventQueueSize is how many events wait for the WithOnEvent function at most; more are dropped.
const eventQueueSize = 1024

// keptEvents is how many events of each plugin [Events] keeps: the newest.
const keptEvents = 100

// Events returns the newest events of the plugin with the given key, "<name>/<descriptor key>", oldest first: at
// most 100. They are the events [WithOnEvent] describes, kept whether the container has a WithOnEvent function or
// not (and whether its queue dropped them or not), from the listing that first names the plugin to the first that
// no longer does. It fails as [Info] does: with [ErrNotFound] for a key Info does not know and with [ErrClosed]
// once the container is closed. Events recorded while it runs may be left out. It reads what the container keeps
// without waiting for loads or calls in progress, and runs no plugin code. It is safe for concurrent use; the
// events' Report and Panic are the container's own: treat them as read-only.
func Events(ctr *Container, key string) ([]Event, error) {
	found, err := ctr.lookup(key)
	if err == nil && found.gen == nil && found.entry.rejectionBeside(nil) == nil { // Info does not know it
		err = fmt.Errorf("%w: %s", ErrNotFound, key)
	}

	if err != nil {
		return nil, err
	}

	var kept []*Event

	if found.entry != nil { // nil once a reload removing the plugin dropped its record, until it unpublishes it
		kept = found.entry.events.recent()
	}

	events := make([]Event, len(kept))

	for i, e := range kept {
		events[i] = *e
	}

	return events, nil
}

// WithOnEvent calls fn with every [Event] of the container's plugins: versions loaded, rejected, retired,
// validated on demand and skipped, and every [PanicError] reported. Events never delay what emits them (calls,
// Register, reloads, validations): one goroutine of the container delivers them, in the order they were queued,
// from a queue of up to 1024 events; an event that finds it full is dropped, counted in plg.events.dropped and
// logged at Warn. A panic in fn is recovered, logged and kept in [Container.RecentPanics] with [PhaseCallback]
// (it emits no event), and delivery goes on. [Container.Close] delivers the events queued, those of the versions
// it retires included, for as long as its ctx allows, then stops delivering: fn's ctx ends when Close's does,
// and events that happen afterwards are not delivered. [Check] emits no events. A nil fn delivers nothing; with or
// without fn, the container keeps each plugin's newest events for [Events].
func WithOnEvent(fn func(ctx context.Context, e Event)) Option {
	return func(c *containerConfig) {
		c.onEvent = fn
	}
}

// eventQueue keeps every event of the container's plugins in its plugin's record ([Events]) and delivers it to
// the WithOnEvent function, if any, from one goroutine. Emitting never blocks: an event that finds the queue full
// is not delivered but counted and logged (it is kept all the same).
type eventQueue struct {
	keep func(*Event) // records an event in its plugin's record
	// fn is the WithOnEvent function; nil without one, and then nothing below is set: no queue, no goroutine.
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

// newEventQueue returns the queue keeping events with keep and, for a non-nil fn, delivering them to fn.
func newEventQueue(keep func(*Event), fn func(context.Context, Event), panics *panicReporter,
	logger *slog.Logger,
) *eventQueue {
	events := &eventQueue{keep: keep}
	if fn == nil {
		return events
	}

	ctx, cancel := context.WithCancel(context.Background())
	events.fn, events.panics, events.logger, events.cancel = fn, panics, logger, cancel
	events.queue, events.stop, events.done = make(chan *Event, eventQueueSize), make(chan struct{}), make(chan struct{})

	go events.run(ctx)

	return events
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
	if q.fn == nil {
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

// emit keeps event and queues it for the WithOnEvent function, or drops it when the queue is full.
func (q *eventQueue) emit(ctx context.Context, event *Event) {
	q.keep(event)

	if q.fn == nil || q.closing.Load() {
		return
	}

	select {
	case q.queue <- event:
	default:
		q.dropped(ctx, event)
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
	q.emit(ctx, &Event{
		At: time.Now(), Report: report, Kind: kind, Key: gen.key, Version: gen.desc.Version,
		Language: gen.desc.Language, Storage: gen.storage, Owner: *gen.desc.owner(),
	})
}

// source emits an event of kind about src, a listed plugin keyed key, with report.
func (q *eventQueue) source(ctx context.Context, kind EventKind, key string, src *source, report *Report) {
	q.emit(ctx, &Event{
		At: time.Now(), Report: report, Kind: kind, Key: key, Version: src.desc.Version,
		Language: src.desc.Language, Storage: src.storeName, Owner: *src.desc.owner(),
	})
}

// problem emits the [EventRejected] event of rej, the rejection of a listed plugin that cannot be loaded.
func (q *eventQueue) problem(ctx context.Context, rej *rejection) {
	q.emit(ctx, &Event{
		At: rej.report.At, Report: rej.report, Kind: EventRejected, Key: rej.report.Key, Version: rej.report.Version,
		Language: rej.language, Storage: rej.storage, Owner: *rej.owner,
	})
}

// panicked emits the [EventPanicked] event of a reported failure, unless [Check] reported it. A nil queue (a
// reporter without a container) emits nothing.
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
