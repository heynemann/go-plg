package plg

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/bytedance/sonic"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/memfs"
)

// manifestName is the file describing a plugin directory of an [FSStorage].
const manifestName = "plugin.json"

// MaxManifestSize is the largest plugin.json, in bytes: [FSStorage] and [ParseManifest] reject a bigger one with
// [ErrInvalidManifest], and a storage reading manifests from elsewhere should read at most this much plus one.
const MaxManifestSize = 64 << 10

// The memory limits a Register, a descriptor or a plugin.json may ask for: from one wasm page (64 KiB) to the
// most a wasm32 instance can address, 65536 pages (4 GiB).
const (
	minMemoryLimit = engine.PageSize
	maxMemoryLimit = 1 << 32
)

// validMemoryLimit reports whether limit is from [minMemoryLimit] to [maxMemoryLimit], the one rule shared by
// WithMemoryLimit, Register's descriptor check and plugin.json decoding.
func validMemoryLimit(limit uint64) bool {
	return limit >= minMemoryLimit && limit <= maxMemoryLimit
}

// contentVersionPrefix starts every version computed from a plugin's files.
const contentVersionPrefix = "sha256:"

// FSStorage is a [Storage] over a file tree holding one plugin per top-level directory. The directory's name is
// the plugin's key, and the directory holds the plugin's files and a plugin.json manifest:
//
//	{"language": "go", "entrypoint": "plugin.go", "version": "1.4.0", "memoryLimit": 67108864}
//
// Its keys are:
//   - language (required): "go", "lua" or "typescript".
//   - entrypoint (required): a slash-separated path inside the plugin directory; see [Descriptor].
//   - version (optional): the plugin's revision. Without it, List leaves the Descriptor's Version empty and
//     Register uses "sha256:" and the hex SHA-256 of the regular files it loads (their paths relative to the
//     directory and their contents): it is stable, and changes exactly when a file is added, removed, renamed
//     or edited.
//   - memoryLimit (optional): the plugin's per-instance memory limit in bytes, from 65536 (one wasm page) to
//     4 GiB, as Register requires of every descriptor. It can only lower the Register limit.
//
// A plugin.json must be a regular file (a link to one is followed only where the tree allows it) of at most
// [MaxManifestSize] bytes. Keys are case sensitive and unknown keys are errors, so a misspelt key is not
// silently ignored. Top-level entries that are not directories (symbolic links to directories included), and
// entries whose names start with a dot, are not plugins and are ignored.
//
// Open copies the plugin directory into memory, as Register copies every storage's files (see [Storage]):
// symbolic links are never followed; a link that stays inside the plugin directory is skipped, and one whose
// target is absolute or leads out of the plugin directory fails the load, naming it. A file over
// [MaxPluginFileSize] bytes, files over [MaxPluginSize] bytes together, more than [MaxPluginEntries] entries or
// entry paths over [MaxPluginPathBytes] bytes together fail it with [ErrPluginTooLarge] before they are read (the
// directories in bounded batches), and the copy stops when Open's context ends. The plugin then runs from that
// copy, so a file changed during the load cannot be half seen.
//
// Over a directory ([NewFSStorage]) these rules hold even while others write to it: the plugin directory, then
// each directory and file below it, is opened relative to its open parent and must then be the very entry that
// parent holds under its name, so an entry swapped for a link during the copy is judged as a link (left out, or
// failing the load, never followed), and one replaced otherwise fails the load. No open there waits: a
// plugin.json, directory or file swapped for a FIFO (which an ordinary open waits on for a writer) fails List or
// the load at once. Over an fs.FS ([NewFSStorageFromFS]) the copy can only trust what the fs.FS reports, as
// [Storage] describes, and an fs.FS's open of an entry swapped for a FIFO may wait.
//
// An FSStorage is safe for concurrent use when its file tree is.
type FSStorage struct {
	fsys fs.FS  // the file tree, nil for dir
	dir  string // the directory of a [NewFSStorage], opened as an [os.Root] by every List and Open
	name string
}

