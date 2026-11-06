package plg_test

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

// sparseRoot returns a directory of plugins holding the plugin directory p, with files (slash-separated names) of
// the given sizes. They are sparse: they cost no disk and read as zeros.
func sparseRoot(t *testing.T, sizes map[string]int64) string {
	t.Helper()

	root := t.TempDir()

	for name, size := range sizes {
		file := filepath.Join(root, "p", filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o750))
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		require.NoError(t, os.Truncate(file, size))
	}

	return root
}

// growingFS is a file tree whose file named grows, whatever size its directory entry states, as it is read: it
// reads as zeros until grownTo bytes. It counts the reads, and calls stop at the read numbered stopAt.
type growingFS struct {
	fs.FS

	stop    func()
	name    string
	grownTo int64
	stopAt  int
	reads   int
	read    int64
}

func (g *growingFS) Open(name string) (fs.File, error) {
	file, err := g.FS.Open(name)
	if err != nil || name != g.name {
		return file, err //nolint:wrapcheck // the tree's own error, as an fs.FS returns it
	}

	return &growingFile{File: file, fsys: g}, nil
}

type growingFile struct {
	fs.File

	fsys *growingFS
}

func (f *growingFile) Read(buf []byte) (int, error) {
	owner := f.fsys

	owner.reads++
	if owner.reads == owner.stopAt {
		owner.stop()
	}

	n := min(int64(len(buf)), owner.grownTo-owner.read)
	if n == 0 {
		return 0, io.EOF
	}

	clear(buf[:n])

	owner.read += n

	return int(n), nil
}

// TestPluginFilesOverTheLimitsAreNeverRead opens plugin directories with a file over MaxPluginFileSize, or files
// together over MaxPluginSize: each fails at once, naming the plugin and the file, without reading them. Files
// at the limits are copied.
//
//nolint:paralleltest // runtime.MemStats.TotalAlloc over one Open measures only it while no other test runs
func TestPluginFilesOverTheLimitsAreNeverRead(t *testing.T) {
	const most = plg.MaxPluginFileSize

	tests := []struct {
		sizes map[string]int64
		name  string
		file  string
	}{
		{
			name:  "a file over the file limit",
			sizes: map[string]int64{"plugin.go": 10, "big.bin": most + 1},
			file:  "big.bin",
		},
		{
			name:  "files over the plugin limit",
			sizes: map[string]int64{"d/1.bin": most, "d/2.bin": most, "d/3.bin": most, "d/4.bin": most, "d/5.bin": 1},
			file:  "d/5.bin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := plg.NewFSStorage(sparseRoot(t, tc.sizes))

			var err error

			allocated := allocatedBy(func() {
				_, err = store.Open(t.Context(), &plg.Descriptor{Key: "p"})
			})
			t.Logf("allocated %d bytes: %v", allocated, err)
			require.ErrorIs(t, err, plg.ErrPluginTooLarge)
			require.ErrorContains(t, err, "plugin directory p")
			require.ErrorContains(t, err, tc.file)
			assert.Less(t, allocated, uint64(1<<20), "nothing is read")
		})
	}

	t.Run("files at the limits", func(t *testing.T) {
		sizes := map[string]int64{"d/1.bin": most, "d/2.bin": most, "d/3.bin": most, "d/4.bin": most}

		files, err := plg.NewFSStorage(sparseRoot(t, sizes)).Open(t.Context(), &plg.Descriptor{Key: "p"})
		require.NoError(t, err)

		for name, size := range sizes {
			info, err := fs.Stat(files, name)
			require.NoError(t, err)
			assert.Equal(t, size, info.Size(), name)
		}
	})
}

// A file growing while it is read is read up to the limit left for it, which accounts for the stated sizes of
// the files not read yet.
func TestGrowingPluginFilesStopAtTheLimits(t *testing.T) {
	t.Parallel()

	const most = plg.MaxPluginFileSize

	tests := []struct {
		sizes   map[string]int64
		name    string
		maxRead int64
	}{
		{name: "past the file limit", sizes: map[string]int64{"a.txt": 0}, maxRead: most + 1},
		{
			name:    "past the plugin limit",
			sizes:   map[string]int64{"a.txt": 0, "b.bin": most, "c.bin": most, "d.bin": most, "e.bin": most},
			maxRead: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			files := &growingFS{
				FS:      os.DirFS(filepath.Join(sparseRoot(t, tc.sizes), "p")),
				name:    "a.txt",
				grownTo: 2 * most,
			}

			err := plg.NewMemoryStorage("mem").Put(&plg.Descriptor{Key: "p"}, files)
			require.ErrorIs(t, err, plg.ErrPluginTooLarge)
			require.ErrorContains(t, err, "plugin p")
			require.ErrorContains(t, err, "a.txt")
			assert.LessOrEqual(t, files.read, tc.maxRead)
		})
	}
}

