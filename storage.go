package plg

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/heynemann/go-plg/internal/memfs"
)

// Language is a plugin's source language.
type Language string

// LanguageGo is a Go plugin: a package declaring func Create() (<interface>, error). Running Go plugins
// needs the Go guest: import _ "github.com/heynemann/go-plg/guest/golang".
const LanguageGo Language = "go"

// LanguageLua is a Lua 5.4 plugin: its entrypoint is a .lua file defining a global function Create() that
// returns a table whose functions implement the interface, called as methods (inst:Method(args...)). A method
// returns its single result, or an array table of several; an error it raises (or a nil, message return) is the
// call's error. The plugin's .lua files are mounted read-only at /plugin, where require resolves them; plugins
// get the base (without file loading), string, table, math, utf8 and coroutine libraries, a json module, host
// (the [WithHost] interface's methods) and http.request (through [WithHTTP]'s policy). Running Lua plugins needs
// the guest: import _ "github.com/heynemann/go-plg/guest/lua".
const LanguageLua Language = "lua"

// LanguageTypeScript is a TypeScript (or JavaScript) plugin: its entrypoint file exports
// function Create() returning an object whose methods implement the interface (methods may be async; Create too).
// esbuild bundles the entrypoint and the files it imports, by relative paths inside the plugin only, in a
// WebAssembly sandbox of its own (at most 512 MiB of memory, and 10 s plus 5 s per MiB of the plugin's files,
// within the load's context), and QuickJS-ng runs the bundle with the ECMAScript built-ins plus console (to the
// log), host (the [WithHost] interface's methods, returning Promises) and fetch (through [WithHTTP]'s policy).
// Values cross by the interface's types: struct fields by json name, nil slices, maps and pointers as null, []byte
// as Uint8Array, time.Time as {seconds, nanos} (a Date too, on the way back), several results as an array; a
// thrown error is the call's error. Running TypeScript plugins needs the guest:
// import _ "github.com/heynemann/go-plg/guest/typescript".
const LanguageTypeScript Language = "typescript"

// Storage lists plugins and opens their sources. A storage belongs to one Register call: every plugin it lists
// implements that call's interface. Implementations must be safe for concurrent use.
//
// Register reads an opened plugin's files exactly once, into an in-memory snapshot it hashes (for an empty
// Version), bundles and loads, so the files cannot change between those steps. The snapshot holds every regular
// file, within the limits of [MaxPluginSize] (entry counts and path lengths included), and the copy stops when
// Register's context ends. Symbolic links (as fs.ReadDir reports them) are never followed: one that stays inside
// the plugin's files is skipped, as if it were not there, and one whose target is absolute, leads out of the
// plugin's files or cannot be read (the fs.FS is no fs.ReadLinkFS) fails the load, naming it. Other irregular
// files are skipped.
//
// Register copies an fs.FS through its own interface only: it walks the tree, judging links by the walk's
// directory entries, then opens each regular file the walk listed by its name. It cannot tell a file the tree
// replaced by a link in between, so the rules above hold for a tree nobody changes during the copy (an embed.FS,
// an fstest.MapFS, the copies [FSStorage] and [MemoryStorage] return); an fs.FS that follows links when it opens
// a file can be led out of the plugin's files by someone writing to the tree while it is copied ([os.DirFS]
// follows them anywhere on the host), and its open of a file replaced by a FIFO may wait for a writer. A storage
// over a directory others can write to should return the copy an [FSStorage] from [NewFSStorage] opens: that one
// follows no link and never waits, whatever changes while it copies.
type Storage interface {
	// Name identifies the storage in errors and logs.
	Name() string
	// List returns the plugins the storage holds.
	List(ctx context.Context) ([]Descriptor, error)
	// Open returns the plugin's source tree; the Descriptor's Entrypoint is a path inside it. Register copies
	// the tree once, as described above, before using it.
	Open(ctx context.Context, d Descriptor) (fs.FS, error)
}

