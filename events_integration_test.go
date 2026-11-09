//go:build integration

package plg_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// onlyLanguage returns the Register option enabling lang only, and a language it then skips.
func onlyLanguage(lang plg.Language) (plg.RegisterOption, plg.Language) {
	switch lang {
	case plg.LanguageGo:
		return plg.WithGolangPlugins(), plg.LanguageLua
	case plg.LanguageLua:
		return plg.WithLuaPlugins(), plg.LanguageGo
	case plg.LanguageTypeScript:
		return plg.WithTypescriptPlugins(), plg.LanguageGo
	default:
		panic("no contract language " + string(lang))
	}
}

// lifeKey is the key of the plugin TestOnEventFollowsAPluginsLife follows.
const lifeKey = "life/p"

// lifeEvents receives and checks the events of TestOnEventFollowsAPluginsLife.
type lifeEvents struct {
	t      *testing.T
	events <-chan plg.Event
	lang   plg.Language
	owner  plg.Owner
}

// next receives the next event and checks that it is of kind about version of the followed plugin, in the
// life-store storage.
func (l *lifeEvents) next(kind plg.EventKind, version string) plg.Event {
	l.t.Helper()

	event := nextEvent(l.t, l.events)
	assertEvent(l.t, &event, kind, lifeKey, version, l.lang, "life-store", &l.owner)

	return event
}

// loaded checks that the next event is version's load, which passed.
func (l *lifeEvents) loaded(version string) {
	l.t.Helper()

	event := l.next(plg.EventLoaded, version)
	require.NotNil(l.t, event.Report, version)
	assert.Equal(l.t, lifeKey, event.Report.Key, version)
	assert.Equal(l.t, version, event.Report.Version, version)
	require.NoError(l.t, event.Report.Err, version)
}

