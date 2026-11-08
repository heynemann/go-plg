// Package memfs is an immutable in-memory file tree. plg snapshots plugin and contract sources into one at
// load time, so every instance of a plugin generation sees exactly the same files, and mounts it read-only
// into guests.
package memfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"path"
	"slices"
	"strings"
	"time"
)

// Errors returned by [Builder].
var (
	// ErrInvalidPath reports a name that is not a valid, non-root fs.FS path.
	ErrInvalidPath = errors.New("memfs: invalid path")
	// ErrConflict reports a path added twice, or used as both a file and a directory.
	ErrConflict = errors.New("memfs: conflicting path")
)

const (
	fileMode = 0o444
	dirMode  = fs.ModeDir | 0o555
)

// Builder collects files for an [FS]. The zero value is ready to use. A Builder is not safe for concurrent use.
type Builder struct {
	files map[string][]byte
}

// Add adds a file. The builder keeps data; the caller must not modify it afterwards.
func (b *Builder) Add(name string, data []byte) error {
	if !fs.ValidPath(name) || name == "." {
		return fmt.Errorf("%w: %q", ErrInvalidPath, name)
	}

	if _, dup := b.files[name]; dup {
		return fmt.Errorf("%w: %q added twice", ErrConflict, name)
	}

	if b.files == nil {
		b.files = map[string][]byte{}
	}

	b.files[name] = data

	return nil
}

// AddTree copies every regular file below root in src into the builder under prefix ("." for the top).
// Symlinks and other irregular files are skipped.
func (b *Builder) AddTree(prefix string, src fs.FS, root string) error {
	err := fs.WalkDir(src, root, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.Type().IsRegular() {
			return nil
		}

		data, err := fs.ReadFile(src, name)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}

		rel := strings.TrimPrefix(strings.TrimPrefix(name, root), "/")
		if root == "." {
			rel = name
		}

		return b.Add(path.Join(prefix, rel), data)
	})
	if err != nil {
		return fmt.Errorf("memfs: copying %s: %w", root, err)
	}

	return nil
}

// Build returns the file tree. Directories are implied by file paths.
func (b *Builder) Build() (*FS, error) {
	nodes := map[string]*node{".": {name: ".", dir: true}}

	names := make([]string, 0, len(b.files))
	for name := range b.files {
		names = append(names, name)
	}

	slices.Sort(names)

	var size int64

	for _, name := range names {
		err := addNode(nodes, name, b.files[name])
		if err != nil {
			return nil, err
		}

		size += int64(len(b.files[name]))
	}

	for _, n := range nodes {
		slices.SortFunc(n.entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	}

	return &FS{nodes: nodes, size: size}, nil
}

// addNode adds a file and the directories it implies. Build adds names in sorted order, so a file is always
// added before any path below it: the only possible conflict is a file where a directory is needed.
func addNode(nodes map[string]*node, name string, data []byte) error {
	child := &node{name: path.Base(name), data: data}
	nodes[name] = child

	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		parent, exists := nodes[dir]
		if exists && !parent.dir {
			return fmt.Errorf("%w: %q is both a file and a directory", ErrConflict, dir)
		}

		if !exists {
			parent = &node{name: path.Base(dir), dir: true}
			nodes[dir] = parent
		}

		parent.entries = append(parent.entries, child)

		if exists || dir == "." {
			return nil
		}

		child = parent
	}
}

// FS is an immutable file tree, safe for concurrent use. It implements fs.ReadFileFS, fs.ReadDirFS and
// fs.StatFS.
type FS struct {
	nodes map[string]*node
	size  int64
}

var (
	_ fs.ReadFileFS = (*FS)(nil)
	_ fs.ReadDirFS  = (*FS)(nil)
	_ fs.StatFS     = (*FS)(nil)
)

// Size returns the size of the tree's files together, in bytes.
func (f *FS) Size() int64 {
	return f.size
}

// Files yields every file's name and contents in the order fs.WalkDir visits them: each directory's entries by
// name, a subdirectory's files where its name sorts. The contents are the tree's own: do not modify them.
func (f *FS) Files() iter.Seq2[string, []byte] {
	return func(yield func(string, []byte) bool) {
		f.walk(".", yield)
	}
}