// Ending Open's context stops copying a plugin's files at the next read; an ended one reads nothing.
func TestCancellingStopsTheCopyOfPluginFiles(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	root := sparseRoot(t, map[string]int64{"a.txt": 0, "b.txt": 1})
	files := &growingFS{FS: os.DirFS(root), name: "p/a.txt", grownTo: plg.MaxPluginFileSize, stop: cancel, stopAt: 3}
	store := plg.NewFSStorageFromFS("growing", files)

	ended, end := context.WithCancel(t.Context())
	end()

	_, err := store.Open(ended, &plg.Descriptor{Key: "p"})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, files.reads, "an ended context reads nothing")

	_, err = store.Open(ctx, &plg.Descriptor{Key: "p"})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "plugin directory p")
	assert.Equal(t, 3, files.reads, "no read after the cancellation")
}

// listingRoot returns a directory of plugins holding the plugin directory p, with create called with each of
// names in it.
func listingRoot(t *testing.T, names []string, create func(t *testing.T, name string)) string {
	t.Helper()

	plugin := filepath.Join(t.TempDir(), "p")
	require.NoError(t, os.Mkdir(plugin, 0o750))

	for _, name := range names {
		create(t, filepath.Join(plugin, name))
	}

	return filepath.Dir(plugin)
}

func emptyFile(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(name, nil, 0o600))
}

func emptyDir(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, os.Mkdir(name, 0o750))
}

// numbered returns n names, format with 0 to n-1.
func numbered(n int, format string) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf(format, i)
	}

	return names
}

// TestPluginListingsOverTheLimitsStopEarly opens plugin directories of twice as many empty files or directories as
// MaxPluginEntries, or of names twice as long together as MaxPluginPathBytes: each fails with ErrPluginTooLarge,
// naming the plugin and an entry, and allocates less than copying a plugin exactly at the limit, so the listing
// stopped at the limit. Plugins at the limits are copied.
//
//nolint:paralleltest // runtime.MemStats.TotalAlloc over one Open measures only it while no other test runs
func TestPluginListingsOverTheLimitsStopEarly(t *testing.T) {
	const (
		entries = plg.MaxPluginEntries
		longest = 255 // the longest name Linux allows: a 5-digit number and 250 x
		long    = plg.MaxPluginPathBytes / longest
	)

	longNames := numbered(2*long, "%05d"+strings.Repeat("x", longest-5))
	// long names of 255 bytes and one shorter make exactly MaxPluginPathBytes.
	exact := append(slices.Clone(longNames[:long]), strings.Repeat("z", plg.MaxPluginPathBytes-long*longest))

	tests := []struct {
		create  func(t *testing.T, name string)
		name    string
		named   string
		atLimit []string
		over    []string
		copied  int // the entries of the copy's top directory: its files (empty directories are left out)
	}{
		{
			name:    "empty files",
			create:  emptyFile,
			atLimit: numbered(entries, "f%05d"),
			over:    numbered(2*entries, "f%05d"),
			named:   `\bp: .*\bf\d{5}\b`,
			copied:  entries,
		},
		{
			name:    "empty directories",
			create:  emptyDir,
			atLimit: numbered(entries, "d%05d"),
			over:    numbered(2*entries, "d%05d"),
			named:   `\bp: .*\bd\d{5}\b`,
		},
		{
			name:    "long paths",
			create:  emptyFile,
			atLimit: exact,
			over:    longNames,
			named:   `\bp: .*\b\d{5}x{250}\b`,
			copied:  len(exact),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			atLimit := plg.NewFSStorage(listingRoot(t, tc.atLimit, tc.create))
			over := plg.NewFSStorage(listingRoot(t, tc.over, tc.create))

			var (
				files fs.FS
				err   error
			)

			copied := allocatedBy(func() {
				files, err = atLimit.Open(t.Context(), &plg.Descriptor{Key: "p"})
			})

			require.NoError(t, err, "at the limits")

			top, err := fs.ReadDir(files, ".")
			require.NoError(t, err)
			assert.Len(t, top, tc.copied)

			stopped := allocatedBy(func() {
				_, err = over.Open(t.Context(), &plg.Descriptor{Key: "p"})
			})
			t.Logf(
				"copying the plugin at the limits allocated %d bytes, failing over them %d: %v",
				copied,
				stopped,
				err,
			)
			require.ErrorIs(t, err, plg.ErrPluginTooLarge)
			require.ErrorContains(t, err, "plugin directory p")
			assert.Regexp(t, tc.named, err.Error())
			assert.Less(t, stopped, copied, "the listing stopped at the limit")
		})
	}
}

