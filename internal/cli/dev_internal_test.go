package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWatchTicksReportsChangedFiles drives plg dev's polling watcher with explicit ticks: each tick reports the
// files created, changed and removed under the watched directory and the watched file since the last report, and
// nothing outside them; a tick with nothing changed sends nothing.
func TestWatchTicksReportsChangedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	plugin := filepath.Join(dir, "plugin")
	repo := filepath.Join(dir, "plg.yaml")
	outside := filepath.Join(dir, "outside.txt")

	require.NoError(t, os.MkdirAll(filepath.Join(plugin, "tests"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(plugin, "plugin.lua"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(repo, []byte("plugins: [plugin]\n"), 0o600))

	ticks := make(chan time.Time)
	changes := watchTicks(t.Context(), []string{plugin, repo}, ticks)

	tick := func() { ticks <- time.Time{} }

	test := filepath.Join(plugin, "tests", "example.yaml")
	require.NoError(t, os.WriteFile(test, []byte("cases: []\n"), 0o600))
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o600))
	tick()
	assert.Equal(t, []string{test}, <-changes)

	require.NoError(t, os.WriteFile(filepath.Join(plugin, "plugin.lua"), []byte("ab"), 0o600))
	require.NoError(t, os.WriteFile(repo, []byte("plugins: [plugin, other]\n"), 0o600))
	tick()
	assert.Equal(t, []string{repo, filepath.Join(plugin, "plugin.lua")}, <-changes) // sorted

	require.NoError(t, os.Remove(test))
	tick()
	assert.Equal(t, []string{test}, <-changes)

	for range 2 { // the watcher takes the second tick only if the first one, with nothing changed, sent nothing
		select {
		case ticks <- time.Time{}:
		case changed := <-changes:
			t.Fatalf("nothing changed, yet the watcher reports %v", changed)
		}
	}
}

// TestPollWatchReportsAChange runs plg dev's default watcher, which polls on its own ticker: a file created under the
// watched directory is reported, and the channel closes once the context ends.
func TestPollWatchReportsAChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	changes := pollWatch(ctx, []string{dir})

	file := filepath.Join(dir, "plugin.lua")
	require.NoError(t, os.WriteFile(file, []byte("a"), 0o600))
	assert.Equal(t, []string{file}, <-changes)

	cancel()

	for range changes { // drains until the watcher closes the channel
	}
}
