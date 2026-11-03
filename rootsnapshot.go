package plg

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/heynemann/go-plg/internal/memfs"
)

var (
	// errSkipped reports a regular file the walk of a plugin directory listed that its copy leaves out after all:
	// the file, or a directory leading to it, became a symbolic link whose target stays inside the plugin's files.
	errSkipped = errors.New("became a symbolic link inside the plugin's files")
	// errReplaced reports an entry of a plugin directory replaced, other than by a symbolic link, while its files
	// were copied.
	errReplaced = errors.New("replaced while the plugin's files were copied")
)

// snapshotDir copies the plugin directory key of root, by the rules of [Storage], through rooted operations that
// follow no symbolic link, whatever changes while they run. The plugin directory, then each directory and file
// below it, is opened relative to its already open parent, and must then be the very entry that parent holds
// under its name (the same file as its Lstat, which follows no link), of the kind the walk listed. [os.Root]
// follows only links that stay inside the directory it opens from, so nothing outside the plugin is ever read,
// and this check refuses even those links: an entry that became a link is judged as the walk judges links
// (left out when its target stays inside the plugin's files, failing the copy otherwise), and one replaced
// otherwise fails the copy. beforeOpen, when not nil, is called with the plugin-relative name of each directory
// and file right before it is opened, once the walk listed them.
func snapshotDir(ctx context.Context, root *os.Root, key string, beforeOpen func(name string)) (*memfs.FS, error) {
	dir, err := openPluginDir(root, key)
	if err != nil {
		return nil, err
	}

	files := dirFiles{dirs: []*os.Root{dir}, beforeOpen: beforeOpen}

	copied, err := copySnapshot(ctx, dir.FS(), files.open)

	err = errors.Join(err, files.close())
	if err != nil {
		return nil, err
	}

	return copied, nil
}

// openPluginDir opens the plugin directory key of root: the directory root holds under key, not one a link leads
// to, since a link to a directory is not a plugin (List does not list it).
func openPluginDir(root *os.Root, key string) (*os.Root, error) {
	dir, err := root.OpenRoot(key)
	if err != nil {
		entry, lstatErr := root.Lstat(key)
		if lstatErr == nil && !entry.IsDir() {
			return nil, errPluginKey
		}

		return nil, err //nolint:wrapcheck // copyPlugin wraps it, naming the plugin directory
	}

	info, err := dir.Stat(".")
	if err == nil && isEntry(root, key, info) {
		return dir, nil
	}

	return nil, errors.Join(cmp.Or(err, errPluginKey), dir.Close())
}

// dirFiles opens the regular files of a plugin directory for snapshotDir. It keeps the directories leading to the
// file it opened last open: the walk lists the files of a directory together, so each directory is opened once.
type dirFiles struct {
	// closeErr holds the errors closing directories.
	closeErr   error
	beforeOpen func(name string)
	// dirs holds the plugin directory, then the directories below it leading to the file opened last, named by
	// names: nil for a directory that became a symbolic link inside the plugin's files, and for those below it.
	dirs  []*os.Root
	names []string
}

// open opens the regular file name of the plugin directory, failing with errSkipped when the copy leaves it out.
func (d *dirFiles) open(name string) (fs.File, error) {
	depth, rest := 0, name

	for {
		part, below, ok := strings.Cut(rest, "/")
		if !ok {
			break
		}

		if depth == len(d.names) || d.names[depth] != part {
			d.closeBelow(depth)

			err := d.descend(part, name[:len(name)-len(below)-1])
			if err != nil {
				return nil, err
			}
		}

		depth++
		rest = below
	}

	d.closeBelow(depth)

	dir := d.dirs[depth]
	if dir == nil {
		return nil, errSkipped
	}

	return openFile(dir, rest, name, d.beforeOpen)
}

// descend opens the directory part of the last directory open, named name in the plugin directory.
func (d *dirFiles) descend(part, name string) error {
	var dir *os.Root

	if parent := d.dirs[len(d.dirs)-1]; parent != nil {
		var err error

		dir, err = openDir(parent, part, name, d.beforeOpen)
		if err != nil && !errors.Is(err, errSkipped) {
			return err
		}
	}

	d.dirs = append(d.dirs, dir)
	d.names = append(d.names, part)

	return nil
}

// closeBelow closes the directories open below the one at depth (the plugin directory is at 0).
func (d *dirFiles) closeBelow(depth int) {
	for _, dir := range d.dirs[depth+1:] {
		if dir != nil {
			d.closeErr = errors.Join(d.closeErr, dir.Close())
		}
	}

	clear(d.dirs[depth+1:])

	d.dirs, d.names = d.dirs[:depth+1], d.names[:depth]
}

// close closes every directory still open, the plugin directory included, returning the errors closing them.
func (d *dirFiles) close() error {
	d.closeBelow(0)

	return errors.Join(d.closeErr, d.dirs[0].Close())
}

// openDir opens the directory part of parent, named name in the plugin directory (see snapshotDir).
func openDir(parent *os.Root, part, name string, beforeOpen func(name string)) (*os.Root, error) {
	if beforeOpen != nil {
		beforeOpen(name)
	}

	dir, err := parent.OpenRoot(part)
	if err != nil {
		return nil, changed(parent, part, name, fs.ModeDir, err)
	}

	info, err := dir.Stat(".")
	if err == nil && isEntry(parent, part, info) {
		return dir, nil
	}

	return nil, errors.Join(changed(parent, part, name, fs.ModeDir, err), dir.Close())
}

// openFile opens the regular file part of parent, named name in the plugin directory (see snapshotDir).
func openFile(parent *os.Root, part, name string, beforeOpen func(name string)) (fs.File, error) {
	if beforeOpen != nil {
		beforeOpen(name)
	}

	file, err := parent.Open(part)
	if err != nil {
		return nil, changed(parent, part, name, 0, err)
	}

	info, err := file.Stat()
	if err == nil && info.Mode().IsRegular() && isEntry(parent, part, info) {
		return file, nil
	}

	return nil, errors.Join(changed(parent, part, name, 0, err), file.Close())
}

// isEntry reports whether info is of the entry parent holds under name: the same file as its Lstat, which follows
// no link.
func isEntry(parent *os.Root, name string, info fs.FileInfo) bool {
	entry, err := parent.Lstat(name)

	return err == nil && os.SameFile(info, entry)
}

// changed tells why the entry part of parent, named name in the plugin directory, could not be opened with
// openErr, or is not what was opened as the kind of entry the walk listed (fs.ModeDir, or 0 for a regular file).
// A symbolic link is judged as the walk judges links (errSkipped when its target stays inside the plugin's files);
// an entry of that kind fails with openErr, and any other with errReplaced.
func changed(parent *os.Root, part, name string, kind fs.FileMode, openErr error) error {
	entry, err := parent.Lstat(part)

	switch {
	case err != nil:
		return fmt.Errorf("reading %s: %w", name, cmp.Or(openErr, err))
	case entry.Mode()&fs.ModeSymlink != 0:
		target, readErr := parent.Readlink(part)

		err = judgeLink(name, target, readErr)
		if err == nil {
			err = errSkipped
		}

		return err
	case openErr != nil && entry.Mode().Type() == kind:
		return fmt.Errorf("reading %s: %w", name, openErr)
	default:
		return fmt.Errorf("%s: %w", name, errReplaced)
	}
}
