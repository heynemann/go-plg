package plg_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

// explosiveStorage is a memory storage whose List panics while panics is set.
type explosiveStorage struct {
	*plg.MemoryStorage

	panics atomic.Bool
}

func (s *explosiveStorage) List(ctx context.Context) ([]plg.Descriptor, error) {
	if s.panics.Load() {
		panic("storage exploded")
	}

	return s.MemoryStorage.List(ctx) //nolint:wrapcheck // the memory storage's own error, unchanged
}

// loggedUnitContainer returns a container logging to logs as JSON.
func loggedUnitContainer(t *testing.T, logs *bytes.Buffer) *plg.Container {
	t.Helper()

	ctr := plg.New(plg.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	return ctr
}

func TestReloadInterfaceNeedsARegisteredName(t *testing.T) {
	t.Parallel()

	ctr := newUnitContainer(t)
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", emptyStorage()))

	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "ping"))
	require.NoError(t, ctr.Reload(t.Context()))

	err := plg.ReloadInterface(t.Context(), ctr, "pong")
	require.ErrorIs(t, err, plg.ErrNotFound)
	require.ErrorContains(t, err, `"pong"`)
}

func TestReloadAfterClose(t *testing.T) {
	t.Parallel()

	ctr := plg.New()
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", emptyStorage()))
	require.NoError(t, ctr.Close(t.Context()))

	require.ErrorIs(t, ctr.Reload(t.Context()), plg.ErrClosed)
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "ping"), plg.ErrClosed)
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "pong"), plg.ErrClosed)
}

// TestReloadSkipsNewPluginsItCannotLoad adds plugins that Register would refuse: a reload skips each, logs it and
// reports it, and publishes nothing for them.
func TestReloadSkipsNewPluginsItCannotLoad(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggedUnitContainer(t, &logs)
	first, second := plg.NewMemoryStorage("first"), plg.NewMemoryStorage("second")
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(first), plg.WithStorage(second)))

	for store, desc := range map[*plg.MemoryStorage]plg.Descriptor{
		first:  {Key: "cobol", Implements: implementsOf[pinger](), Language: "cobol", Entrypoint: "main.cbl"},
		second: {Key: "noentry", Implements: implementsOf[pinger](), Language: plg.LanguageGo},
	} {
		require.NoError(t, store.Put(&desc, fstest.MapFS{"main.cbl": {}}))
	}

	for _, store := range []*plg.MemoryStorage{first, second} {
		desc := plg.Descriptor{
			Key: "dup", Implements: implementsOf[pinger](), Language: plg.LanguageGo, Entrypoint: "plugin.go",
		}
		require.NoError(t, store.Put(&desc, fstest.MapFS{"plugin.go": {}}))
	}

	err := plg.ReloadInterface(t.Context(), ctr, "ping")
	require.ErrorIs(t, err, plg.ErrUnsupportedLanguage)
	require.ErrorIs(t, err, plg.ErrInvalidDescriptor)
	require.ErrorIs(t, err, plg.ErrDuplicateKey)

	for _, key := range []string{"ping/cobol", "ping/noentry", "ping/dup"} {
		_, err = plg.Get[pinger](t.Context(), ctr, key)
		require.ErrorIs(t, err, plg.ErrNotFound, key)
		assert.Contains(t, logs.String(),
			`"msg":"plg: reload skipped a new plugin that failed to load","key":"`+key+`"`)
	}
}

func TestReloadFailsWhenAStorageCannotBeListed(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggedUnitContainer(t, &logs)
	store := &failingStorage{MemoryStorage: plg.NewMemoryStorage("flaky")}
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(store)))

	store.failList = true // read by the reload round, which the request starts

	err := ctr.Reload(t.Context())
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorIs(t, err, errStorage)
	require.ErrorContains(t, err, "listing storage flaky")
	assert.Contains(t, logs.String(), `"level":"WARN","msg":"plg: reload failed","name":"ping"`)
}

// TestReloadSurvivesAPanickingStorage reloads through a storage that panics: the reload fails with the storage's
// PanicError, and the registration's reloader goroutine survives to serve the next one.
func TestReloadSurvivesAPanickingStorage(t *testing.T) {
	t.Parallel()

	ctr := newUnitContainer(t)
	store := &explosiveStorage{MemoryStorage: plg.NewMemoryStorage("explosive")}
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(store)))

	store.panics.Store(true)

	err := plg.ReloadInterface(t.Context(), ctr, "ping")
	require.ErrorIs(t, err, plg.ErrLoad)

	pe, ok := errors.AsType[*plg.PanicError](err)
	require.True(t, ok, "the panic is reported")
	assert.Equal(t, plg.PhaseStorage, pe.Phase)
	assert.Equal(t, "List", pe.Method)
	assert.Equal(t, "ping", pe.Key)
	assert.Equal(t, "storage exploded", pe.Value)
	assert.Equal(t, []*plg.PanicError{pe}, ctr.RecentPanics())

	store.panics.Store(false)
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "ping"))
}

// panickingLogger is a slog handler that panics on every record with the message msg and passes the others on.
// It takes every level, so that the record it panics on is never filtered out.
type panickingLogger struct {
	slog.Handler

	msg string
}

