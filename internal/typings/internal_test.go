package typings

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/layout"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/tscheck"
)

// TestCreatePosition finds the export of Create in the forms TypeScript and JavaScript write it, in UTF-16 columns
// as the compiler counts them, and falls back to the file's start.
func TestCreatePosition(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		source       string
		line, column int
	}{
		{"export function Create() {}", 1, 17},
		{"// é\n\nexport async function Create() {}", 3, 23},
		{"export function* Create() {}", 1, 18},
		{"const x = '😀'; export const Create = () => 1;", 1, 30},
		{"export let Create = 1", 1, 12},
		{"function Create() {}\nexport { other, Create };", 2, 17},
		{"function Create() {}", 1, 1},
		{"export function CreateMore() {}", 1, 1},
		{"", 1, 1},
	} {
		diag := tscheck.Diagnostic{Line: 9, Column: 9}
		placeAtCreate(&diag, []byte(tc.source))
		assert.Equal(t, []int{tc.line, tc.column}, []int{diag.Line, diag.Column}, tc.source)
	}
}

// TestRelative maps the compiler's file names, relative to the plugin's directory, to the plugin's files, and
// tells the type-check's own files apart.
func TestRelative(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, want string
		plugin     bool
	}{
		{"plugin.ts", "plugin.ts", true},
		{"lib/a.ts", "lib/a.ts", true},
		{"./lib/../b.ts", "b.ts", true},
		{"../plg/check.ts", "/plg/check.ts", false},
		{"", "", true},
	} {
		got, plugin := relative(tc.name)
		assert.Equal(t, tc.want, got, tc.name)
		assert.Equal(t, tc.plugin, plugin, tc.name)
	}
}

// TestShapeOfClassifiesEveryKind gives every kind layout names a shape, so a new wire kind fails here instead of
// being written as some other shape, and panics on a kind layout does not name.
func TestShapeOfClassifiesEveryKind(t *testing.T) {
	t.Parallel()

	kind := layout.Kind(1)
	for ; !strings.HasPrefix(kind.String(), "kind("); kind++ {
		assert.NotPanics(t, func() { shapeOf(kind) }, "kind %s", kind)
	}

	assert.Equal(t, shapeInteger, shapeOf(layout.Uint16))
	assert.Equal(t, shapeFloat, shapeOf(layout.Float32))
	assert.Equal(t, shapeStruct, shapeOf(layout.Struct))
	assert.Panics(t, func() { shapeOf(0) })
	assert.Panics(t, func() { shapeOf(kind) })
}

// TestScalarTablesSpellEveryScalarShape: the TypeScript and Lua tables hold exactly the scalar shapes (those before
// shapeSlice), so a new one cannot be written as an empty type.
func TestScalarTablesSpellEveryScalarShape(t *testing.T) {
	t.Parallel()

	for s := shapeBool; s < shapeSlice; s++ {
		assert.Contains(t, tsScalars, s)
		assert.Contains(t, luaScalars, s)
	}

	assert.Len(t, tsScalars, int(shapeSlice-shapeBool))
	assert.Len(t, luaScalars, int(shapeSlice-shapeBool))
}

// sparseDir returns a directory holding sparse files of the given sizes, by slash-separated name.
func sparseDir(t *testing.T, sizes map[string]int64) string {
	t.Helper()

	dir := t.TempDir()

	for name, size := range sizes {
		full := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, nil, 0o600))
		require.NoError(t, os.Truncate(full, size))
	}

	return dir
}