var _ Storage = (*FSStorage)(nil)

// NewFSStorage returns a storage over the plugin directories in dir, named dir. List and Open read dir through
// an [os.Root], so neither a path nor a symbolic link can lead them out of dir, and Open's copy of a plugin
// directory follows no link, even one swapped in while it copies (see [FSStorage]). Each opens dir anew: a
// directory moved into dir's place is seen by the next call.
func NewFSStorage(dir string) *FSStorage {
	return &FSStorage{dir: dir, name: dir}
}

// NewFSStorageFromFS returns a storage named name over the plugin directories at the root of fsys, such as an
// embed.FS (use [fs.Sub] to pick one of its directories). Open copies a plugin directory through fsys's own
// interface, so a link swapped into fsys during the copy is followed if fsys follows it, and an entry swapped for
// a FIFO may block fsys's open of it (see [Storage]): use [NewFSStorage], not an [os.DirFS], for a directory
// others can write to.
func NewFSStorageFromFS(name string, fsys fs.FS) *FSStorage {
	return &FSStorage{fsys: fsys, name: name}
}

// Name returns the storage's name.
func (s *FSStorage) Name() string {
	return s.name
}

// List reads every plugin directory's plugin.json and returns the plugins, sorted by key. It fails with
// [ErrInvalidManifest], naming the directory, if a plugin.json is missing or malformed.
func (s *FSStorage) List(ctx context.Context) ([]Descriptor, error) {
	return s.listCalling(ctx, nil)
}

var errPluginKey = errors.New("a plugin key is a directory name")

// Open returns a copy of the files of the plugin directory named by desc's key (see [FSStorage]).
func (s *FSStorage) Open(ctx context.Context, desc Descriptor) (fs.FS, error) {
	return s.copyPlugin(ctx, desc, openHooks{})
}

// listCalling is List; beforeOpen, when not nil, is called with each name of the storage's directory (not of an
// fs.FS tree) right before it is opened.
func (s *FSStorage) listCalling(ctx context.Context, beforeOpen func(name string)) ([]Descriptor, error) {
	var descs []Descriptor

	err := s.withTree(beforeOpen, func(fsys fs.FS, _ *os.Root) error {
		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			return fmt.Errorf("plg: reading the plugin directories: %w", err)
		}

		descs = make([]Descriptor, 0, len(entries))

		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}

			err = ctx.Err()
			if err != nil {
				return fmt.Errorf("plg: listing plugins: %w", err)
			}

			desc, err := describe(fsys, entry.Name())
			if err != nil {
				return err
			}

			descs = append(descs, desc)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return descs, nil
}

