package plg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

// link is a symbolic link of an fstest.MapFS.
func link(target string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
}

// noLinkFS is an fs.FS reporting symbolic links that cannot read their targets: it is no fs.ReadLinkFS.
type noLinkFS struct {
	fs.FS
}

// fileNames returns the names of every file below the root of files.
func fileNames(t *testing.T, files fs.FS) []string {
	t.Helper()

	var names []string

	require.NoError(t, fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			names = append(names, name)
		}

		return err
	}))

	return names
}

func TestPluginFilesNeverFollowSymlinks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		files fs.FS
		name  string
		want  string // the error, or "" when the links are skipped
	}{
		{
			name:  "absolute target",
			files: fstest.MapFS{"a.go": {}, "x.txt": link("/proc/self/environ")},
			want:  "x.txt: symbolic link out of the plugin's files (to /proc/self/environ)",
		},
		{
			name:  "to the parent",
			files: fstest.MapFS{"a.go": {}, "up": link("..")},
			want:  "up: symbolic link out of the plugin's files (to ..)",
		},
		{
			name:  "up from a subdirectory",
			files: fstest.MapFS{"a.go": {}, "sub/x.txt": link("../../secret")},
			want:  "sub/x.txt: symbolic link out of the plugin's files (to ../../secret)",
		},
		{
			name:  "back in from outside",
			files: fstest.MapFS{"p/a.go": {}, "p/x.go": link("../../p/a.go")},
			want:  "p/x.go: symbolic link out of the plugin's files",
		},
		{
			name:  "unreadable target",
			files: noLinkFS{fstest.MapFS{"a.go": {}, "x.go": link("a.go")}},
			want:  "x.go: symbolic link out of the plugin's files: reading its target",
		},
		{
			name: "inside",
			files: fstest.MapFS{
				"a.go": {}, "x.go": link("a.go"), "self": link("."), "sub/y.go": link("../a.go"),
				"sub/z": link("./../sub"), "w": link("sub/../a.go"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := plg.NewMemoryStorage("mem")
			desc := plg.Descriptor{Key: "k", Language: plg.LanguageGo, Entrypoint: "."}

			err := store.Put(desc, tc.files)
			if tc.want != "" {
				require.ErrorContains(t, err, "plg: memory storage mem: plugin k: copying the plugin's files: "+tc.want)

				return
			}

			require.NoError(t, err)

			files, err := store.Open(t.Context(), desc)
			require.NoError(t, err)
			assert.Equal(t, []string{"a.go"}, fileNames(t, files), "links inside the files are skipped")
		})
	}
}

func TestFSStorageReadsThroughARoot(t *testing.T) {
	t.Parallel()

	outside := filepath.Join(t.TempDir(), "plugin.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{"language":"go","entrypoint":".","version":"1"}`), 0o600))

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "p"), 0o750))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "p", "plugin.json")))

	// os.DirFS would read the manifest through the link.
	_, err := plg.NewFSStorage(dir).List(t.Context())
	require.ErrorIs(t, err, plg.ErrInvalidManifest)
	require.ErrorContains(t, err, "plugin directory p")
	require.ErrorContains(t, err, "path escapes from parent")

	require.NoError(t, os.Remove(filepath.Join(dir, "p", "plugin.json")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p", "a.go"), []byte("package p\n"), 0o600))
	require.NoError(t, os.Symlink("a.go", filepath.Join(dir, "p", "b.go")))
	require.NoError(t, os.Symlink(filepath.Dir(outside), filepath.Join(dir, "p", "out")))
	require.NoError(t, os.Symlink("p", filepath.Join(dir, "q")))

	store := plg.NewFSStorage(dir)

	_, err = store.Open(t.Context(), plg.Descriptor{Key: "p"})
	require.ErrorContains(t, err, "plugin directory p: copying the plugin's files: out: symbolic link out of the "+
		"plugin's files")

	_, err = store.Open(t.Context(), plg.Descriptor{Key: "q"})
	require.ErrorContains(t, err, "plugin directory q: a plugin key is a directory name",
		"a link to a plugin directory is not one, as List does not list it")

	require.NoError(t, os.Remove(filepath.Join(dir, "p", "out")))

	files, err := store.Open(t.Context(), plg.Descriptor{Key: "p"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a.go"}, fileNames(t, files), "the link inside the directory is skipped")
}
