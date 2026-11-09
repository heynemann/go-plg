package typings

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/memfs"
)

// listBatch is how many entries the walk of a plugin's files reads from a directory at once, at most.
const listBatch = 256

// compilerReads tells whether the compiler can read the file name: a TypeScript or JavaScript source or
// declaration, or JSON (tsconfig.json, package.json, a JSON module). It resolves no import to another file.
func compilerReads(name string) bool {
	switch path.Ext(name) {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs", ".json":
		return true
	default:
		return false
	}
}

// regularFiles returns the plugin's files the compiler can read: plugin itself when it is an in-memory tree,
// which a storage's limits bounded and which holds no links, else a copy of its regular files compilerReads
// accepts, symbolic links unfollowed. The copy holds the plugin to plg's limits as loading it does, failing with
// plg.ErrPluginTooLarge before reading any file: every entry, read or not, counts against plg.MaxPluginEntries and
// plg.MaxPluginPathBytes, and every regular file's stated size against plg.MaxPluginFileSize and
// plg.MaxPluginSize; a file growing while it is read is read up to what is left of them. It fails when ctx ends.
func regularFiles(ctx context.Context, plugin fs.FS) (*memfs.FS, error) {
	if files, ok := plugin.(*memfs.FS); ok {
		return files, nil
	}

	walk := pluginWalk{files: plugin}

	err := walk.dir(ctx, ".")
	if err == nil {
		err = walk.copy(ctx)
	}

	var files *memfs.FS

	if err == nil {
		files, err = walk.tree.Build()
	}

	if err != nil {
		return nil, fmt.Errorf("typings: copying the plugin's files: %w", err)
	}

	return files, nil
}

// pluginWalk is the copy of a plugin's files by regularFiles: the sources it reads, with their stated sizes, the
// sum of every regular file's stated size, how many entries it has read and the bytes of their paths.
type pluginWalk struct {
	files   fs.FS
	tree    memfs.Builder
	sources []statedFile
	stated  int
	entries int
	paths   int
}

// statedFile is a regular file of a plugin's files and the size its directory entry states.
type statedFile struct {
	name string
	size int
}

// dir walks the directory name and the directories below it, failing when ctx has ended by the time it reads a
// directory.
func (w *pluginWalk) dir(ctx context.Context, name string) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("listing %s: %w", name, err)
	}

	entries, err := w.readDir(name)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		child := path.Join(name, entry.Name())

		switch {
		case entry.IsDir():
			err = w.dir(ctx, child)
		case entry.Type().IsRegular():
			err = w.file(child, entry)
		default: // links are not followed, other irregular files not read
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// file counts the regular file name, whose directory entry is entry, against the size limits, and keeps it when
// the compiler reads it.
func (w *pluginWalk) file(name string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err //nolint:wrapcheck // the file tree's error, naming the file like its others
	}

	size := max(info.Size(), 0)
	if size > plg.MaxPluginFileSize {
		return fileTooLarge(name)
	}

	w.stated += int(size)
	if w.stated > plg.MaxPluginSize {
		return pluginTooLarge(name)
	}

	if compilerReads(name) {
		w.sources = append(w.sources, statedFile{name: name, size: int(size)})
	}

	return nil
}

// readDir returns the entries of the directory name, sorted by name, counted against the limits as they are read:
// an [fs.ReadDirFile] in batches of at most listBatch, and one more than the entries left, so that reading stops at
// the batch that brings the plugin over a limit. A directory that is no [fs.ReadDirFile] is read whole.
func (w *pluginWalk) readDir(name string) ([]fs.DirEntry, error) {
	dir, err := w.files.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // the file tree's error, naming the directory like its others
	}

	entries, err := w.read(name, dir)

	err = errors.Join(err, dir.Close())
	if err != nil {
		return nil, err
	}

	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })

	return entries, nil
}

// read reads the entries of dir, the directory name, for readDir.
func (w *pluginWalk) read(name string, dir fs.File) ([]fs.DirEntry, error) {
	batches, ok := dir.(fs.ReadDirFile)
	if !ok {
		entries, err := fs.ReadDir(w.files, name)
		if err != nil {
			return nil, err //nolint:wrapcheck // the file tree's error, naming the directory like its others
		}

		return entries, w.count(name, entries)
	}

	var entries []fs.DirEntry

	for {
		batch, err := batches.ReadDir(min(listBatch, plg.MaxPluginEntries-w.entries+1))

		countErr := w.count(name, batch)
		if countErr != nil {
			return nil, countErr
		}

		entries = append(entries, batch...)

		switch {
		case errors.Is(err, io.EOF), err == nil && len(batch) == 0:
			return entries, nil
		case err != nil:
			return nil, err //nolint:wrapcheck // the file tree's error, naming the directory like its others
		}
	}
}

