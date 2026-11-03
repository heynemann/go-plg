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
)

// manifestName is the file describing a plugin directory of an [FSStorage].
const manifestName = "plugin.json"

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
//   - version (optional): the plugin's revision. Without it, the version is "sha256:" and the hex SHA-256 of
//     the directory's regular files (their paths relative to the directory and their contents): it is stable,
//     and changes exactly when a file is added, removed, renamed or edited.
//   - memoryLimit (optional): the plugin's per-instance memory limit in bytes, from 1 to 4 GiB. It can only
//     lower the Register limit.
//
// Keys are case sensitive and unknown keys are errors, so a misspelt key is not silently ignored. Top-level
// entries that are not directories, and entries whose names start with a dot, are not plugins and are
// ignored. Symlinked files are skipped, as Register skips them when it loads a plugin.
//
// An FSStorage is safe for concurrent use when its file tree is.
type FSStorage struct {
	fsys fs.FS
	name string
}

var _ Storage = (*FSStorage)(nil)

// NewFSStorage returns a storage over the plugin directories in dir, named dir.
func NewFSStorage(dir string) *FSStorage {
	return &FSStorage{fsys: os.DirFS(dir), name: dir}
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
	entries, err := fs.ReadDir(s.fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("plg: reading the plugin directories: %w", err)
	}

	descs := make([]Descriptor, 0, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		err = ctx.Err()
		if err != nil {
			return nil, fmt.Errorf("plg: listing plugins: %w", err)
		}

		desc, err := s.describe(entry.Name())
		if err != nil {
			return nil, err
		}

		descs = append(descs, desc)
	}

	return descs, nil
}

var errPluginKey = errors.New("a plugin key is a directory name")

// Open returns the files of the plugin directory named by desc's key.
func (s *FSStorage) Open(_ context.Context, desc Descriptor) (fs.FS, error) {
	key := desc.Key
	if key == "." || strings.Contains(key, "/") {
		return nil, fmt.Errorf("%w %q: %w", ErrInvalidDescriptor, key, errPluginKey)
	}

	files, err := fs.Sub(s.fsys, key)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrInvalidDescriptor, key, err)
	}

	info, err := fs.Stat(s.fsys, key)
	if err == nil && !info.IsDir() {
		err = errPluginKey
	}

	if err != nil {
		return nil, fmt.Errorf("plg: plugin directory %s: %w", key, err)
	}

	return files, nil
}

// describe reads the plugin directory dir.
func (s *FSStorage) describe(dir string) (Descriptor, error) {
	data, err := fs.ReadFile(s.fsys, path.Join(dir, manifestName))
	if err != nil {
		return Descriptor{}, fmt.Errorf("%w: plugin directory %s: %w", ErrInvalidManifest, dir, err)
	}

	desc, err := decodeManifest(data)
	if err != nil {
		return Descriptor{}, fmt.Errorf("%w: plugin directory %s: %w", ErrInvalidManifest, dir, err)
	}

	desc.Key = dir

	if desc.Version == "" {
		desc.Version, err = contentVersion(s.fsys, dir)
		if err != nil {
			return Descriptor{}, fmt.Errorf("plg: plugin directory %s: %w", dir, err)
		}
	}

	return desc, nil
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
// A malformed manifest fails with [ErrInvalidManifest].
func ParseManifest(data []byte) (Descriptor, error) {
	desc, err := decodeManifest(data)
	if err != nil {
		return Descriptor{}, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}

	return desc, nil
}

// decodeManifest parses and checks a plugin.json; the returned descriptor has no key.
func decodeManifest(data []byte) (Descriptor, error) {
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
// relative to root and the SHA-256 of its contents, in lexical order. Irregular files (symlinks included) are
// skipped, as [memfs.Builder.AddTree] skips them when a plugin loads.
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
