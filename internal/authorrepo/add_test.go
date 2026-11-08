package authorrepo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/authorrepo"
)

// readFile returns the content of the file name.
func readFile(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(name) //nolint:gosec // G304: the test's own file
	require.NoError(t, err)

	return string(data)
}

// TestAddPluginKeepsTheFile adds a plugin to a plg.yaml listing its plugins in a block sequence: the new plugin
// follows the last one at its indentation, every comment and other line unchanged, and the repository lists it.
// Adding it again changes nothing.
func TestAddPluginKeepsTheFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, map[string]string{
		"plg.yaml": "# rules\nplugins:\n    - gold   # the first\n    - rules/silver\nshared: [lib]   # shared\n",
		"gold/":    "", "rules/silver/": "", "lib/": "", "rules/bronze/": "",
	})

	repo, err := authorrepo.Load(dir)
	require.NoError(t, err)
	require.NoError(t, repo.AddPlugin(filepath.Join(dir, "rules", "bronze")))
	assert.Equal(t, []string{"gold", "rules/silver", "rules/bronze"}, repo.Plugins)

	want := "# rules\nplugins:\n    - gold   # the first\n    - rules/silver\n    - rules/bronze\nshared: [lib]   # shared\n"
	assert.Equal(t, want, readFile(t, filepath.Join(dir, "plg.yaml")))

	require.NoError(t, repo.AddPlugin(filepath.Join(dir, "rules", "bronze")))
	assert.Equal(t, want, readFile(t, filepath.Join(dir, "plg.yaml")))
}

// TestAddPluginToAFlowSequence adds a plugin to plugins written as a flow sequence, and to a last line without a
// newline: both read back with the plugin listed.
func TestAddPluginToAFlowSequence(t *testing.T) {
	t.Parallel()

	for _, content := range []string{"plugins: [gold]\n", "plugins:\n  - gold"} {
		dir := t.TempDir()
		write(t, dir, map[string]string{"plg.yaml": content, "gold/": "", "silver/": ""})

		repo, err := authorrepo.Load(dir)
		require.NoError(t, err)
		require.NoError(t, repo.AddPlugin(filepath.Join(dir, "silver")), content)

		reread, err := authorrepo.Load(dir)
		require.NoError(t, err)
		assert.Equal(t, []string{"gold", "silver"}, reread.Plugins, content)
	}
}

// TestAddPluginRefusals refuses a directory outside the repository and a plugin whose key is another's, which
// leaves plg.yaml as it was.
func TestAddPluginRefusals(t *testing.T) {
	t.Parallel()

	const listed = "plugins:\n  - gold\n"

	dir := t.TempDir()

	write(t, dir, map[string]string{"plg.yaml": listed, "gold/": "", "other/gold/": ""})

	repo, err := authorrepo.Load(dir)
	require.NoError(t, err)

	err = repo.AddPlugin(t.TempDir())
	require.ErrorIs(t, err, authorrepo.ErrInvalid)
	require.ErrorContains(t, err, "is not inside the repository")

	err = repo.AddPlugin(filepath.Join(dir, "other", "gold"))
	require.ErrorIs(t, err, authorrepo.ErrInvalid)
	require.ErrorContains(t, err, "have the same key")
	assert.Equal(t, listed, readFile(t, filepath.Join(dir, "plg.yaml")))
	assert.Equal(t, []string{"gold"}, repo.Plugins)
}

// TestAddPluginFailsOnAChangedFile adds a plugin after plg.yaml was removed, then after it was rewritten without
// plugins, then as no YAML: each fails, naming the file.
func TestAddPluginFailsOnAChangedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, map[string]string{"plg.yaml": "plugins: [gold]\n", "gold/": "", "silver/": ""})

	repo, err := authorrepo.Load(dir)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, "plg.yaml")))

	err = repo.AddPlugin(filepath.Join(dir, "silver"))
	require.ErrorIs(t, err, authorrepo.ErrInvalid)
	require.ErrorContains(t, err, "plg.yaml")

	write(t, dir, map[string]string{"plg.yaml": "shared: [gold]\n"})

	err = repo.AddPlugin(filepath.Join(dir, "silver"))
	require.ErrorIs(t, err, authorrepo.ErrInvalid)
	require.ErrorContains(t, err, "plugins must list at least one plugin directory")

	write(t, dir, map[string]string{"plg.yaml": "plugins: [gold\n"})

	err = repo.AddPlugin(filepath.Join(dir, "silver"))
	require.ErrorIs(t, err, authorrepo.ErrInvalid)
	require.ErrorContains(t, err, "plg.yaml")
}
