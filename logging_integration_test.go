//go:build integration

package plg_test

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// panickingOn is a slog handler that panics on every record whose message it holds and discards the others. It
// takes every level.
type panickingOn map[string]bool

func (panickingOn) Enabled(context.Context, slog.Level) bool { return true }
func (h panickingOn) WithAttrs([]slog.Attr) slog.Handler     { return h }
func (h panickingOn) WithGroup(string) slog.Handler          { return h }

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (h panickingOn) Handle(_ context.Context, rec slog.Record) error {
	if h[rec.Message] {
		panic("logger exploded")
	}

	return nil
}

// Lua entrypoints added to the Lua fooer test plugin: closing makes the instance's Close fail, broken makes
// Create fail.
const (
	closingLua = `require("plugin")
local create = Create
function Create()
  local inst = create()
  inst.Close = function() error("close failed on purpose") end
  return inst
end
`
	brokenLua = `function Create() error("create failed on purpose") end`
)

// putLua puts the Lua fooer test plugin under key in store, at version, run from entry: its own plugin.lua, or a
// file of source added to it.
func putLua(t *testing.T, store *plg.MemoryStorage, key, version, entry, source string) {
	t.Helper()

	files := fstest.MapFS{}
	base := luaLanguage().files("fooer")

	require.NoError(t, fs.WalkDir(base, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		data, err := fs.ReadFile(base, name)
		files[name] = &fstest.MapFile{Data: data}

		return err //nolint:wrapcheck // the test tree's own error
	}))

	if entry != "plugin.lua" {
		files[entry] = &fstest.MapFile{Data: []byte(source)}
	}

	desc := plg.Descriptor{Key: key, Language: plg.LanguageLua, Entrypoint: entry, Version: version}
	require.NoError(t, store.Put(&desc, files))
}

// handlerPanics returns the PanicErrors of the logger's handler among panics, by key, failing on any other.
func handlerPanics(t *testing.T, panics []*plg.PanicError) map[string]int {
	t.Helper()

	keys := map[string]int{}

	for _, pe := range panics {
		assert.Equal(t, plg.PhaseCallback, pe.Phase, "%s: %v", pe.Key, pe.Value)
		assert.Equal(t, "slog.Handler.Handle", pe.Method, pe.Key)
		assert.Equal(t, "logger exploded", pe.Value, pe.Key)

		keys[pe.Key]++
	}

	return keys
}

// TestAPanickingLoggerOnASkippedPluginLeavesNoNameReserved registers a Lua plugin with Go plugins only enabled,
// with a logger panicking on the skipped plugin's record: the panic is a PanicError in PhaseCallback under the
// plugin's key, and the Register goes on, here to fail on its Go plugin (over the memory budget), leaving its name
// free, so that registering it again succeeds.
func TestAPanickingLoggerOnASkippedPluginLeavesNoNameReserved(t *testing.T) {
	t.Parallel()

	ctr := newContainer(
		t,
		plg.WithLogger(slog.New(panickingOn{"plg: plugin skipped: its language is not enabled": true})),
		plg.WithMemoryBudget(1<<20),
	)

	store := plg.NewMemoryStorage("mixed")
	putLua(t, store, "a", "v1", "plugin.lua", "")
	putVersioned(t, store, "b", "v1")

	var err error

	require.NotPanics(t, func() {
		err = plg.Register[testcontract.Fooer](t.Context(), ctr, "langs", plg.WithStorage(store),
			plg.WithGolangPlugins())
	})
	require.ErrorIs(t, err, plg.ErrMemoryBudget)
	assert.Equal(t, map[string]int{"langs/a": 1}, handlerPanics(t, ctr.RecentPanics()))

	store.Delete("b")
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "langs", plg.WithStorage(store),
		plg.WithGolangPlugins()), "the name is free")
	assert.Equal(t, map[string]int{"langs/a": 2}, handlerPanics(t, ctr.RecentPanics()))
}

// TestAPanickingLoggerInAReloadLeavesTheReloaderRunning reloads a new version of a plugin whose Close fails
// alongside a new plugin that fails to load, with a logger panicking on the failed plugin's record and on the
// cleanup of an unpublished version: the panic is a PanicError in PhaseCallback, the round publishes the new
// version and returns the failure, and the reloader serves the next rounds.
func TestAPanickingLoggerInAReloadLeavesTheReloaderRunning(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t, plg.WithLogger(slog.New(panickingOn{
		"plg: reload skipped a new plugin that failed to load": true,
		"plg: closing an unpublished plugin failed":            true,
	})))

	store := plg.NewMemoryStorage("lua")
	putLua(t, store, "a", "v1", "plugin.lua", "")
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "r", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	putLua(t, store, "a", "v2", "closing.lua", closingLua)
	putLua(t, store, "b", "v1", "broken.lua", brokenLua)

	for round := 1; round <= 2; round++ {
		err := plg.ReloadInterface(t.Context(), ctr, "r")
		require.ErrorIs(t, err, plg.ErrLoad, "round %d", round)
		require.ErrorContains(t, err, "create failed on purpose", "round %d", round)
		assert.NotErrorAs(t, err, new(*plg.PanicError), "round %d: the plugin's failure, not a panic", round)
		assert.Equal(t, map[string]int{"r/b": round}, handlerPanics(t, ctr.RecentPanics()), "round %d", round)
	}

	got, err := getFooer(t, ctr, "r/a").Foo(t.Context(), "v2")
	require.NoError(t, err)
	assert.Equal(t, "hello, v2", got, "the new version serves")

	store.Delete("b")
	putLua(t, store, "a", "v3", "plugin.lua", "")
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "r"), "the reloader serves the next round")
	require.NoError(t, plg.AwaitRetired(t.Context(), ctr))
	assert.Equal(t, 1, plg.Instances(ctr), "v3's instance only")
}

