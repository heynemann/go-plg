package plg

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
// esbuild bundles the entrypoint and the files it imports, by relative paths inside the plugin only, and
// QuickJS-ng runs the bundle with the ECMAScript built-ins plus console (to the log), host (the [WithHost]
// interface's methods, returning Promises) and fetch (through [WithHTTP]'s policy). Values cross by the interface's
// types: struct fields by json name, nil slices, maps and pointers as null, []byte as Uint8Array, time.Time as
// {seconds, nanos} (a Date too, on the way back), several results as an array; a thrown error is the call's
// error. Running TypeScript plugins needs the guest: import _ "github.com/heynemann/go-plg/guest/typescript".
const LanguageTypeScript Language = "typescript"

// Storage lists plugins and opens their sources. A storage belongs to one Register call: every plugin it lists
// implements that call's interface. Implementations must be safe for concurrent use.
//
// Register reads an opened plugin's files exactly once, into an in-memory snapshot it hashes (for an empty
// Version), bundles and loads, so the files cannot change between those steps. The snapshot holds every regular
// file. Symbolic links (as fs.ReadDir reports them) are never followed: one that stays inside the plugin's files
// is skipped, as if it were not there, and one whose target is absolute, leads out of the plugin's files or
// cannot be read (the fs.FS is no fs.ReadLinkFS) fails the load, naming it. Other irregular files are skipped.
type Storage interface {
	// Name identifies the storage in errors and logs.
	Name() string
	// List returns the plugins the storage holds.
	List(ctx context.Context) ([]Descriptor, error)
	// Open returns the plugin's source tree; the Descriptor's Entrypoint is a path inside it. Register copies
	// the tree once, as described above, before using it.
	Open(ctx context.Context, d Descriptor) (fs.FS, error)
}

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
// [Storage]), failing on a symbolic link out of them, so later changes to files are not seen.
func (s *MemoryStorage) Put(desc Descriptor, files fs.FS) error {
	if desc.Key == "" {
		return fmt.Errorf("%w: memory storage %s: empty key", ErrInvalidDescriptor, s.name)
	}

	copied, err := snapshotFiles(files)
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

// snapshotFiles returns an immutable copy of a plugin's files, as [Storage] describes. A [memfs.FS], which the
// built-in storages return, is one already.
func snapshotFiles(files fs.FS) (*memfs.FS, error) {
	if frozen, ok := files.(*memfs.FS); ok {
		return frozen, nil
	}

	var tree memfs.Builder

	err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.Type().IsRegular():
			data, readErr := fs.ReadFile(files, name)
			if readErr != nil {
				return fmt.Errorf("reading %s: %w", name, readErr)
			}

			return tree.Add(name, data)
		case entry.Type()&fs.ModeSymlink != 0:
			return checkLink(files, name)
		default:
			return nil
		}
	})
	if err != nil {
		return nil, fmt.Errorf("copying the plugin's files: %w", err)
	}

	copied, err := tree.Build()
	if err != nil {
		return nil, fmt.Errorf("copying the plugin's files: %w", err)
	}

	return copied, nil
}

// checkLink accepts the symbolic link name of files when its target stays inside files, judged by the target's
// path alone: snapshotFiles skips it, so nothing is read through it whatever it resolves to.
func checkLink(files fs.FS, name string) error {
	target, err := fs.ReadLink(files, name)
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
