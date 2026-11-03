package memfs_test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/memfs"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func build(t *testing.T, files map[string]string) *memfs.FS {
	t.Helper()

	var b memfs.Builder

	for name, data := range files {
		require.NoError(t, b.Add(name, []byte(data)))
	}

	fsys, err := b.Build()
	require.NoError(t, err)

	return fsys
}

func TestConformsToFSSemantics(t *testing.T) {
	t.Parallel()

	fsys := build(t, map[string]string{
		"src/a/a.go":       "package a",
		"src/a/b/b.go":     "package b",
		"src/plg/x.go":     "package x",
		"top.txt":          "",
		"src/a/zz/deep.go": "package deep",
	})

	require.NoError(t, fstest.TestFS(fsys, "src/a/a.go", "src/a/b/b.go", "src/plg/x.go", "top.txt", "src/a/zz/deep.go"))
}

func TestEmptyTree(t *testing.T) {
	t.Parallel()

	fsys := build(t, nil)
	require.NoError(t, fstest.TestFS(fsys))

	entries, err := fs.ReadDir(fsys, ".")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestBuilderRejects(t *testing.T) {
	t.Parallel()

	var b memfs.Builder

	require.NoError(t, b.Add("a/b.go", nil))

	for _, name := range []string{"", ".", "/abs", "a/../b", "a//b", "a/"} {
		require.ErrorIs(t, b.Add(name, nil), memfs.ErrInvalidPath, "%q", name)
	}

	require.ErrorIs(t, b.Add("a/b.go", nil), memfs.ErrConflict, "added twice")

	for _, files := range [][]string{{"a", "a/b"}, {"x/y", "x/y/z"}} {
		var c memfs.Builder

		for _, f := range files {
			require.NoError(t, c.Add(f, nil))
		}

		_, err := c.Build()
		require.ErrorIs(t, err, memfs.ErrConflict, "%v: a path is both a file and a directory", files)
	}
}

func TestFilesAreSnapshots(t *testing.T) {
	t.Parallel()

	fsys := build(t, map[string]string{"f": "abc"})

	data, err := fsys.ReadFile("f")
	require.NoError(t, err)

	data[0] = 'X'

	again, err := fsys.ReadFile("f")
	require.NoError(t, err)
	assert.Equal(t, "abc", string(again), "readers cannot modify the tree")
}

func TestErrors(t *testing.T) {
	t.Parallel()

	fsys := build(t, map[string]string{"dir/f": "abc"})

	_, err := fsys.Open("missing")
	require.ErrorIs(t, err, fs.ErrNotExist)

	_, err = fsys.Open("/dir")
	require.ErrorIs(t, err, fs.ErrInvalid)

	_, err = fsys.ReadFile("dir")
	require.ErrorContains(t, err, "is a directory")

	_, err = fsys.ReadDir("dir/f")
	require.ErrorContains(t, err, "not a directory")

	_, err = fsys.Stat("nope")
	require.ErrorIs(t, err, fs.ErrNotExist)

	_, err = fsys.ReadDir("nope")
	require.ErrorIs(t, err, fs.ErrNotExist)

	dir, err := fsys.Open("dir")
	require.NoError(t, err)

	_, err = dir.Read(make([]byte, 1))
	require.ErrorContains(t, err, "is a directory")
}

func TestFileReadsAndSeeks(t *testing.T) {
	t.Parallel()

	fsys := build(t, map[string]string{"f": "hello"})

	f, err := fsys.Open("f")
	require.NoError(t, err)

	t.Cleanup(func() { assert.NoError(t, f.Close()) })

	seeker, ok := f.(io.ReadSeeker)
	require.True(t, ok)

	readerAt, ok := f.(io.ReaderAt)
	require.True(t, ok)

	buf := make([]byte, 3)

	n, err := readerAt.ReadAt(buf, 3)
	assert.Equal(t, 2, n)
	require.ErrorIs(t, err, io.EOF, "a short ReadAt reports EOF")

	_, err = readerAt.ReadAt(buf, -1)
	require.ErrorIs(t, err, fs.ErrInvalid)

	_, err = readerAt.ReadAt(buf, 5)
	require.ErrorIs(t, err, io.EOF)

	pos, err := seeker.Seek(-2, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(3), pos)

	pos, err = seeker.Seek(1, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(4), pos)

	rest, err := io.ReadAll(seeker)
	require.NoError(t, err)
	assert.Equal(t, "o", string(rest))

	_, err = seeker.Seek(-1, io.SeekStart)
	require.ErrorIs(t, err, fs.ErrInvalid)

	_, err = seeker.Seek(0, 42)
	require.ErrorIs(t, err, fs.ErrInvalid)

	pos, err = seeker.Seek(10, io.SeekStart)
	require.NoError(t, err, "seeking past the end is allowed")
	assert.Equal(t, int64(10), pos)

	n, err = seeker.Read(buf)
	assert.Zero(t, n)
	require.ErrorIs(t, err, io.EOF)
}

func TestReadDirPages(t *testing.T) {
	t.Parallel()

	fsys := build(t, map[string]string{"d/a": "", "d/b": "", "d/c": ""})

	d, err := fsys.Open("d")
	require.NoError(t, err)

	dirFile, ok := d.(fs.ReadDirFile)
	require.True(t, ok)

	var names []string

	for {
		page, pageErr := dirFile.ReadDir(2)
		for _, e := range page {
			names = append(names, e.Name())
		}

		if errors.Is(pageErr, io.EOF) {
			break
		}

		require.NoError(t, pageErr)
	}

	assert.Equal(t, []string{"a", "b", "c"}, names)

	rest, err := dirFile.ReadDir(-1)
	require.NoError(t, err)
	assert.Empty(t, rest, "a drained directory returns nothing, without error, for n <= 0")
}

var errDiskOnFire = errors.New("disk on fire")

// failingFS fails to read one file, to check AddTree propagates read errors.
type failingFS struct{ fstest.MapFS }

func (f failingFS) ReadFile(name string) ([]byte, error) {
	if name == "sub/bad" {
		return nil, errDiskOnFire
	}

	data, err := f.MapFS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("mapfs: %w", err)
	}

	return data, nil
}