// Open opens the named file or directory.
func (f *FS) Open(name string) (fs.File, error) {
	n, err := f.lookup("open", name)
	if err != nil {
		return nil, err
	}

	if n.dir {
		return &openDir{n: n}, nil
	}

	return &openFile{n: n}, nil
}

// ReadFile returns a copy of the named file's contents.
func (f *FS) ReadFile(name string) ([]byte, error) {
	n, err := f.lookup("readfile", name)
	if err != nil {
		return nil, err
	}

	if n.dir {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: errIsDir}
	}

	return slices.Clone(n.data), nil
}

// ReadDir returns the named directory's entries, sorted by name.
func (f *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	n, err := f.lookup("readdir", name)
	if err != nil {
		return nil, err
	}

	if !n.dir {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errNotDir}
	}

	return slices.Clone(n.entries), nil
}

// Stat returns the named file's information.
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	n, err := f.lookup("stat", name)
	if err != nil {
		return nil, err
	}

	return n, nil
}

// walk yields the files below the directory dir, reporting false when yield stopped the walk.
func (f *FS) walk(dir string, yield func(string, []byte) bool) bool {
	for _, entry := range f.nodes[dir].entries {
		name := entry.Name()
		if dir != "." {
			name = dir + "/" + name
		}

		n := f.nodes[name]
		if n.dir && !f.walk(name, yield) || !n.dir && !yield(name, n.data) {
			return false
		}
	}

	return true
}

func (f *FS) lookup(op, name string) (*node, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}

	n, ok := f.nodes[name]
	if !ok {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}

	return n, nil
}

var (
	errIsDir  = errors.New("is a directory")
	errNotDir = errors.New("not a directory")
)

// node is a file or directory; it is its own fs.FileInfo and fs.DirEntry.
type node struct {
	name    string
	data    []byte
	entries []fs.DirEntry
	dir     bool
}

func (n *node) Name() string               { return n.name }
func (n *node) Size() int64                { return int64(len(n.data)) }
func (*node) ModTime() time.Time           { return time.Time{} }
func (n *node) IsDir() bool                { return n.dir }
func (*node) Sys() any                     { return nil }
func (n *node) Type() fs.FileMode          { return n.Mode().Type() }
func (n *node) Info() (fs.FileInfo, error) { return n, nil }

func (n *node) Mode() fs.FileMode {
	if n.dir {
		return dirMode
	}

	return fileMode
}

type openFile struct {
	n   *node
	off int64
}

func (f *openFile) Stat() (fs.FileInfo, error) { return f.n, nil }
func (*openFile) Close() error                 { return nil }

func (f *openFile) Read(p []byte) (int, error) {
	if f.off >= int64(len(f.n.data)) {
		return 0, io.EOF
	}

	n := copy(p, f.n.data[f.off:])
	f.off += int64(n)

	return n, nil
}

func (f *openFile) ReadAt(buf []byte, off int64) (int, error) {
	if off < 0 {
		return 0, &fs.PathError{Op: "read", Path: f.n.name, Err: fs.ErrInvalid}
	}

	if off >= int64(len(f.n.data)) {
		return 0, io.EOF
	}

	n := copy(buf, f.n.data[off:])
	if n < len(buf) {
		return n, io.EOF
	}

	return n, nil
}

func (f *openFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		offset += int64(len(f.n.data))
	default:
		return 0, &fs.PathError{Op: "seek", Path: f.n.name, Err: fs.ErrInvalid}
	}

	if offset < 0 {
		return 0, &fs.PathError{Op: "seek", Path: f.n.name, Err: fs.ErrInvalid}
	}

	f.off = offset

	return offset, nil
}

type openDir struct {
	n   *node
	off int
}

func (d *openDir) Stat() (fs.FileInfo, error) { return d.n, nil }
func (*openDir) Close() error                 { return nil }

func (d *openDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.n.name, Err: errIsDir}
}

// ReadDir implements fs.ReadDirFile: n > 0 pages through the entries and reports io.EOF at the end.
func (d *openDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.n.entries[d.off:]
	if n <= 0 {
		d.off = len(d.n.entries)

		return slices.Clone(rest), nil
	}

	if len(rest) == 0 {
		return nil, io.EOF
	}

	rest = rest[:min(n, len(rest))]
	d.off += len(rest)

	return slices.Clone(rest), nil
}
