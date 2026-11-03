package plg

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"slices"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/heynemann/go-plg/internal/memfs"
)

// Language is a plugin's source language.
type Language string

// LanguageGo is a Go plugin: a package declaring func Create() (<interface>, error). Running Go plugins
// needs the Go guest: import _ "github.com/heynemann/go-plg/guest/golang".
const LanguageGo Language = "go"

// Storage lists plugins and opens their sources. A storage belongs to one Register call: every plugin it lists
// implements that call's interface. Implementations must be safe for concurrent use.
type Storage interface {
	// Name identifies the storage in errors and logs.
	Name() string
	// List returns the plugins the storage holds.
	List(ctx context.Context) ([]Descriptor, error)
	// Open returns the plugin's source tree; the Descriptor's Entrypoint is a path inside it.
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
	// Version identifies the plugin's revision.
	Version string
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

// Put adds the plugin desc, or replaces the plugin with the same key. It copies every regular file of files,
// so later changes to files are not seen.
func (s *MemoryStorage) Put(desc Descriptor, files fs.FS) error {
	if desc.Key == "" {
		return fmt.Errorf("%w: memory storage %s: empty key", ErrInvalidDescriptor, s.name)
	}

	var tree memfs.Builder

	err := tree.AddTree(".", files, ".")
	if err != nil {
		return fmt.Errorf("plg: memory storage %s: plugin %s: %w", s.name, desc.Key, err)
	}

	snapshot, err := tree.Build()
	if err != nil {
		return fmt.Errorf("plg: memory storage %s: plugin %s: %w", s.name, desc.Key, err)
	}

	s.plugins.Store(desc.Key, memoryPlugin{desc: desc, files: snapshot})

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