// TestOnEventFollowsAPluginsLife follows a validated plugin through every event of its life, in the order they
// happen, each naming its key, version, language, storage and owner: rejected by a Register its validation
// blocks, skipped (a plugin in another language) at Register and each reload, loaded, rejected again by a reload
// its validation blocks, loaded at a new version that retires the old one, validated on demand, then unpublished
// by an on-demand validation that panics (the panic, the validation, the rejection and the retirement). A Check
// whose validation panics in between emits nothing, its PanicError included.
func TestOnEventFollowsAPluginsLife(t *testing.T) {
	t.Parallel()

	eachValidatingLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		events := make(chan plg.Event, 16)
		ctr := newContainer(t, sendEvents(events))

		const key = lifeKey

		life := &lifeEvents{t: t, events: events, owner: validatorOwner(), lang: lang.lang}
		checks := newLiveChecks(map[string]string{"bad/p": "error", key: "valid"})
		only, other := onlyLanguage(lang.lang)
		register := func(name string, store *plg.MemoryStorage) error {
			return plg.Register[testcontract.Fooer](t.Context(), ctr, name, plg.WithStorage(store), only,
				plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit), plg.WithHost[testcontract.Host](newTestHost()),
				plg.WithValidation(checks.input()), plg.WithOnDemandValidationPolicy(plg.Unpublish))
		}

		bad := plg.NewMemoryStorage("bad-store")
		putValidator(t, bad, lang, "p", "1")
		require.ErrorIs(t, register("bad", bad), plg.ErrBlockingIssue)

		event := nextEvent(t, events)
		assertEvent(t, &event, plg.EventRejected, "bad/p", "1", lang.lang, "bad-store", &life.owner)
		require.NotNil(t, event.Report)
		require.ErrorIs(t, event.Report.Err, plg.ErrBlockingIssue)
		assert.Len(t, event.Report.Issues, 3, "the contract's local checkout, the validation's error and warning")

		store := plg.NewMemoryStorage("life-store")
		putValidator(t, store, lang, "p", "1")
		require.NoError(t, store.Put(&plg.Descriptor{
			Key: "q", Implements: implementsOf[testcontract.Fooer](), Language: other, Entrypoint: "main", Version: "9",
			Owner: &life.owner,
		}, fstest.MapFS{"main": {Data: []byte("never read\n")}}))

		skipped := func() {
			t.Helper()

			got := nextEvent(t, events)
			assertEvent(t, &got, plg.EventSkipped, "life/q", "9", other, "life-store", &life.owner)
			require.NotNil(t, got.Report)
			require.ErrorIs(t, got.Report.Err, plg.ErrUnsupportedLanguage)
		}

		require.NoError(t, register("life", store))
		skipped()
		life.loaded("1")

		checks.set(map[string]string{key: "error"})
		putValidator(t, store, lang, "p", "2")
		require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "life"), plg.ErrBlockingIssue)
		skipped()

		event = life.next(plg.EventRejected, "2")
		require.NotNil(t, event.Report)
		require.ErrorIs(t, event.Report.Err, plg.ErrBlockingIssue)

		checks.set(map[string]string{key: "valid"})
		putValidator(t, store, lang, "p", "3")
		require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "life"))
		skipped()
		life.loaded("3")
		assert.Nil(t, life.next(plg.EventRetired, "1").Report)

		checks.set(map[string]string{key: "panic"})

		panics := len(ctr.RecentPanics())
		checked, err := plg.Check(t.Context(), ctr, "life", &plg.Descriptor{
			Implements: implementsOf[testcontract.Fooer](),
			Key:        "p", Language: lang.lang, Entrypoint: lang.entry, Version: "4", Owner: &life.owner,
		}, lang.files("validator"))
		require.NoError(t, err)
		require.ErrorIs(t, checked.Err, plg.ErrValidation)
		assert.Len(t, ctr.RecentPanics(), panics+1, "the check's validation panicked")

		checks.set(map[string]string{key: "valid"})

		report, err := plg.Validate(t.Context(), ctr, key)
		require.NoError(t, err)
		require.NoError(t, report.Err)

		event = life.next(plg.EventValidated, "3") // the check emitted nothing
		require.NotNil(t, event.Report)
		assert.Equal(t, report, *event.Report)

		checks.set(map[string]string{key: "panic"})

		report, err = plg.Validate(t.Context(), ctr, key)
		require.NoError(t, err)

		var failure *plg.PanicError

		require.ErrorAs(t, report.Err, &failure)

		event = life.next(plg.EventPanicked, "3")
		assert.Same(t, failure, event.Panic)
		assert.Equal(t, failure.Time, event.At)
		assert.Nil(t, event.Report)

		event = life.next(plg.EventValidated, "3")
		require.NotNil(t, event.Report)
		assert.Equal(t, report, *event.Report)

		validated := event.Report

		event = life.next(plg.EventRejected, "3")
		assert.Same(t, validated, event.Report, "the unpublishing validation's report")
		assert.Nil(t, life.next(plg.EventRetired, "3").Report)

		require.NoError(t, ctr.Close(t.Context()))
		assert.Empty(t, events, "nothing else happened, Close included: nothing was served any more")
	})
}

