package plg_test

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

// TestInfoDescribesPluginsNoVersionServes registers a storage listing a plugin in a language the Register does
// not enable: Info describes it as skipped, with its storage, language and owner. A reload then finds a plugin
// in another such language, a key listed by two storages and a descriptor whose owner breaks the limits: the
// first is skipped too, the others rejected, each described with why, its storage, its language and its owner
// unless the owner is what is wrong, until the storages stop listing them. Plugins lists them all in key order.
// None needs a guest.
func TestInfoDescribesPluginsNoVersionServes(t *testing.T) {
	t.Parallel()

	owner := plg.Owner{Team: plg.Team{Name: "payments"}, Contact: plg.Contact{Email: "payments@example.com"}}
	other := plg.Owner{Team: plg.Team{Name: "search"}}
	first, second := plg.NewMemoryStorage("first"), plg.NewMemoryStorage("second")
	put := func(store *plg.MemoryStorage, key string, lang plg.Language, version string, by *plg.Owner) {
		t.Helper()

		require.NoError(t, store.Put(&plg.Descriptor{
			Key: key, Language: lang, Entrypoint: "plugin", Version: version, Owner: by,
		}, fstest.MapFS{"plugin": {}}))
	}

	put(first, "lua", plg.LanguageLua, "3", &owner)

	ctr := newUnitContainer(t)
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(first),
		plg.WithStorage(second), plg.WithGolangPlugins()))

	lua := plg.PluginState{
		Key: "ping/lua", Registration: "ping", Storage: "first", Language: plg.LanguageLua, Status: plg.StatusSkipped,
		Owner: owner,
	}
	assert.Equal(t, lua, info(t, ctr, "ping/lua"), "skipped at Register")
	assert.Equal(t, []plg.PluginState{lua}, plg.Plugins(ctr))

	put(second, "ts", plg.LanguageTypeScript, "", nil)
	put(first, "dup", plg.LanguageGo, "1", &owner)
	put(second, "dup", plg.LanguageGo, "2", &other)

	tooLong := plg.Owner{Team: plg.Team{Name: strings.Repeat("x", plg.MaxOwnerStringBytes+1)}}
	put(first, "owner", plg.LanguageGo, "1", &tooLong)

	before := time.Now()
	err := plg.ReloadInterface(t.Context(), ctr, "ping")
	after := time.Now()

	require.ErrorIs(t, err, plg.ErrDuplicateKey)
	require.ErrorIs(t, err, plg.ErrInvalidDescriptor)

	dup := info(t, ctr, "ping/dup")
	require.NotNil(t, dup.Rejection)
	require.ErrorIs(t, dup.Rejection.Err, plg.ErrDuplicateKey)
	assert.False(t, dup.Rejection.At.Before(before) || dup.Rejection.At.After(after), "the reload's listing")
	assert.Equal(t, plg.PluginState{
		Rejection: &plg.Report{
			At: dup.Rejection.At, Err: dup.Rejection.Err, Owner: other, Key: "ping/dup", Version: "2",
		},
		Key: "ping/dup", Registration: "ping", Storage: "second", Language: plg.LanguageGo,
		Status: plg.StatusRejected, RejectedVersion: "2", Owner: other,
	}, dup, "described as the second storage lists it")

	invalid := info(t, ctr, "ping/owner")
	require.NotNil(t, invalid.Rejection)
	require.ErrorIs(t, invalid.Rejection.Err, plg.ErrInvalidDescriptor)
	assert.Equal(t, plg.PluginState{
		Rejection: &plg.Report{At: invalid.Rejection.At, Err: invalid.Rejection.Err, Key: "ping/owner", Version: "1"},
		Key:       "ping/owner", Registration: "ping", Storage: "first", Language: plg.LanguageGo,
		Status: plg.StatusRejected, RejectedVersion: "1",
	}, invalid, "an invalid owner is left out")

	ts := plg.PluginState{
		Key: "ping/ts", Registration: "ping", Storage: "second", Language: plg.LanguageTypeScript,
		Status: plg.StatusSkipped,
	}
	assert.Equal(t, ts, info(t, ctr, "ping/ts"), "skipped by a reload")
	assert.Equal(t, lua, info(t, ctr, "ping/lua"))
	assert.Equal(t, []plg.PluginState{dup, lua, invalid, ts}, plg.Plugins(ctr))

	for _, key := range []string{"ping/none", "ping", "none/lua", ""} {
		_, err = plg.Info(ctr, key)
		require.ErrorIs(t, err, plg.ErrNotFound, key)
	}

	first.Delete("dup")
	second.Delete("dup")
	first.Delete("owner")
	second.Delete("ts")
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "ping"))

	for _, key := range []string{"ping/dup", "ping/owner", "ping/ts"} {
		_, err = plg.Info(ctr, key)
		require.ErrorIs(t, err, plg.ErrNotFound, "%s is no longer listed", key)
	}

	assert.Equal(t, []plg.PluginState{lua}, plg.Plugins(ctr))
}

// TestInfoOfAClosedContainer fails with ErrClosed, and Plugins lists nothing, once the container is closed.
func TestInfoOfAClosedContainer(t *testing.T) {
	t.Parallel()

	store := plg.NewMemoryStorage("store")
	require.NoError(t, store.Put(&plg.Descriptor{Key: "lua", Language: plg.LanguageLua, Entrypoint: "plugin"},
		fstest.MapFS{"plugin": {}}))

	ctr := plg.New()
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(store),
		plg.WithGolangPlugins()))
	require.Len(t, plg.Plugins(ctr), 1)
	require.NoError(t, ctr.Close(t.Context()))

	_, err := plg.Info(ctr, "ping/lua")
	require.ErrorIs(t, err, plg.ErrClosed)
	assert.Nil(t, plg.Plugins(ctr))
}

// info returns what plg.Info says of the plugin with the given key.
func info(t *testing.T, ctr *plg.Container, key string) plg.PluginState {
	t.Helper()

	state, err := plg.Info(ctr, key)
	require.NoError(t, err)

	return state
}
