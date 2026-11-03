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

	"github.com/heynemann/go-plg/internal/memfs"
)

// manifestName is the file describing a plugin directory of an [FSStorage].
const manifestName = "plugin.json"

// MaxManifestSize is the largest plugin.json, in bytes: [FSStorage] and [ParseManifest] reject a bigger one with
// [ErrInvalidManifest], and a storage reading manifests from elsewhere should read at most this much plus one.
const MaxManifestSize = 64 << 10

// maxMemoryLimit is the most memory a wasm32 instance can address: 65536 pages of 64 KiB.
const maxMemoryLimit = 1 << 32

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
//   - memoryLimit (optional): the plugin's per-instance memory limit in bytes, from 1 to 4 GiB. It can only
//     lower the Register limit.
//
// A plugin.json must be a regular file (a link to one is followed only where the tree allows it) of at most
// [MaxManifestSize] bytes. Keys are case sensitive and unknown keys are errors, so a misspelt key is not
// silently ignored. Top-level entries that are not directories (symbolic links to directories included), and
// entries whose names start with a dot, are not plugins and are ignored.
//
// Open copies the plugin directory into memory, as Register copies every storage's files (see [Storage]):
// symbolic links are never followed; a link that stays inside the plugin directory is skipped, and one whose
// target is absolute or leads out of the plugin directory fails the load, naming it. The plugin then runs from
// that copy, so a file changed during the load cannot be half seen.
//
// An FSStorage is safe for concurrent use when its file tree is.
type FSStorage struct {
	fsys fs.FS  // the file tree, nil for dir
	dir  string // the directory of a [NewFSStorage], opened as an [os.Root] by every List and Open
	name string
}

var _ Storage = (*FSStorage)(nil)

// NewFSStorage returns a storage over the plugin directories in dir, named dir. List and Open read dir through
// an [os.Root], so neither a path nor a symbolic link can lead them out of dir, even when a file is replaced by
// a link while they read. Each opens dir anew: a directory moved into dir's place is seen by the next call.
func NewFSStorage(dir string) *FSStorage {
	return &FSStorage{dir: dir, name: dir}
}

// NewFSStorageFromFS returns a storage named name over the plugin directories at the root of fsys, such as an
// embed.FS (use [fs.Sub] to pick one of its directories).
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
	var descs []Descriptor

	err := s.withTree(func(fsys fs.FS) error {
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

var errPluginKey = errors.New("a plugin key is a directory name")

// Open returns a copy of the files of the plugin directory named by desc's key (see [FSStorage]).
func (s *FSStorage) Open(_ context.Context, desc Descriptor) (fs.FS, error) {
	key := desc.Key
	if key == "." || strings.Contains(key, "/") || !fs.ValidPath(key) {
		return nil, fmt.Errorf("%w %q: %w", ErrInvalidDescriptor, key, errPluginKey)
	}

	var files *memfs.FS

	err := s.withTree(func(fsys fs.FS) error {
		// Lstat: a link to a directory is not a plugin, as List does not list it.
		info, err := fs.Lstat(fsys, key)
		if err == nil && !info.IsDir() {
			err = errPluginKey
		}

		if err == nil {
			var dir fs.FS

			dir, err = fs.Sub(fsys, key)
			if err == nil {
				files, err = snapshotFiles(dir)
			}
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

// withTree calls use with the storage's file tree: its fs.FS, or its directory opened as an [os.Root], closed
// once use returns.
func (s *FSStorage) withTree(use func(fsys fs.FS) error) error {
	if s.fsys != nil {
		return use(s.fsys)
	}

	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return fmt.Errorf("plg: opening the plugin directories: %w", err)
	}

	err = use(root.FS())

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
// most [MaxManifestSize] bytes. It checks the type before opening, since opening a FIFO waits for a writer, and
// again on the opened file, and reads at most one byte past the cap.
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
	errManifestMemoryLimit = errors.New("memoryLimit must be from 1 byte to 4 GiB")
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
		if *limit == 0 || *limit > maxMemoryLimit {
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