func (panickingLogger) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (h panickingLogger) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Message == h.msg {
		panic("logger exploded")
	}

	return h.Handler.Handle(ctx, rec) //nolint:wrapcheck // the wrapped handler's own error, unchanged
}

// TestReloadSurvivesAPanicInTheRound panics in a reload round outside any storage call or load (the user's
// logger, unguarded as it was before logging was guarded, logging the round's failure): the round fails with a
// PanicError in PhasePoll, and the reloader survives to serve the next one.
func TestReloadSurvivesAPanicInTheRound(t *testing.T) {
	t.Parallel()

	logger := slog.New(panickingLogger{Handler: slog.DiscardHandler, msg: "plg: reload failed"})
	ctr := plg.New(plg.WithLogger(logger))
	plg.UnguardLogger(ctr, true)

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	store := &failingStorage{MemoryStorage: plg.NewMemoryStorage("flaky")}
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(store)))

	store.failList = true // read by the reload round, which the request starts

	err := plg.ReloadInterface(t.Context(), ctr, "ping")

	pe, ok := errors.AsType[*plg.PanicError](err)
	require.True(t, ok, "the panic is reported")
	assert.Equal(t, plg.PhasePoll, pe.Phase)
	assert.Equal(t, "ping", pe.Key)
	assert.Equal(t, "logger exploded", pe.Value)
	assert.Contains(t, string(pe.Stack), "(*Container).refresh", "recovered around the round")
	assert.Contains(t, ctr.RecentPanics(), pe)

	store.failList = false

	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "ping"))
}

// TestReloadRoundsContainAPanickingMeterProvider fails reload rounds (a storage that cannot be listed) once every
// measurement panics: counting the failure is a PanicError in PhaseCallback, also when the round itself panicked
// (the user's logger, unguarded, logging the failure) and its recovery counts it, and the reloader serves the next
// round.
func TestReloadRoundsContainAPanickingMeterProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		check     func(t *testing.T, err error, recent []*plg.PanicError)
		handler   slog.Handler
		name      string
		unguarded bool
	}{
		{
			name: "failed round", handler: slog.DiscardHandler,
			check: func(t *testing.T, err error, recent []*plg.PanicError) {
				t.Helper()
				require.ErrorIs(t, err, errStorage)
				assert.Equal(t, []string{"ping"}, meterPanicKeys(t, recent), "counting the failure")
			},
		},
		{
			name: "panicking round", handler: panickingLogger{Handler: slog.DiscardHandler, msg: "plg: reload failed"},
			unguarded: true,
			check: func(t *testing.T, err error, recent []*plg.PanicError) {
				t.Helper()

				pe, ok := errors.AsType[*plg.PanicError](err)
				require.True(t, ok, "the round's panic is reported: %v", err)
				assert.Equal(t, plg.PhasePoll, pe.Phase)
				assert.Equal(t, "logger exploded", pe.Value)

				require.Len(t, recent, 3)
				assert.Equal(t, pe, recent[0])
				assert.Equal(t, []string{"ping", "ping"}, meterPanicKeys(t, recent[1:]),
					"counting the round's panic, then its failure")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			broken := new(atomic.Bool)
			ctr := plg.New(plg.WithLogger(slog.New(tc.handler)), plg.WithMeterProvider(brokenMeters{broken: broken}))
			plg.UnguardLogger(ctr, tc.unguarded)

			t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

			store := &failingStorage{MemoryStorage: plg.NewMemoryStorage("flaky")}
			require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(store)))

			broken.Store(true)

			store.failList = true // read by the reload round, which the request starts

			tc.check(t, plg.ReloadInterface(t.Context(), ctr, "ping"), ctr.RecentPanics())

			store.failList = false

			require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "ping"), "the reloader serves the next round")
		})
	}
}

// TestCountingSkippedPluginsContainsAPanickingMeterProvider lists a plugin in a language the Register does not
// enable while every measurement panics: Register succeeds, the reload skips the plugin again, and each count of
// it is a PanicError in PhaseCallback.
func TestCountingSkippedPluginsContainsAPanickingMeterProvider(t *testing.T) {
	t.Parallel()

	broken := new(atomic.Bool)
	broken.Store(true)

	ctr := plg.New(plg.WithMeterProvider(brokenMeters{broken: broken}))

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	store := plg.NewMemoryStorage("scripts")
	lua := plg.Descriptor{
		Key:        "l",
		Implements: implementsOf[pinger](),
		Language:   plg.LanguageLua,
		Entrypoint: "main.lua",
	}
	require.NoError(t, store.Put(&lua, fstest.MapFS{"main.lua": {Data: []byte("return {}")}}))

	var err error

	require.NotPanics(t, func() {
		err = plg.Register[pinger](t.Context(), ctr, "langs", plg.WithStorage(store), plg.WithGolangPlugins())
	}, "counting the skipped plugin")
	require.NoError(t, err)
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "langs"), "the reload skips it again")
	assert.Equal(t, []string{"langs/l", "langs/l"}, meterPanicKeys(t, ctr.RecentPanics()))
}