// The limits of a plugin's files, which Register copies into memory (see [Storage]): a regular file of more than
// MaxPluginFileSize bytes, or regular files of more than MaxPluginSize bytes together, fail the load with
// [ErrPluginTooLarge] before they are read, and a file that grows while it is read is read up to the limit only.
// The listing is bounded too, whatever the files hold: more than MaxPluginEntries directory entries (files,
// directories, links and others, empty or not), or entries whose paths in the plugin's files add up to more than
// MaxPluginPathBytes bytes, fail the load with [ErrPluginTooLarge] as soon as a directory read brings them over,
// before the rest of the listing is read. Directories are read in bounded batches through [fs.ReadDirFile], which
// the directories of os.DirFS, embed.FS, fstest.MapFS and [NewFSStorage] are; an fs.FS whose opened directories
// are not is listed a directory at a time through its [fs.ReadDirFS], each checked once read. [FSStorage] and
// [MemoryStorage] copy plugins by the same rules.
const (
	MaxPluginFileSize  = 16 << 20
	MaxPluginSize      = 64 << 20
	MaxPluginEntries   = 1 << 14
	MaxPluginPathBytes = 2 << 20
)

// Descriptor describes one plugin of a [Storage].
type Descriptor struct {
	// Key identifies the plugin within its Register call; its catalogue key is "<name>/<Key>".
	Key string
	// Language is the plugin's source language.
	Language Language
	// Entrypoint is a path inside the plugin's fs.FS. For Go it names the package directory or a .go file in
	// it; every file of that directory (and below) is loaded.
	Entrypoint string
	// Version identifies the plugin's revision. When it is empty, Register uses a content hash of the files it
	// loads (see [FSStorage]).
	Version string
	// MemoryLimit is the plugin's per-instance memory limit in bytes, zero for the Register limit. It can only
	// lower that limit.
	MemoryLimit uint64
}

// MemoryStorage is a [Storage] holding plugins in memory, for tests and for plugins a program builds itself.
// It is safe for concurrent use.
type MemoryStorage struct {
	plugins *xsync.Map[string, memoryPlugin]
	name    string
}

type memoryPlugin struct {
	files *memfs.FS
	desc  Descriptor
}

var _ Storage = (*MemoryStorage)(nil)

// NewMemoryStorage returns an empty storage with the given name.
func NewMemoryStorage(name string) *MemoryStorage {
	return &MemoryStorage{name: name, plugins: xsync.NewMap[string, memoryPlugin]()}
}

// Name returns the storage's name.
func (s *MemoryStorage) Name() string {
	return s.name
}

// Put adds the plugin desc, or replaces the plugin with the same key. It copies files as Register does (see
// [Storage]), failing on a symbolic link out of them or files over the limits (see [MaxPluginSize]), so later
// changes to files are not seen.
func (s *MemoryStorage) Put(desc Descriptor, files fs.FS) error {
	if desc.Key == "" {
		return fmt.Errorf("%w: memory storage %s: empty key", ErrInvalidDescriptor, s.name)
	}

	copied, err := snapshotFiles(context.Background(), files)
	if err != nil {
		return fmt.Errorf("plg: memory storage %s: plugin %s: %w", s.name, desc.Key, err)
	}

	s.plugins.Store(desc.Key, memoryPlugin{desc: desc, files: copied})

	return nil
}

// Delete removes the plugin with the given key, if any.
func (s *MemoryStorage) Delete(key string) {
	s.plugins.Delete(key)
}

// List returns the stored plugins, sorted by key.
func (s *MemoryStorage) List(context.Context) ([]Descriptor, error) {
	descs := make([]Descriptor, 0, s.plugins.Size())
	for _, p := range s.plugins.All() {
		descs = append(descs, p.desc)
	}

	slices.SortFunc(descs, func(a, b Descriptor) int { return cmp.Compare(a.Key, b.Key) })

	return descs, nil
}

// Open returns the files of the plugin with d's key, as they were when it was put.
func (s *MemoryStorage) Open(_ context.Context, d Descriptor) (fs.FS, error) {
	p, ok := s.plugins.Load(d.Key)
	if !ok {
		return nil, fmt.Errorf("plg: memory storage %s: plugin %s: %w", s.name, d.Key, fs.ErrNotExist)
	}

	return p.files, nil
}

// errSymlinkOut reports a symbolic link of a plugin's files whose target is absolute, leads out of the files or
// cannot be read.
var errSymlinkOut = errors.New("symbolic link out of the plugin's files")

// snapshotFiles returns an immutable copy of a plugin's files, as [Storage] describes, failing when ctx ends. A
// [memfs.FS], which the built-in storages return, is one already.
func snapshotFiles(ctx context.Context, files fs.FS) (*memfs.FS, error) {
	if frozen, ok := files.(*memfs.FS); ok {
		return frozen, nil
	}

	return copySnapshot(ctx, files, func(name string) (fs.File, error) {
		file, err := files.Open(name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}

		return file, nil
	})
}

