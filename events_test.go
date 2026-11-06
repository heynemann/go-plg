package plg_test

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

// eventWait bounds how long a test waits for an event it expects, so a missing event fails the test instead of
// hanging it; delivery itself is signalled, never slept for.
const eventWait = time.Minute

// sendEvents returns a WithOnEvent option sending every event on events, so a test receives them in the order
// they were delivered.
func sendEvents(events chan<- plg.Event) plg.Option {
	return plg.WithOnEvent(func(_ context.Context, e plg.Event) { events <- e })
}

// nextEvent receives the next delivered event.
func nextEvent(t *testing.T, events <-chan plg.Event) plg.Event {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), eventWait)
	defer cancel()

	select {
	case e := <-events:
		return e
	case <-ctx.Done():
		require.FailNow(t, "no event delivered")

		return plg.Event{}
	}
}

// awaitClosed waits for signal to be closed.
func awaitClosed(t *testing.T, signal <-chan struct{}) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), eventWait)
	defer cancel()

	select {
	case <-signal:
	case <-ctx.Done():
		require.FailNow(t, "no signal")
	}
}

// assertEvent checks an event's kind and the plugin it is about.
func assertEvent(t *testing.T, event *plg.Event, kind plg.EventKind, key, version string, lang plg.Language,
	storage string, owner *plg.Owner,
) {
	t.Helper()

	assert.Equal(t, kind, event.Kind, key)
	assert.Equal(t, key, event.Key, kind)
	assert.Equal(t, version, event.Version, "%s %s", kind, key)
	assert.Equal(t, lang, event.Language, "%s %s", kind, key)
	assert.Equal(t, storage, event.Storage, "%s %s", kind, key)
	assert.Equal(t, *owner, event.Owner, "%s %s", kind, key)
	assert.False(t, event.At.IsZero(), "%s %s", kind, key)
}

// putSkipped puts a TypeScript plugin, which a registration enabling Lua only skips, into store.
func putSkipped(t *testing.T, store *plg.MemoryStorage, key, version string, owner *plg.Owner) {
	t.Helper()

	require.NoError(t, store.Put(&plg.Descriptor{
		Key: key, Language: plg.LanguageTypeScript, Entrypoint: "plugin.ts", Version: version, Owner: owner,
	}, fstest.MapFS{"plugin.ts": {Data: []byte("export function Create() {}\n")}}))
}

// TestOnEventReportsSkippedAndListingProblems follows the events of plugins that never load: a key listed by two
// storages is rejected at Register (which fails) and at a reload (which keeps the rest), with its listing problem
// as the report's Err and nothing known of the plugin; a plugin in a language the Register leaves out is skipped
// at Register and at every reload, with its descriptor's version, language, storage and owner.
func TestOnEventReportsSkippedAndListingProblems(t *testing.T) {
	t.Parallel()

	events := make(chan plg.Event, 16)
	ctr := plg.New(sendEvents(events))

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	owner := plg.Owner{Team: plg.Team{Name: "search"}, Contact: plg.Contact{Channel: "#search"}}
	first, second := plg.NewMemoryStorage("first"), plg.NewMemoryStorage("second")
	putSkipped(t, first, "skipped", "3", &owner)
	putSkipped(t, first, "dup", "1", &owner)
	putSkipped(t, second, "dup", "1", &owner)

	err := plg.Register[pinger](t.Context(), ctr, "dups", plg.WithStorage(first), plg.WithStorage(second),
		plg.WithLuaPlugins())
	require.ErrorIs(t, err, plg.ErrDuplicateKey)

	rejected := nextEvent(t, events)
	assertEvent(t, &rejected, plg.EventRejected, "dups/dup", "", "", "", &plg.Owner{})
	require.NotNil(t, rejected.Report)
	assert.Equal(t, "dups/dup", rejected.Report.Key)
	require.ErrorIs(t, rejected.Report.Err, plg.ErrDuplicateKey)
	assert.Nil(t, rejected.Panic)

	second.Delete("dup")
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ok", plg.WithStorage(first), plg.WithStorage(second),
		plg.WithLuaPlugins()))

	for _, key := range []string{"dup", "skipped"} { // in the storages' order
		skipped := nextEvent(t, events)
		assertEvent(t, &skipped, plg.EventSkipped, "ok/"+key, map[string]string{"dup": "1", "skipped": "3"}[key],
			plg.LanguageTypeScript, "first", &owner)
		assert.Nil(t, skipped.Report, key)
		assert.Nil(t, skipped.Panic, key)
	}

	putSkipped(t, second, "dup", "1", &owner)
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "ok"), plg.ErrDuplicateKey)

	skipped := nextEvent(t, events)
	assertEvent(t, &skipped, plg.EventSkipped, "ok/skipped", "3", plg.LanguageTypeScript, "first", &owner)

	rejected = nextEvent(t, events)
	assertEvent(t, &rejected, plg.EventRejected, "ok/dup", "", "", "", &plg.Owner{})
	require.NotNil(t, rejected.Report)
	require.ErrorIs(t, rejected.Report.Err, plg.ErrDuplicateKey)

	require.NoError(t, ctr.Close(t.Context()))
	assert.Empty(t, events, "nothing else happened")
}