// count counts entries, read from the directory dir, against plg.MaxPluginEntries and their paths against
// plg.MaxPluginPathBytes, failing with the first entry over either, which it names.
func (w *pluginWalk) count(dir string, entries []fs.DirEntry) error {
	prefix := len(dir) + len("/")
	if dir == "." {
		prefix = 0
	}

	for _, entry := range entries {
		w.entries++
		w.paths += prefix + len(entry.Name())

		switch {
		case w.entries > plg.MaxPluginEntries:
			return fmt.Errorf("%w: %s brings the plugin's files over %d entries",
				plg.ErrPluginTooLarge, path.Join(dir, entry.Name()), plg.MaxPluginEntries)
		case w.paths > plg.MaxPluginPathBytes:
			return fmt.Errorf("%w: %s brings the paths of the plugin's files over %d bytes",
				plg.ErrPluginTooLarge, path.Join(dir, entry.Name()), plg.MaxPluginPathBytes)
		}
	}

	return nil
}

// copy reads the sources the walk kept into the tree. A source may hold more than it stated, up to what the
// limits leave: plg.MaxPluginFileSize, and plg.MaxPluginSize less every regular file's stated size and what the
// sources read so far held beyond theirs.
func (w *pluginWalk) copy(ctx context.Context) error {
	left := plg.MaxPluginSize - w.stated

	for _, source := range w.sources {
		data, err := w.readFile(ctx, source, min(plg.MaxPluginFileSize, source.size+left))
		if err == nil {
			left -= len(data) - source.size
			err = w.tree.Add(source.name, data)
		}

		if err != nil {
			return err //nolint:wrapcheck // readFile's errors name the file, as memfs's do; regularFiles wraps it
		}
	}

	return nil
}

// readFile reads the source file, of at most limit bytes, failing when ctx ends.
func (w *pluginWalk) readFile(ctx context.Context, file statedFile, limit int) ([]byte, error) {
	opened, err := w.files.Open(file.name)
	if err != nil {
		return nil, err //nolint:wrapcheck // the file tree's error, naming the file like its others
	}

	data, err := readUpTo(ctx, opened, file.size, limit+1)

	err = errors.Join(err, opened.Close())

	switch {
	case err != nil:
		return nil, fmt.Errorf("reading %s: %w", file.name, err)
	case len(data) > limit && limit < plg.MaxPluginFileSize:
		return nil, pluginTooLarge(file.name)
	case len(data) > limit:
		return nil, fileTooLarge(file.name)
	default:
		return data, nil
	}
}

// readUpTo reads src to its end, or to most bytes, into a buffer first sized for size bytes (so a file that keeps
// its stated size is read without growing it), failing when ctx ends.
func readUpTo(ctx context.Context, src io.Reader, size, most int) ([]byte, error) {
	data := make([]byte, 0, min(size+1, most))

	for len(data) < most {
		err := ctx.Err()
		if err != nil {
			return nil, err //nolint:wrapcheck // readFile wraps it, naming the file
		}

		if len(data) == cap(data) {
			data = slices.Grow(data, 1)
		}

		n, err := src.Read(data[len(data):min(cap(data), most)])
		data = data[:len(data)+n]

		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err //nolint:wrapcheck // readFile wraps it, naming the file
		}
	}

	return data, nil
}

// fileTooLarge reports the plugin's file name, over plg.MaxPluginFileSize.
func fileTooLarge(name string) error {
	return fmt.Errorf("%w: %s is over %d bytes", plg.ErrPluginTooLarge, name, plg.MaxPluginFileSize)
}

// pluginTooLarge reports the plugin's file name, bringing the plugin's files over plg.MaxPluginSize.
func pluginTooLarge(name string) error {
	return fmt.Errorf("%w: %s brings the plugin's files over %d bytes", plg.ErrPluginTooLarge, name,
		plg.MaxPluginSize)
}
