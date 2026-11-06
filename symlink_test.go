package plg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

	outside := filepath.Join(t.TempDir(), "plugin.yaml")
	require.NoError(t, os.WriteFile(outside, []byte("language: go\nentrypoint: .\nversion: \"1\"\n"), 0o600))

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "p"), 0o750))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "p", "plugin.yaml")))

	// os.DirFS would read the manifest through the link.
	_, err := plg.NewFSStorage(dir).List(t.Context())
	require.ErrorIs(t, err, plg.ErrInvalidManifest)
	require.ErrorContains(t, err, "plugin directory p")
	require.ErrorContains(t, err, "path escapes from parent")

	require.NoError(t, os.Remove(filepath.Join(dir, "p", "plugin.yaml")))
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

// secretContent is what every file next to the plugin directory of swapRoot holds: no copy of the plugin may
// hold it.
const secretContent = "next to the plugin"

// swapFiles returns the files of the plugin directory p of swapRoot, but those named in left out: each holds its
// own name.
func swapFiles(left ...string) map[string]string {
	files := map[string]string{}

	for _, name := range []string{"plugin.yaml", "other/x.txt", "sub/x.txt", "x.txt", "y.txt"} {
		if !slices.Contains(left, name) {
			files[name] = name
		}
	}

	return files
}

// swapRoot returns a directory of plugins holding the plugin directory p (swapFiles), and next to it secret.txt
// and a directory secret holding the same files as p, all holding secretContent.
func swapRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret.txt"), []byte(secretContent), 0o600))

	for name := range swapFiles() {
		for dir, content := range map[string]string{"p": name, "secret": secretContent} {
			file := filepath.Join(root, dir, filepath.FromSlash(name))
			require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o750))
			require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
		}
	}

	return root
}

// copiedFiles returns the contents of every file of files, by name.
func copiedFiles(t *testing.T, files fs.FS) map[string]string {
	t.Helper()

	copied := map[string]string{}

	for _, name := range fileNames(t, files) {
		data, err := fs.ReadFile(files, name)
		require.NoError(t, err)

		copied[name] = string(data)
	}

	return copied
}

// swapLink returns a swap replacing the entry it is given with a symbolic link to target.
func swapLink(target string) func(t *testing.T, name string) {
	return func(t *testing.T, name string) {
		t.Helper()

		require.NoError(t, os.RemoveAll(name))
		require.NoError(t, os.Symlink(target, name))
	}
}

// swapFIFO replaces the entry it is given with a FIFO: opening it read-only would wait for a writer forever.
func swapFIFO(t *testing.T, name string) {
	t.Helper()

	require.NoError(t, os.RemoveAll(name))
	require.NoError(t, syscall.Mkfifo(name, 0o600))
}

// swapCase swaps an entry of the plugin directory of a swapRoot while FSStorage.Open copies it: right before the
// copy opens the entry, or the first entry below it (right before the walk of the plugin's files opens it, for
// walk).
type swapCase struct {
	is    error // what the error wraps, when want is not empty
	swap  func(t *testing.T, name string)
	files map[string]string // the files copied, when want is empty
	name  string
	entry string // the entry swapped, relative to the plugin directory ("." for the plugin directory)
	want  string // the error, after "copying the plugin's files: "
	root  bool   // whether root skips the case, since root reads unreadable files
	walk  bool   // whether the swap happens before the walk opens the entry, not before the copy does
}

// run copies the plugin directory of a new swapRoot, swapping tc's entry, and checks the copy.
func (tc *swapCase) run(t *testing.T) {
	t.Helper()

	if tc.root && os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}

	root := swapRoot(t)
	swapped := false

	swapping := plg.OpenSwapping
	if tc.walk {
		swapping = plg.WalkSwapping
	}

	files, err := swapping(t.Context(), plg.NewFSStorage(root), plg.Descriptor{Key: "p"}, func(name string) {
		if !swapped && (tc.entry == "." || name == tc.entry || strings.HasPrefix(name, tc.entry+"/")) {
			swapped = true

			tc.swap(t, filepath.Join(root, "p", filepath.FromSlash(tc.entry)))
		}
	})
	require.True(t, swapped, "the copy opened the entry swapped")

	if tc.want == "" {
		require.NoError(t, err)
		assert.Equal(t, tc.files, copiedFiles(t, files), "only the plugin's own files are copied")

		return
	}

	require.ErrorContains(t, err, "plg: plugin directory p: copying the plugin's files: "+tc.want)
	assert.NotContains(t, err.Error(), secretContent)

	if tc.is != nil {
		require.ErrorIs(t, err, tc.is)
	}
}