// copyPlugin is Open; hooks are called as snapshotDir describes.
func (s *FSStorage) copyPlugin(ctx context.Context, desc Descriptor, hooks openHooks) (fs.FS, error) {
	key := desc.Key
	if key == "." || strings.Contains(key, "/") || !fs.ValidPath(key) {
		return nil, fmt.Errorf("%w %q: %w", ErrInvalidDescriptor, key, errPluginKey)
	}

	var files *memfs.FS

	err := s.withTree(nil, func(fsys fs.FS, root *os.Root) error {
		var err error

		if root != nil {
			files, err = snapshotDir(ctx, root, key, hooks)
		} else {
			files, err = snapshotSub(ctx, fsys, key)
		}

		if err != nil {
			return fmt.Errorf("plg: plugin directory %s: %w", key, err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return files, nil
}

// snapshotSub copies the plugin directory key of fsys, through fsys's own interface (see [Storage]).
func snapshotSub(ctx context.Context, fsys fs.FS, key string) (*memfs.FS, error) {
	// Lstat: a link to a directory is not a plugin, as List does not list it.
	info, err := fs.Lstat(fsys, key)
	if err == nil && !info.IsDir() {
		err = errPluginKey
	}

	var dir fs.FS

	if err == nil {
		dir, err = fs.Sub(fsys, key)
	}

	if err != nil {
		return nil, err //nolint:wrapcheck // copyPlugin wraps it, naming the plugin directory
	}

	return snapshotFiles(ctx, dir)
}

// withTree calls use with the storage's file tree: its fs.FS, with a nil root, or its directory opened as an
// [os.Root] and that root as an fs.FS whose opens never wait (calling beforeOpen, when not nil, right before
// each), closed once use returns.
func (s *FSStorage) withTree(beforeOpen func(name string), use func(fsys fs.FS, root *os.Root) error) error {
	if s.fsys != nil {
		return use(s.fsys, nil)
	}

	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return fmt.Errorf("plg: opening the plugin directories: %w", err)
	}

	err = use(nonblockingFS{root: root, beforeOpen: beforeOpen}, root)

	closeErr := root.Close()
	if closeErr != nil {
		err = errors.Join(err, fmt.Errorf("plg: closing the plugin directories: %w", closeErr))
	}

	return err
}

// describe reads the plugin directory dir of fsys. A plugin.json without a version leaves Version empty:
// Register hashes the files it loads.
func describe(fsys fs.FS, dir string) (Descriptor, error) {
	data, err := readManifest(fsys, path.Join(dir, manifestName))
	if err != nil {
		return Descriptor{}, fmt.Errorf("%w: plugin directory %s: %w", ErrInvalidManifest, dir, err)
	}

	desc, err := decodeManifest(data)
	if err != nil {
		return Descriptor{}, fmt.Errorf("%w: plugin directory %s: %w", ErrInvalidManifest, dir, err)
	}

	desc.Key = dir

	return desc, nil
}

var (
	errManifestFile = errors.New("plugin.json is not a regular file")
	errManifestSize = fmt.Errorf("plugin.json is over %d bytes", MaxManifestSize)
)

// readManifest reads the plugin.json name of fsys: a regular file (followed through links the tree allows) of at
// most [MaxManifestSize] bytes. It checks the type before opening, since opening a FIFO can wait for a writer, and
// again on the opened file, and reads at most one byte past the cap. The storage's directory is read through a
// nonblockingFS, whose open never waits, so a plugin.json replaced by a FIFO after the first check fails the
// second; an fs.FS tree's open may wait (see [FSStorage]).
func readManifest(fsys fs.FS, name string) ([]byte, error) {
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, err //nolint:wrapcheck // describe names the plugin directory
	}

	if !info.Mode().IsRegular() {
		return nil, errManifestFile
	}

	file, err := fsys.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // describe names the plugin directory
	}

	data, err := readRegular(file)

	closeErr := file.Close()
	if err == nil && closeErr != nil {
		err = fmt.Errorf("closing: %w", closeErr)
	}

	return data, err
}

// readRegular reads file, which must still be a regular file, up to one byte past [MaxManifestSize].
func readRegular(file fs.File) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}

	if !info.Mode().IsRegular() {
		return nil, errManifestFile
	}

	data, err := io.ReadAll(io.LimitReader(file, MaxManifestSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading: %w", err)
	}

	if len(data) > MaxManifestSize {
		return nil, errManifestSize
	}

	return data, nil
}

// manifest is the content of a plugin.json.
type manifest struct {
	MemoryLimit *uint64  `json:"memoryLimit"`
	Language    Language `json:"language"`
	Entrypoint  string   `json:"entrypoint"`
	Version     string   `json:"version"`
}

var (
	errManifestLanguage    = errors.New(`language must be "go", "lua" or "typescript"`)
	errManifestEntrypoint  = errors.New("entrypoint must be a slash-separated path inside the plugin directory")
	errManifestMemoryLimit = fmt.Errorf("memoryLimit must be from %d bytes (one wasm page) to 4 GiB", minMemoryLimit)
)