// allocatedBy returns the bytes run allocates, for a test no other runs beside.
func allocatedBy(run func()) uint64 {
	var before, after runtime.MemStats

	runtime.GC() //nolint:revive // call-to-gc: only what run allocates is measured
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

// names returns the names of the files of files.
func names(t *testing.T, files *memfs.FS) []string {
	t.Helper()

	var got []string

	for name := range files.Files() {
		got = append(got, name)
	}

	return got
}

// TestPluginFilesOverTheLimitsAreNotCopied copies plugin directories on disk, of sparse files, for the type-check:
// a file over plg.MaxPluginFileSize, or files over plg.MaxPluginSize together, fail with plg.ErrPluginTooLarge
// naming the file, without reading any; a large file the compiler does not read is not copied, nor read.
//
//nolint:paralleltest // runtime.MemStats.TotalAlloc over one copy measures only it while no other test runs
func TestPluginFilesOverTheLimitsAreNotCopied(t *testing.T) {
	const most = plg.MaxPluginFileSize

	for _, tc := range []struct {
		sizes map[string]int64
		name  string
		file  string
	}{
		{name: "a file over the file limit", sizes: map[string]int64{"plugin.ts": 10, "big.bin": most + 1}, file: "big.bin"},
		{name: "a source over the file limit", sizes: map[string]int64{"plugin.ts": most + 1}, file: "plugin.ts"},
		{
			name:  "files over the plugin limit",
			sizes: map[string]int64{"d/1.bin": most, "d/2.bin": most, "d/3.bin": most, "d/4.bin": most, "d/5.ts": 1},
			file:  "d/5.ts",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := os.DirFS(sparseDir(t, tc.sizes))

			var err error

			allocated := allocatedBy(func() { _, err = regularFiles(t.Context(), dir) })
			t.Logf("allocated %d bytes: %v", allocated, err)
			require.ErrorIs(t, err, plg.ErrPluginTooLarge)
			require.ErrorContains(t, err, tc.file)
			assert.Less(t, allocated, uint64(1<<20), "nothing is read")
		})
	}

	t.Run("a large file the compiler does not read", func(t *testing.T) {
		dir := os.DirFS(sparseDir(t, map[string]int64{
			"plugin.ts": 10, "data.bin": most, "tsconfig.json": 2, "types/plg.d.ts": 3, "README.md": 4,
		}))

		var (
			files *memfs.FS
			err   error
		)

		allocated := allocatedBy(func() { files, err = regularFiles(t.Context(), dir) })
		t.Logf("allocated %d bytes", allocated)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"plugin.ts", "tsconfig.json", "types/plg.d.ts"}, names(t, files))
		assert.Less(t, allocated, uint64(1<<20), "data.bin is not read")
	})
}

// TestPluginListingsOverTheLimitsAreNotCopied copies plugins of more than plg.MaxPluginEntries entries, or entry
// paths over plg.MaxPluginPathBytes, whether the compiler reads them or not: each fails with
// plg.ErrPluginTooLarge naming the entry that brings it over. A plugin at the entry limit is copied.
func TestPluginListingsOverTheLimitsAreNotCopied(t *testing.T) {
	t.Parallel()

	files := func(n int, name func(int) string) fstest.MapFS {
		tree := fstest.MapFS{}
		for i := range n {
			tree[name(i)] = &fstest.MapFile{}
		}

		return tree
	}

	numbered := func(i int) string { return fmt.Sprintf("f%05d", i) }
	long := func(i int) string { return fmt.Sprintf("%04d", i) + strings.Repeat("x", 4092) }

	_, err := regularFiles(t.Context(), files(plg.MaxPluginEntries+1, numbered))
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
	require.ErrorContains(t, err, fmt.Sprintf("f%05d brings the plugin's files over %d entries",
		plg.MaxPluginEntries, plg.MaxPluginEntries))

	dirs := files(plg.MaxPluginEntries/2+1, func(i int) string { return numbered(i) + "/a.ts" })
	_, err = regularFiles(t.Context(), dirs)
	require.ErrorIs(t, err, plg.ErrPluginTooLarge, "directories count")

	_, err = regularFiles(t.Context(), files(plg.MaxPluginPathBytes/4096+1, long))
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
	require.ErrorContains(t, err, fmt.Sprintf("brings the paths of the plugin's files over %d bytes",
		plg.MaxPluginPathBytes))

	_, err = regularFiles(t.Context(), files(plg.MaxPluginEntries, numbered))
	require.NoError(t, err, "at the limit")
}

// TestCopyReadsOnlySourcesAndNoLinks copies the files the compiler reads, TypeScript, JavaScript and JSON, of a
// plugin with a link to one of them: the link is not followed, other files are left out. An in-memory tree is the
// copy already.
func TestCopyReadsOnlySourcesAndNoLinks(t *testing.T) {
	t.Parallel()

	sources := []string{"a.tsx", "a.mts", "a.cts", "a.js", "a.jsx", "a.mjs", "a.cjs", "a.json", "a.d.ts"}
	plugin := fstest.MapFS{
		"plugin.ts": {Data: []byte("x")},
		"leak.ts":   {Data: []byte("plugin.ts"), Mode: fs.ModeSymlink},
		"lib/a.txt": {Data: []byte("text")},
	}
	want := make([]string, 0, len(sources)+1)
	want = append(want, "plugin.ts")

	for _, name := range sources {
		plugin["lib/"+name] = &fstest.MapFile{Data: []byte(name)}
		want = append(want, "lib/"+name)
	}

	files, err := regularFiles(t.Context(), plugin)
	require.NoError(t, err)
	assert.ElementsMatch(t, want, names(t, files))

	data, err := files.ReadFile("lib/a.d.ts")
	require.NoError(t, err)
	assert.Equal(t, "a.d.ts", string(data))

	same, err := regularFiles(t.Context(), files)
	require.NoError(t, err)
	assert.Same(t, files, same)
}