// TestSlowOnEventNeverDelaysThePlugins blocks the WithOnEvent function on the first event: meanwhile Register
// loads a Lua plugin, the plugin serves a call and reloads run, each queueing its events without waiting; once
// 1024 events wait, every further one is dropped, counted in plg.events.dropped by key and kind and logged, and
// still kept for Events. When the function is released it gets every queued event in order, and Close delivers
// its own (the plugin retired).
func TestSlowOnEventNeverDelaysThePlugins(t *testing.T) {
	t.Parallel()

	var (
		logs     bytes.Buffer
		got      []plg.Event // written by the delivering goroutine only, read once Close stopped it
		entered  = make(chan struct{})
		release  = make(chan struct{})
		caughtUp = make(chan struct{})
	)

	reader := newMetricsReader(t)
	ctr := newContainer(t, plg.WithMeterProvider(reader.provider),
		plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
		plg.WithOnEvent(func(_ context.Context, e plg.Event) {
			if len(got) == 0 {
				close(entered)
				<-release
			}

			got = append(got, e)

			if e.Key == "flood/s1021" { // the last event queued
				close(caughtUp)
			}
		}))

	releaseOnce := sync.OnceFunc(func() { close(release) })

	t.Cleanup(releaseOnce) // before the container's Close, should the test stop early

	first := plg.NewMemoryStorage("first")
	putSkipped(t, first, "s", "1", nil)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "first", plg.WithStorage(first),
		plg.WithLuaPlugins()))
	awaitClosed(t, entered)

	lua := plg.NewMemoryStorage("lua")
	require.NoError(t, lua.Put(&plg.Descriptor{
		Key: "p", Implements: implementsOf[testcontract.Fooer](), Language: plg.LanguageLua, Entrypoint: "plugin.lua",
		Version: "1",
	}, luaLanguage().files("fooer")))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "lua", plg.WithStorage(lua),
		plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)))

	greeting, err := getFooer(t, ctr, "lua/p").Foo(t.Context(), "events")
	require.NoError(t, err)
	assert.Equal(t, "hello, events", greeting)

	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "lua"), "unchanged: no event")
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "first"))

	// Two events wait (lua/p loaded, first/s skipped again): 1022 of these fit, the last 78 are dropped.
	flood := plg.NewMemoryStorage("flood")
	for i := range 1100 {
		putSkipped(t, flood, fmt.Sprintf("s%04d", i), "1", nil)
	}

	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "flood", plg.WithStorage(flood),
		plg.WithLuaPlugins()))

	dropped, ok := reader.collect(t)["plg.events.dropped"].Data.(metricdata.Sum[int64])
	require.True(t, ok)

	var total int64

	for _, dp := range dropped.DataPoints {
		total += dp.Value
	}

	assert.Equal(t, int64(78), total)

	points := reader.collect(t)
	assert.Equal(t, int64(1), points.int64Value(t, "plg.events.dropped", attribute.String("key", "flood/s1099"),
		attribute.String("kind", "skipped")))
	assert.Equal(t, int64(1), points.int64Value(t, "plg.events.dropped", attribute.String("key", "flood/s1022"),
		attribute.String("kind", "skipped")))

	kept := keptEvents(t, ctr, "flood/s1099")
	require.Len(t, kept, 1, "an event the queue dropped is kept for Events all the same")
	assert.Equal(t, plg.EventSkipped, kept[0].Kind)

	releaseOnce()
	awaitClosed(t, caughtUp)
	require.NoError(t, ctr.Close(t.Context()))

	require.Len(t, got, 1+2+1022+1)
	assert.Equal(t, plg.EventSkipped, got[0].Kind)
	assert.Equal(t, "first/s", got[0].Key)
	assert.Equal(t, plg.EventLoaded, got[1].Kind)
	assert.Equal(t, "lua/p", got[1].Key)
	assert.Equal(t, "first/s", got[2].Key)

	for i, e := range got[3:1025] {
		assert.Equal(t, fmt.Sprintf("flood/s%04d", i), e.Key)
	}

	assert.Equal(t, plg.EventRetired, got[1025].Kind, "Close retired the plugin")
	assert.Equal(t, "lua/p", got[1025].Key)

	var warnings []string

	lines := bufio.NewScanner(bytes.NewReader(logs.Bytes()))
	for lines.Scan() {
		var rec struct {
			Msg   string `json:"msg"`
			Level string `json:"level"`
			Key   string `json:"key"`
			Kind  string `json:"kind"`
		}

		require.NoError(t, sonic.Unmarshal(lines.Bytes(), &rec))

		if rec.Msg == "plg: event dropped: the WithOnEvent queue is full" {
			assert.Equal(t, "WARN", rec.Level)
			assert.Equal(t, "skipped", rec.Kind)

			warnings = append(warnings, rec.Key)
		}
	}

	require.NoError(t, lines.Err())
	require.Len(t, warnings, 78)
	assert.Equal(t, "flood/s1022", warnings[0])
	assert.Equal(t, "flood/s1099", warnings[77])
}
