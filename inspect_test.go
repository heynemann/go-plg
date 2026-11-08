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
// not enable and one implementing another interface: Info describes both as skipped, each with its reason, its
// storage, its language and its owner. A reload then finds a plugin in another such language, a key listed by two
// storages and a descriptor whose owner breaks the limits: the first is skipped too, the others rejected, each
// described with why, its storage, its language and its owner unless the owner is what is wrong, until the
// storages stop listing them. Plugins lists them all in key order. None needs a guest.
func TestInfoDescribesPluginsNoVersionServes(t *testing.T) {
	t.Parallel()

	owner := plg.Owner{Team: plg.Team{Name: "payments"}, Contact: plg.Contact{Email: "payments@example.com"}}
	other := plg.Owner{Team: plg.Team{Name: "search"}}
	first, second := plg.NewMemoryStorage("first"), plg.NewMemoryStorage("second")
	put := func(store *plg.MemoryStorage, key, implements string, lang plg.Language, version string, by *plg.Owner) {
		t.Helper()

		require.NoError(t, store.Put(&plg.Descriptor{
			Key: key, Implements: implements, Language: lang, Entrypoint: "plugin", Version: version, Owner: by,
		}, fstest.MapFS{"plugin": {}}))
	}
	// skipped is the state of a plugin skipped for the reason want, as got describes it: when its listing was.
	skipped := func(got, state plg.PluginState, want error) plg.PluginState {
		t.Helper()

		require.NotNil(t, got.Rejection, state.Key)
		require.ErrorIs(t, got.Rejection.Err, want, state.Key)

		state.Status, state.RejectedVersion = plg.StatusSkipped, got.Rejection.Version
		state.Rejection = &plg.Report{
			At:      got.Rejection.At,
			Err:     got.Rejection.Err,
			Owner:   state.Owner,
			Key:     state.Key,
			Version: state.RejectedVersion,
		}

		return state
	}

	put(first, "lua", implementsOf[pinger](), plg.LanguageLua, "3", &owner)
	put(second, "cohort", "example.com/contracts/rewards.Cohort@v1.2.0", plg.LanguageGo, "7", &other)

	ctr := newUnitContainer(t)
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(first),
		plg.WithStorage(second), plg.WithGolangPlugins()))

	lua := skipped(info(t, ctr, "ping/lua"), plg.PluginState{
		Key: "ping/lua", Registration: "ping", Storage: "first", Language: plg.LanguageLua, Owner: owner,
	}, plg.ErrUnsupportedLanguage)
	assert.Equal(t, "3", lua.RejectedVersion)
	assert.Equal(t, lua, info(t, ctr, "ping/lua"), "skipped at Register: its language is not enabled")
	require.NotErrorIs(t, lua.Rejection.Err, plg.ErrOtherInterface)
	require.EqualError(t, lua.Rejection.Err, "plg: unsupported language: lua plugins are not enabled for ping")

	cohort := skipped(info(t, ctr, "ping/cohort"), plg.PluginState{
		Key: "ping/cohort", Registration: "ping", Storage: "second", Language: plg.LanguageGo, Owner: other,
	}, plg.ErrOtherInterface)
	assert.Equal(t, cohort, info(t, ctr, "ping/cohort"), "skipped at Register: it implements another interface")
	require.NotErrorIs(t, cohort.Rejection.Err, plg.ErrUnsupportedLanguage)
	require.EqualError(t, cohort.Rejection.Err, "plg: plugin implements another interface: cohort implements "+
		"example.com/contracts/rewards.Cohort, not github.com/heynemann/go-plg_test.pinger")
	assert.Equal(t, []plg.PluginState{cohort, lua}, plg.Plugins(ctr))

	put(second, "ts", implementsOf[pinger](), plg.LanguageTypeScript, "", nil)
	put(first, "dup", implementsOf[pinger](), plg.LanguageGo, "1", &owner)
	put(second, "dup", implementsOf[pinger](), plg.LanguageGo, "2", &other)

	tooLong := plg.Owner{Team: plg.Team{Name: strings.Repeat("x", plg.MaxOwnerStringBytes+1)}}
	put(first, "owner", implementsOf[pinger](), plg.LanguageGo, "1", &tooLong)

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

	tsState := skipped(info(t, ctr, "ping/ts"), plg.PluginState{
		Key: "ping/ts", Registration: "ping", Storage: "second", Language: plg.LanguageTypeScript,
	}, plg.ErrUnsupportedLanguage)
	assert.False(t, tsState.Rejection.At.Before(before) || tsState.Rejection.At.After(after), "the reload's listing")
	assert.Equal(t, tsState, info(t, ctr, "ping/ts"), "skipped by a reload")

	lua = skipped(info(t, ctr, "ping/lua"), lua, plg.ErrUnsupportedLanguage)
	assert.False(t, lua.Rejection.At.Before(before), "skipped again by the reload")

	cohort = skipped(info(t, ctr, "ping/cohort"), cohort, plg.ErrOtherInterface)
	assert.Equal(t, []plg.PluginState{cohort, dup, lua, invalid, tsState}, plg.Plugins(ctr))

	for _, key := range []string{"ping/none", "ping", "none/lua", ""} {
		_, err = plg.Info(ctr, key)
		require.ErrorIs(t, err, plg.ErrNotFound, key)
	}

	first.Delete("dup")
	second.Delete("dup")
	first.Delete("owner")
	second.Delete("ts")
	second.Delete("cohort")
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "ping"))

	for _, key := range []string{"ping/dup", "ping/owner", "ping/ts", "ping/cohort"} {
		_, err = plg.Info(ctr, key)
		require.ErrorIs(t, err, plg.ErrNotFound, "%s is no longer listed", key)
	}

	assert.Equal(t, []plg.PluginState{skipped(info(t, ctr, "ping/lua"), lua, plg.ErrUnsupportedLanguage)},
		plg.Plugins(ctr))
}

// TestInfoOfAClosedContainer fails with ErrClosed, and Plugins lists nothing, once the container is closed.
func TestInfoOfAClosedContainer(t *testing.T) {
	t.Parallel()

	store := plg.NewMemoryStorage("store")
	require.NoError(t, store.Put(&plg.Descriptor{
		Key: "lua", Implements: implementsOf[pinger](), Language: plg.LanguageLua, Entrypoint: "plugin",
	}, fstest.MapFS{"plugin": {}}))

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