// gateOneOpen is a memory storage whose Open of the plugin gated reports that it was entered, then waits for its
// context to end.
type gateOneOpen struct {
	*plg.MemoryStorage

	entered chan struct{}
	gated   string
}

func (s *gateOneOpen) Open(ctx context.Context, desc *plg.Descriptor) (fs.FS, error) {
	if desc.Key == s.gated {
		close(s.entered)

		<-ctx.Done()

		return nil, fmt.Errorf("gated open: %w", context.Cause(ctx))
	}

	return s.MemoryStorage.Open(ctx, desc) //nolint:wrapcheck // the memory storage's own error, unchanged
}

// TestAPanickingLoggerCleaningUpAReloadIsContained closes the container while a reload, having loaded a new
// version of a plugin whose Close fails, waits on another plugin's Open, with a logger panicking on the cleanup of
// the unpublished version: the round closes that version, its failure's record is a PanicError in PhaseCallback
// under its key, the reload fails with ErrClosed, and Close returns with nothing left open.
func TestAPanickingLoggerCleaningUpAReloadIsContained(t *testing.T) {
	t.Parallel()

	ctr := plg.New(plg.WithLogger(slog.New(panickingOn{"plg: closing an unpublished plugin failed": true})))

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) }) // when the test fails before its own

	store := &gateOneOpen{MemoryStorage: plg.NewMemoryStorage("lua"), entered: make(chan struct{}), gated: "b"}

	putLua(t, store.MemoryStorage, "a", "v1", "plugin.lua", "")
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "c", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	putLua(t, store.MemoryStorage, "a", "v2", "closing.lua", closingLua)
	putLua(t, store.MemoryStorage, "b", "v1", "plugin.lua", "")

	reloaded := make(chan error, 1)

	go func() { reloaded <- plg.ReloadInterface(context.Background(), ctr, "c") }()

	<-store.entered // a's v2 is loaded, b's Open waits for the round's context
	require.NoError(t, ctr.Close(t.Context()))
	require.ErrorIs(t, <-reloaded, plg.ErrClosed)

	assert.Equal(t, map[string]int{"c/a": 1}, handlerPanics(t, ctr.RecentPanics()))
	assert.Zero(t, plg.Instances(ctr))
	assert.Zero(t, plg.MemoryInUse(ctr))
	assert.Zero(t, plg.MemoryReserved(ctr))
}

// TestAPanickingLoggerOnACompileLeavesTheGuestCompiled registers Lua plugins under two names with a logger
// panicking on the guest's compile record: the panic is a PanicError in PhaseCallback, with no key, and both
// Registers succeed, the second on the guest the first compiled.
func TestAPanickingLoggerOnACompileLeavesTheGuestCompiled(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t, plg.WithLogger(slog.New(panickingOn{"plg: guest compiled": true})))
	store := plg.NewMemoryStorage("lua")
	putLua(t, store, "a", "v1", "plugin.lua", "")

	for _, name := range []string{"one", "two"} {
		require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, name, plg.WithStorage(store),
			plg.WithPool(1, 1)), name)

		got, err := getFooer(t, ctr, name+"/a").Foo(t.Context(), name)
		require.NoError(t, err, name)
		assert.Equal(t, "hello, "+name, got)
	}

	assert.Equal(t, map[string]int{"": 1}, handlerPanics(t, ctr.RecentPanics()), "one compile")
}

// panickingOnce is a slog handler that panics on the first record with the message msg and discards the others.
type panickingOnce struct {
	panicked *atomic.Bool
	msg      string
}

func (panickingOnce) Enabled(context.Context, slog.Level) bool { return true }
func (h panickingOnce) WithAttrs([]slog.Attr) slog.Handler     { return h }
func (h panickingOnce) WithGroup(string) slog.Handler          { return h }

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (h panickingOnce) Handle(_ context.Context, rec slog.Record) error {
	if rec.Message == h.msg && h.panicked.CompareAndSwap(false, true) {
		panic("logger exploded")
	}

	return nil
}

// TestACompileUnwoundByAPanicIsForgotten has a panic escape the guest's compile record (the logger unguarded, as
// it was before logging was guarded): the Register fails with a PanicError in PhaseLoad, and a later Register
// compiles the guest anew and serves, rather than finding the interrupted compile's module missing.
func TestACompileUnwoundByAPanicIsForgotten(t *testing.T) {
	t.Parallel()

	logger := panickingOnce{panicked: new(atomic.Bool), msg: "plg: guest compiled"}
	ctr := newContainer(t, plg.WithLogger(slog.New(logger)))
	plg.UnguardLogger(ctr, true)

	store := plg.NewMemoryStorage("lua")
	putLua(t, store, "a", "v1", "plugin.lua", "")

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "one", plg.WithStorage(store), plg.WithPool(1, 1))
	require.ErrorIs(t, err, plg.ErrLoad)
	assert.Equal(t, "logger exploded", requirePanic(t, err, "one/a", plg.PhaseLoad).Value)

	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "two", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	got, err := getFooer(t, ctr, "two/a").Foo(t.Context(), "again")
	require.NoError(t, err)
	assert.Equal(t, "hello, again", got)
}