// ParseManifest parses and checks a plugin.json exactly as [FSStorage] does, for storages keeping the same
// manifest in another place (an object store, a database). The returned Descriptor has no Key, and an empty
// Version when the manifest gives none: the storage then picks one (Register falls back to a content hash).
// A malformed manifest, or one over [MaxManifestSize] bytes, fails with [ErrInvalidManifest].
func ParseManifest(data []byte) (Descriptor, error) {
	desc, err := decodeManifest(data)
	if err != nil {
		return Descriptor{}, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}

	return desc, nil
}

// decodeManifest parses and checks a plugin.json; the returned descriptor has no key.
func decodeManifest(data []byte) (Descriptor, error) {
	if len(data) > MaxManifestSize {
		return Descriptor{}, errManifestSize
	}

	strict := sonic.Config{
		CaseSensitive:         true,
		DisallowUnknownFields: true,
		ValidateString:        true,
		UseUnicodeErrors:      true,
	}.Froze()

	var parsed manifest

	err := strict.Unmarshal(data, &parsed)
	if err != nil {
		return Descriptor{}, fmt.Errorf("parsing: %w", err)
	}

	switch parsed.Language {
	case LanguageGo, LanguageLua, LanguageTypeScript:
	default:
		return Descriptor{}, fmt.Errorf("%w, not %q", errManifestLanguage, parsed.Language)
	}

	if parsed.Entrypoint == "" || !fs.ValidPath(parsed.Entrypoint) {
		return Descriptor{}, fmt.Errorf("%w, not %q", errManifestEntrypoint, parsed.Entrypoint)
	}

	desc := Descriptor{Language: parsed.Language, Entrypoint: parsed.Entrypoint, Version: parsed.Version}

	if limit := parsed.MemoryLimit; limit != nil {
		if !validMemoryLimit(*limit) {
			return Descriptor{}, fmt.Errorf("%w, not %d", errManifestMemoryLimit, *limit)
		}

		desc.MemoryLimit = *limit
	}

	return desc, nil
}

// contentVersion returns the version of the files below root in fsys: a SHA-256 over every regular file's path
// relative to root and the SHA-256 of its contents, in lexical order. Register hashes the snapshot it loads,
// which holds regular files only.
func contentVersion(fsys fs.FS, root string) (string, error) {
	sum, file := sha256.New(), sha256.New()
	buf := make([]byte, 0, binary.MaxVarintLen64+sha256.Size)
	copyBuf := make([]byte, copyBufferSize)

	prefix := root + "/"
	if root == "." {
		prefix = ""
	}

	err := fs.WalkDir(fsys, root, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.Type().IsRegular() {
			return nil
		}

		file.Reset()

		err = copyFile(file, fsys, name, copyBuf)
		if err != nil {
			return err
		}

		rel := strings.TrimPrefix(name, prefix)
		buf = binary.AppendVarint(buf[:0], int64(len(rel)))
		buf = append(buf, rel...)
		buf = file.Sum(buf)
		sum.Write(buf)

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hashing the plugin's files: %w", err)
	}

	version := make([]byte, 0, len(contentVersionPrefix)+hex.EncodedLen(sha256.Size))
	version = append(version, contentVersionPrefix...)
	version = hex.AppendEncode(version, sum.Sum(buf[:0]))

	return string(version), nil
}

// copyBufferSize is the size of the buffer contentVersion reads files through; plugin sources are small.
const copyBufferSize = 4 << 10

// copyFile writes the contents of the file name in fsys to dst, through buf.
func copyFile(dst io.Writer, fsys fs.FS, name string, buf []byte) error {
	src, err := fsys.Open(name)
	if err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}

	// Hiding src's WriteTo makes io.CopyBuffer use buf: *os.File's WriteTo allocates its own buffer per call.
	_, err = io.CopyBuffer(dst, struct{ io.Reader }{src}, buf)

	err = errors.Join(err, src.Close())
	if err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}

	return nil
}