// TestOnEventPanicIsContained gives the WithOnEvent function a panic on one event: it is kept as a PanicError in
// PhaseCallback naming the function and the event's plugin, emits no event of its own, and the next event is
// delivered. Close delivers what is queued and stops the delivering goroutine.
func TestOnEventPanicIsContained(t *testing.T) {
	t.Parallel()

	events := make(chan plg.Event, 4)
	ctr := plg.New(plg.WithOnEvent(func(_ context.Context, e plg.Event) {
		if e.Key == "p/a" {
			panic("event callback broke")
		}

		events <- e
	}))

	store := plg.NewMemoryStorage("store")
	putSkipped(t, store, "a", "1", nil)
	putSkipped(t, store, "b", "1", nil)

	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "p", plg.WithStorage(store), plg.WithLuaPlugins()))
	require.NoError(t, ctr.Close(t.Context()), "Close delivered both events")

	select {
	case <-plg.EventsStopped(ctr):
	default:
		require.FailNow(t, "Close returned before the delivering goroutine")
	}

	require.Len(t, events, 1)
	assert.Equal(t, "p/b", (<-events).Key, "delivery went on after the panic")

	panics := ctr.RecentPanics()
	require.Len(t, panics, 1)
	assert.Equal(t, plg.PhaseCallback, panics[0].Phase)
	assert.Equal(t, "OnEvent", panics[0].Method)
	assert.Equal(t, "p/a", panics[0].Key)
	assert.Equal(t, plg.LanguageTypeScript, panics[0].Language)
	assert.Equal(t, "event callback broke", panics[0].Value)
}

// TestCloseStopsDeliveringEventsAtItsDeadline closes a container whose WithOnEvent function blocks on the first
// of three events: Close returns with its context's error instead of waiting, the function's context ends with
// Close's, and the events still queued are never delivered.
func TestCloseStopsDeliveringEventsAtItsDeadline(t *testing.T) {
	t.Parallel()

	var (
		entered   = make(chan struct{})
		ended     = make(chan error, 1)
		delivered int
	)

	ctr := plg.New(plg.WithOnEvent(func(ctx context.Context, _ plg.Event) {
		delivered++ // only the delivering goroutine writes; read once it stopped

		if delivered == 1 {
			close(entered)
			<-ctx.Done()

			ended <- ctx.Err()
		}
	}))

	store := plg.NewMemoryStorage("store")
	for _, key := range []string{"a", "b", "c"} {
		putSkipped(t, store, key, "1", nil)
	}

	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "slow", plg.WithStorage(store),
		plg.WithLuaPlugins()))
	awaitClosed(t, entered)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, ctr.Close(ctx), context.Canceled)
	require.ErrorIs(t, <-ended, context.Canceled, "the function's context ended with Close's")

	awaitClosed(t, plg.EventsStopped(ctr))
	assert.Equal(t, 1, delivered, "the queued events were not delivered")
}