// copySnapshot walks a plugin's files, then reads each regular file the walk listed, opened with open (whose
// errors name the file), into an immutable copy, failing when ctx ends. A file open fails with errSkipped is left
// out.
func copySnapshot(ctx context.Context, files fs.FS, open func(name string) (fs.File, error)) (*memfs.FS, error) {
	var tree memfs.Builder

	regular, stated, err := listFiles(ctx, files)
	if err == nil {
		err = copyFiles(ctx, open, &tree, regular, stated)
	}

	var copied *memfs.FS

	if err == nil {
		copied, err = tree.Build()
	}

	if err != nil {
		return nil, fmt.Errorf("copying the plugin's files: %w", err)
	}

	return copied, nil
}

// copyFiles reads the regular files of a plugin's files, whose stated sizes add up to stated, opened with open,
// into tree, leaving out those open fails with errSkipped.
func copyFiles(
	ctx context.Context, open func(name string) (fs.File, error), tree *memfs.Builder, regular []sizedFile, stated int,
) error {
	read := 0

	for _, file := range regular {
		// What is left for this file: the plugin's limit, less what was read and what the other files state.
		stated -= file.size

		data, err := readFile(ctx, open, file, min(MaxPluginFileSize, MaxPluginSize-read-stated))
		if errors.Is(err, errSkipped) {
			continue
		}

		if err == nil {
			read += len(data)
			err = tree.Add(file.name, data)
		}

		if err != nil {
			return err //nolint:wrapcheck // readFile's errors name the file, as memfs's do; copySnapshot wraps it
		}
	}

	return nil
}

// sizedFile is a regular file of a plugin's files and the size its directory entry states.
type sizedFile struct {
	name string
	size int
}

// listFiles walks a plugin's files, checking their symbolic links, and returns their regular files and the sum of
// their stated sizes. It fails before anything is read when a file states more than [MaxPluginFileSize] bytes or
// the files more than [MaxPluginSize] together, as soon as a directory read brings the entries over
// [MaxPluginEntries] or their paths over [MaxPluginPathBytes], and when ctx ends. It walks as fs.WalkDir does
// (lexical order, a directory's entries before the next directory's), but reads each directory in batches (see
// listing.readDir), since fs.WalkDir reads a directory whole before anything can look at its entries.
func listFiles(ctx context.Context, files fs.FS) ([]sizedFile, int, error) {
	walk := listing{files: files}

	err := walk.dir(ctx, ".")

	return walk.regular, walk.stated, err
}

// listBatch is how many entries the walk of a plugin's files reads from a directory at once, at most.
const listBatch = 256

// listing is the walk of a plugin's files by listFiles: their regular files so far, the sum of their stated sizes,
// and how many entries it has read and the bytes of their paths, which the limits bound.
type listing struct {
	files   fs.FS
	regular []sizedFile
	stated  int
	entries int
	paths   int
}

// dir walks the directory name and the directories below it, failing when ctx has ended by the time it reads a
// directory.
func (l *listing) dir(ctx context.Context, name string) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("listing %s: %w", name, err)
	}

	entries, err := l.readDir(name)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		child := path.Join(name, entry.Name())

		switch {
		case entry.IsDir():
			err = l.dir(ctx, child)
		case entry.Type().IsRegular():
			err = l.file(child, entry)
		case entry.Type()&fs.ModeSymlink != 0:
			err = checkLink(l.files, child)
		default: // other irregular files are left out
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// file adds the regular file name, whose directory entry is entry, failing when the size it states brings the
// plugin over a limit.
func (l *listing) file(name string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err //nolint:wrapcheck // the file tree's error, naming the file like its others
	}

	size := max(info.Size(), 0)
	if size > MaxPluginFileSize {
		return fileTooLarge(name)
	}

	l.stated += int(size)
	if l.stated > MaxPluginSize {
		return pluginTooLarge(name)
	}

	l.regular = append(l.regular, sizedFile{name: name, size: int(size)})

	return nil
}

