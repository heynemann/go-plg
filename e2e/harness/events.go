package harness

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

// eventBuffer is how many events Events holds before the test reads them: more than any scenario's platform
// emits. A full buffer holds plg's delivery goroutine, never a call (plg drops, and counts, the events that find
// its own queue full), until the test reads or the platform's Close gives up on the delivery: Close's context
// ending (CloseTimeout for a rig's platform) ends OnEvent's, so that Close fails instead of hanging.
const eventBuffer = 4096

// eventTimeout bounds Wait: an event a scenario waits for comes within a poll or two, so a minute means it never
// will.
const eventTimeout = time.Minute

// Events receives a platform's lifecycle events (Config.OnEvent), in the order plg delivers them, for a test to
// wait on what the platform does in the background (a poll publishing a plugin) without sleeping or polling. Safe
// for concurrent use.
type Events struct {
	events chan plg.Event
}

// NewEvents returns an empty Events; pass its OnEvent as the platform's.
func NewEvents() *Events {
	return &Events{events: make(chan plg.Event, eventBuffer)}
}

// OnEvent keeps event, until ctx (the platform's delivery's, which its Close ends) ends.
//
//nolint:gocritic // hugeParam: the signature of plg.WithOnEvent's function, which Config.OnEvent takes
func (e *Events) OnEvent(ctx context.Context, event plg.Event) {
	select {
	case e.events <- event:
	case <-ctx.Done():
	}
}

// Wait returns the first event, of those not yet passed by an earlier Wait, that match accepts, discarding the
// ones before it; tb fails when none comes within eventTimeout.
func (e *Events) Wait(tb testing.TB, match func(plg.Event) bool) plg.Event {
	tb.Helper()

	timeout := time.NewTimer(eventTimeout)
	defer timeout.Stop()

	for {
		select {
		case event := <-e.events:
			if match(event) {
				return event
			}
		case <-timeout.C:
			require.FailNow(tb, "no matching event within "+eventTimeout.String())
		case <-tb.Context().Done():
			require.FailNow(tb, "the test ended waiting for an event")
		}
	}
}

// Drain returns the events received and not yet passed by a Wait, oldest first, without waiting for more.
func (e *Events) Drain() []plg.Event {
	var drained []plg.Event

	for {
		select {
		case event := <-e.events:
			drained = append(drained, event)
		default:
			return drained
		}
	}
}
