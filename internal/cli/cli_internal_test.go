package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveReturnsCleanAbsolutePaths: a path given to a command resolves against the working directory to a
// clean absolute path, whether that directory is given relative or absolute, so "." names the directory itself
// (its last element is the plugin's key) and git's pathspecs and the author repository's directory compare with
// it; an empty path stays empty (a flag not given). With the working directory gone, a relative path stays
// relative, cleaned.
//
//nolint:paralleltest // t.Chdir: the working directory is the process's
func TestResolveReturnsCleanAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	for _, c := range []struct{ dir, path, want string }{
		{".", ".", root},
		{".", "rules/vip/", filepath.Join(root, "rules", "vip")},
		{"rules", "../lib", filepath.Join(root, "lib")},
		{root, "rules/./vip", filepath.Join(root, "rules", "vip")},
		{"rules", "/elsewhere/x/..", "/elsewhere"},
		{".", "", ""},
	} {
		assert.Equal(t, c.want, resolve(c.dir, c.path), "%s %s", c.dir, c.path)
	}

	gone := filepath.Join(root, "gone")
	require.NoError(t, os.Mkdir(gone, 0o750))
	t.Chdir(gone)
	require.NoError(t, os.Remove(gone))

	assert.Equal(t, filepath.Join("rules", "vip"), resolve(".", "rules/./vip"))
}