// readDir returns the entries of the directory name, sorted by name, charged to the limits as they are read: an
// [fs.ReadDirFile] in batches of at most listBatch, and one more than the entries left, so that reading stops at
// the batch that brings the plugin over a limit. A directory that is no [fs.ReadDirFile] is read whole through
// fs.ReadDir, then charged. A directory the walk listed that is not one when opened was replaced.
func (l *listing) readDir(name string) ([]fs.DirEntry, error) {
	dir, err := l.files.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // the file tree's error, naming the directory like its others
	}

	entries, err := l.read(name, dir)

	err = errors.Join(err, dir.Close())
	if err != nil {
		return nil, err
	}

	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })

	return entries, nil
}

// read reads the entries of dir, the directory name, for readDir.
func (l *listing) read(name string, dir fs.File) ([]fs.DirEntry, error) {
	info, err := dir.Stat()
	if err != nil {
		return nil, err //nolint:wrapcheck // the file tree's error, naming the directory like its others
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("%s: %w", name, errReplaced)
	}

	batches, ok := dir.(fs.ReadDirFile)
	if !ok {
		entries, err := fs.ReadDir(l.files, name)
		if err != nil {
			return nil, err //nolint:wrapcheck // the file tree's error, naming the directory like its others
		}

		return entries, l.charge(name, entries)
	}

	var entries []fs.DirEntry

	for {
		batch, err := batches.ReadDir(min(listBatch, MaxPluginEntries-l.entries+1))

		chargeErr := l.charge(name, batch)
		if chargeErr != nil {
			return nil, chargeErr
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

// charge counts entries, read from the directory dir, against [MaxPluginEntries] and their paths against
// [MaxPluginPathBytes], failing with the first entry over either, which it names.
func (l *listing) charge(dir string, entries []fs.DirEntry) error {
	prefix := len(dir) + len("/")
	if dir == "." {
		prefix = 0
	}

	for _, entry := range entries {
		l.entries++
		l.paths += prefix + len(entry.Name())

		switch {
		case l.entries > MaxPluginEntries:
			return fmt.Errorf("%w: %s brings the plugin's files over %d entries",
				ErrPluginTooLarge, path.Join(dir, entry.Name()), MaxPluginEntries)
		case l.paths > MaxPluginPathBytes:
			return fmt.Errorf("%w: %s brings the paths of the plugin's files over %d bytes",
				ErrPluginTooLarge, path.Join(dir, entry.Name()), MaxPluginPathBytes)
		}
	}

	return nil
}

// fileTooLarge reports the file name of a plugin, over [MaxPluginFileSize].
func fileTooLarge(name string) error {
	return fmt.Errorf("%w: %s is over %d bytes", ErrPluginTooLarge, name, MaxPluginFileSize)
}

// pluginTooLarge reports the file name of a plugin, bringing the plugin's files over [MaxPluginSize].
func pluginTooLarge(name string) error {
	return fmt.Errorf("%w: %s brings the plugin's files over %d bytes", ErrPluginTooLarge, name, MaxPluginSize)
}

// readFile reads a regular file of a plugin's files, opened with open, of at most limit bytes: what is left for
// it, since it may have grown after its size was stated. It fails when ctx ends.
func readFile(ctx context.Context, open func(name string) (fs.File, error), file sizedFile, limit int) ([]byte, error) {
	opened, err := open(file.name)
	if err != nil {
		return nil, err
	}

	data, err := readUpTo(ctx, opened, file.size, limit+1)

	closeErr := opened.Close()
	if err == nil && closeErr != nil {
		err = fmt.Errorf("closing: %w", closeErr)
	}

	switch {
	case err != nil:
		return nil, fmt.Errorf("reading %s: %w", file.name, err)
	case len(data) > limit && limit < MaxPluginFileSize:
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

// checkLink accepts the symbolic link name of files when its target stays inside files (see judgeLink).
func checkLink(files fs.FS, name string) error {
	target, err := fs.ReadLink(files, name)

	return judgeLink(name, target, err)
}

// judgeLink accepts the symbolic link name of a plugin's files, whose target is target (or could not be read,
// failing with err), when that target stays inside the files, judged by its path alone: the copy skips the link,
// so nothing is read through it whatever it resolves to.
func judgeLink(name, target string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w: reading its target: %w", name, errSymlinkOut, err)
	}

	slashed := filepath.ToSlash(target)
	if inside := path.Join(path.Dir(name), slashed); filepath.IsAbs(target) || path.IsAbs(slashed) ||
		inside == ".." || strings.HasPrefix(inside, "../") {
		return fmt.Errorf("%s: %w (to %s)", name, errSymlinkOut, target)
	}

	return nil
}