func TestFileInfo(t *testing.T) {
	t.Parallel()

	fsys := build(t, map[string]string{"d/f.go": "package f"})

	file, err := fsys.Stat("d/f.go")
	require.NoError(t, err)
	assert.Equal(t, "f.go", file.Name())
	assert.Equal(t, int64(9), file.Size())
	assert.Equal(t, fs.FileMode(0o444), file.Mode(), "files are read-only")
	assert.False(t, file.IsDir())
	assert.True(t, file.ModTime().IsZero(), "no timestamps, so snapshots of equal content are identical")
	assert.Nil(t, file.Sys())

	dir, err := fsys.Stat("d")
	require.NoError(t, err)
	assert.Equal(t, fs.ModeDir|0o555, dir.Mode(), "directories are read-only")
	assert.True(t, dir.IsDir())
}

func TestAddTree(t *testing.T) {
	t.Parallel()

	src := fstest.MapFS{
		"sub/a.go":     {Data: []byte("a")},
		"sub/x/b.go":   {Data: []byte("b")},
		"sub/link":     {Data: []byte("a.go"), Mode: fs.ModeSymlink},
		"other/c.go":   {Data: []byte("c")},
		"top-level.go": {Data: []byte("t")},
	}

	var b memfs.Builder

	require.NoError(t, b.AddTree("src/plg.local/plugin", src, "sub"))
	require.NoError(t, b.AddTree("all", src, "."))

	fsys, err := b.Build()
	require.NoError(t, err)

	for name, want := range map[string]string{
		"src/plg.local/plugin/a.go":   "a",
		"src/plg.local/plugin/x/b.go": "b",
		"all/other/c.go":              "c",
		"all/top-level.go":            "t",
		"all/sub/a.go":                "a",
	} {
		got, readErr := fs.ReadFile(fsys, name)
		require.NoError(t, readErr, name)
		assert.Equal(t, want, string(got), name)
	}

	_, err = fs.Stat(fsys, "src/plg.local/plugin/link")
	require.ErrorIs(t, err, fs.ErrNotExist, "symlinks are not followed or copied")

	var missing memfs.Builder

	require.ErrorIs(t, missing.AddTree("p", src, "nope"), fs.ErrNotExist)

	var failing memfs.Builder

	bad := failingFS{fstest.MapFS{"sub/bad": {Data: []byte("x")}}}
	require.ErrorIs(t, failing.AddTree("p", bad, "sub"), errDiskOnFire)
}