// TestFSStorageCopyNeverFollowsASwappedLink swaps an entry of a plugin directory after the walk of its files,
// right before the copy opens it (or, for a directory or the plugin directory, a file below it): an entry that
// became a symbolic link is judged as the walk judges links, with the same errors, whether it is a file or a
// directory, and never followed; one replaced otherwise fails the copy, a FIFO (which a read-only open would
// wait on for a writer forever) at once, also when a directory becomes one before the walk reads it; nothing
// next to the plugin is ever copied.
func TestFSStorageCopyNeverFollowsASwappedLink(t *testing.T) {
	t.Parallel()

	tests := []swapCase{
		{
			name:  "a file to a link out of the plugin",
			entry: "x.txt",
			swap:  swapLink("../secret.txt"),
			want:  "x.txt: symbolic link out of the plugin's files (to ../secret.txt)",
		},
		{
			name:  "a file to an absolute link",
			entry: "x.txt",
			swap: func(t *testing.T, name string) {
				t.Helper()
				swapLink(filepath.Join(filepath.Dir(filepath.Dir(name)), "secret.txt"))(t, name)
			},
			want: "x.txt: symbolic link out of the plugin's files (to /",
		},
		{
			name:  "a directory to a link out of the plugin",
			entry: "sub",
			swap:  swapLink("../secret/sub"),
			want:  "sub: symbolic link out of the plugin's files (to ../secret/sub)",
		},
		{
			name:  "the plugin directory, after the walk",
			entry: ".",
			swap: func(t *testing.T, name string) {
				t.Helper()
				require.NoError(t, os.Rename(name, name+".moved"))
				require.NoError(t, os.Symlink("secret", name))
			},
			files: swapFiles(),
		},
		{
			name:  "a file to a link inside the plugin",
			entry: "x.txt",
			swap:  swapLink("y.txt"),
			files: swapFiles("x.txt"),
		},
		{
			name:  "a directory to a link inside the plugin",
			entry: "sub",
			swap:  swapLink("other"),
			files: swapFiles("sub/x.txt"),
		},
		{
			name:  "a file to a directory",
			entry: "x.txt",
			swap: func(t *testing.T, name string) {
				t.Helper()
				require.NoError(t, os.Remove(name))
				require.NoError(t, os.Mkdir(name, 0o750))
			},
			want: "x.txt: replaced while the plugin's files were copied",
		},
		{
			name:  "a directory to a file",
			entry: "sub",
			swap: func(t *testing.T, name string) {
				t.Helper()
				require.NoError(t, os.RemoveAll(name))
				require.NoError(t, os.WriteFile(name, []byte(secretContent), 0o600))
			},
			want: "sub: replaced while the plugin's files were copied",
		},
		{
			name:  "a file to a FIFO",
			entry: "x.txt",
			swap:  swapFIFO,
			want:  "x.txt: replaced while the plugin's files were copied",
		},
		{
			name:  "a directory to a FIFO",
			entry: "sub",
			swap:  swapFIFO,
			want:  "sub: replaced while the plugin's files were copied",
		},
		{
			name:  "a directory to a FIFO, before the walk reads it",
			entry: "sub",
			swap:  swapFIFO,
			want:  "sub: replaced while the plugin's files were copied",
			walk:  true,
		},
		{
			name:  "a removed file",
			entry: "x.txt",
			swap: func(t *testing.T, name string) {
				t.Helper()
				require.NoError(t, os.Remove(name))
			},
			want: "reading x.txt: ",
			is:   fs.ErrNotExist,
		},
		{
			name:  "an unreadable file",
			entry: "x.txt",
			swap: func(t *testing.T, name string) {
				t.Helper()
				require.NoError(t, os.Chmod(name, 0))
			},
			want: "reading x.txt: ",
			is:   fs.ErrPermission,
			root: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}