// growingFS is a file tree whose file name reads endless zeros whatever size it states, counting what it hands
// out, and ends cancel's context on the first read when cancel is set.
type growingFS struct {
	fs.FS

	cancel context.CancelFunc
	name   string
	read   int
}

func (g *growingFS) Open(name string) (fs.File, error) {
	file, err := g.FS.Open(name)
	if err != nil || name != g.name {
		return file, err //nolint:wrapcheck // the tree's own error
	}

	return &growingFile{File: file, fsys: g}, nil
}

type growingFile struct {
	fs.File

	fsys *growingFS
}

func (f *growingFile) Read(buf []byte) (int, error) {
	if f.fsys.cancel != nil {
		f.fsys.cancel()
	}

	clear(buf)

	f.fsys.read += len(buf)

	return len(buf), nil
}

// A source growing while it is read is read up to what the limits leave it, which counts the stated sizes of every
// file, read or not; a copy stops once its context ends.
func TestGrowingSourcesStopAtTheLimits(t *testing.T) {
	t.Parallel()

	const most = plg.MaxPluginFileSize

	for _, tc := range []struct {
		sizes   map[string]int64
		name    string
		err     string
		maxRead int
	}{
		{name: "past the file limit", sizes: map[string]int64{"a.ts": 0}, err: "a.ts is over", maxRead: most + 1},
		{
			name:    "past the plugin limit",
			sizes:   map[string]int64{"a.ts": 0, "b.bin": most, "c.bin": most, "d.bin": most, "e.bin": most - 4},
			err:     "a.ts brings the plugin's files over",
			maxRead: 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			files := &growingFS{FS: os.DirFS(sparseDir(t, tc.sizes)), name: "a.ts"}

			_, err := regularFiles(t.Context(), files)
			require.ErrorIs(t, err, plg.ErrPluginTooLarge)
			require.ErrorContains(t, err, tc.err)
			assert.LessOrEqual(t, files.read, tc.maxRead)
		})
	}

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		files := &growingFS{FS: os.DirFS(sparseDir(t, map[string]int64{"a.ts": 0})), name: "a.ts", cancel: cancel}

		_, err := regularFiles(ctx, files)
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, "reading a.ts")

		_, err = regularFiles(ctx, files)
		require.ErrorIs(t, err, context.Canceled, "an ended context lists nothing")
		require.ErrorContains(t, err, "listing .")
	})
}

// failingFS fails to open the name fail, and hands out directories that read only whole (no fs.ReadDirFile).
type failingFS struct {
	fstest.MapFS

	fail string
}

var errOpen = errors.New("open failed")

func (f failingFS) Open(name string) (fs.File, error) {
	if name == f.fail {
		return nil, fmt.Errorf("%s: %w", name, errOpen)
	}

	file, err := f.MapFS.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // the tree's own error
	}

	return struct{ fs.File }{file}, nil
}

// A tree whose directories read only whole is copied within the limits too; a file or directory that cannot be
// opened fails the copy, naming it.
func TestCopyThroughWholeDirectoryReads(t *testing.T) {
	t.Parallel()

	tree := fstest.MapFS{"plugin.ts": {Data: []byte("x")}, "lib/a.ts": {Data: []byte("a")}}

	files, err := regularFiles(t.Context(), failingFS{MapFS: tree})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"plugin.ts", "lib/a.ts"}, names(t, files))

	for _, name := range []string{"lib", "lib/a.ts"} {
		_, err = regularFiles(t.Context(), failingFS{MapFS: tree, fail: name})
		require.ErrorIs(t, err, errOpen, name)
		require.ErrorContains(t, err, name)
	}

	many := fstest.MapFS{}
	for i := range plg.MaxPluginEntries + 1 {
		many[fmt.Sprintf("f%05d", i)] = &fstest.MapFile{}
	}

	_, err = regularFiles(t.Context(), failingFS{MapFS: many})
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
}
