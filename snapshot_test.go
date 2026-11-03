package plg_test

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

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
				_, err = store.Open(t.Context(), plg.Descriptor{Key: "p"})
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

		files, err := plg.NewFSStorage(sparseRoot(t, sizes)).Open(t.Context(), plg.Descriptor{Key: "p"})
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

			err := plg.NewMemoryStorage("mem").Put(plg.Descriptor{Key: "p"}, files)
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

	_, err := store.Open(ended, plg.Descriptor{Key: "p"})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, files.reads, "an ended context reads nothing")

	_, err = store.Open(ctx, plg.Descriptor{Key: "p"})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "plugin directory p")
	assert.Equal(t, 3, files.reads, "no read after the cancellation")
}