// generatedFS is a plugin whose top directory holds count entries, made up as its directory is read: empty
// directories (dirs) or empty files, named by name. It counts the entries it handed out.
type generatedFS struct {
	name   func(i int) string
	count  int
	served int
	dirs   bool
}

func (g *generatedFS) Open(name string) (fs.File, error) {
	switch {
	case name == ".":
		return &generatedDir{fsys: g}, nil
	case g.dirs:
		return &generatedDir{fsys: &generatedFS{}}, nil
	default:
		return fstest.MapFS{name: {}}.Open(name) //nolint:wrapcheck // the tree's own error, as an fs.FS returns it
	}
}

// wholeFS is a generatedFS whose directories are no fs.ReadDirFile: it is an fs.ReadDirFS instead, which reads a
// directory whole.
type wholeFS struct {
	*generatedFS
}

func (w wholeFS) Open(name string) (fs.File, error) {
	file, err := w.generatedFS.Open(name)
	if err != nil {
		return nil, err
	}

	return struct{ fs.File }{file}, nil
}

func (w wholeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	fsys := w.generatedFS
	if name != "." {
		fsys = &generatedFS{}
	}

	return (&generatedDir{fsys: fsys}).ReadDir(-1)
}

// generatedDir is an open directory of a generatedFS.
type generatedDir struct {
	fsys *generatedFS
	next int
}

func (*generatedDir) Stat() (fs.FileInfo, error) { return generatedInfo{name: ".", dir: true}, nil }

func (*generatedDir) Read([]byte) (int, error) { return 0, errStorage }

func (*generatedDir) Close() error { return nil }

func (d *generatedDir) ReadDir(n int) ([]fs.DirEntry, error) {
	fsys := d.fsys

	left := fsys.count - d.next
	if n > 0 && left == 0 {
		return nil, io.EOF
	}

	if n <= 0 || n > left {
		n = left
	}

	entries := make([]fs.DirEntry, n)
	for i := range entries {
		entries[i] = fs.FileInfoToDirEntry(generatedInfo{name: fsys.name(d.next + i), dir: fsys.dirs})
	}

	d.next += n
	fsys.served += n

	return entries, nil
}

// generatedInfo is the information of an empty file or directory.
type generatedInfo struct {
	name string
	dir  bool
}

func (i generatedInfo) Name() string { return i.name }

func (generatedInfo) Size() int64 { return 0 }

func (i generatedInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}

	return 0o444
}

func (generatedInfo) ModTime() time.Time { return time.Time{} }

func (i generatedInfo) IsDir() bool { return i.dir }

func (generatedInfo) Sys() any { return nil }

// A plugin of an fs.FS listing a million made-up entries (empty files, empty directories), or 8 times the names
// of 4 KiB that MaxPluginPathBytes allows, fails once its listing goes over a limit: at the entry over it for the
// entry count, within a batch of it for the names. An fs.FS whose directories can only be read whole is read
// whole, then fails.
func TestPluginListingsStopAtTheLimits(t *testing.T) {
	t.Parallel()

	const (
		count    = 1 << 20
		nameSize = 4 << 10
		named    = plg.MaxPluginPathBytes/nameSize + 1 // the entries that bring the names over the limit
	)

	short := func(i int) string { return fmt.Sprintf("e%07d", i) }
	long := func(i int) string { return fmt.Sprintf("%07d", i) + strings.Repeat("x", nameSize-7) }

	tests := []struct {
		files *generatedFS
		name  string
		read  int
		whole bool
	}{
		{
			name:  "empty files",
			files: &generatedFS{name: short, count: count},
			read:  plg.MaxPluginEntries + 1,
		},
		{
			name:  "empty directories",
			files: &generatedFS{name: short, count: count, dirs: true},
			read:  plg.MaxPluginEntries + 1,
		},
		{name: "long names", files: &generatedFS{name: long, count: 8 * named}, read: 2 * named},
		{name: "read whole", files: &generatedFS{name: short, count: 2 * plg.MaxPluginEntries}, whole: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var files fs.FS = tc.files

			if tc.whole {
				files = wholeFS{tc.files}
			}

			err := plg.NewMemoryStorage("mem").Put(&plg.Descriptor{Key: "p"}, files)
			require.ErrorIs(t, err, plg.ErrPluginTooLarge)
			require.ErrorContains(t, err, "plugin p")

			if tc.whole {
				assert.Equal(t, tc.files.count, tc.files.served, "read whole")
			} else {
				assert.LessOrEqual(t, tc.files.served, tc.read, "entries read")
			}
		})
	}
}
