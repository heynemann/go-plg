package authorrepo

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/modfetch"
)

// Modes of what Compose creates.
const (
	dirMode  = 0o750
	fileMode = 0o600
)

var (
	// ErrCompose reports a shared directory that cannot be copied into a plugin's files.
	ErrCompose = errors.New("cannot copy a shared directory into the plugin")

	errPlaced = errors.New("the plugin already has")
)

// Placement is where Compose copied a shared directory into a plugin's files.
type Placement struct {
	// Shared is the shared directory, as the repository lists it.
	Shared string
	// At is its path in the plugin's files (slash-separated).
	At string
}

// treeCopy is a directory to copy: from, into the directory into below the destination.
type treeCopy struct {
	from, into string
}

// Compose copies the plugin directory plugin (as the repository lists it) into the empty directory dst, and every
// shared directory into it: at the shared directory's path in the repository for Lua and TypeScript plugins
// (goPackage empty), so an import or require of that path from the plugin's root finds it; for a Go plugin, whose
// package is the directory goPackage of its files, under goPackage/vendor at the shared directory's import path
// (its go.mod's module path and its path below it), as Go vendors packages, and only shared directories holding Go
// files. Symbolic links are copied as links; other irregular files are left out, as a storage leaves them out.
// Everything is written through a root opened on dst, so nothing lands outside it, not even through a link of the
// plugin's on the way to a shared directory's place (ui -> /elsewhere with a shared ui/kit fails).
// It fails with [ErrCompose] when a shared directory would replace a file of the plugin or leave dst, when a Go
// shared directory is in no module, and with [plg.ErrPluginTooLarge] when the files to copy are over the limits of
// a plugin's files, before copying them.
func (r *Repo) Compose(dst, plugin, goPackage string) ([]Placement, error) {
	src := filepath.Join(r.Dir, filepath.FromSlash(plugin))
	copies := []treeCopy{{from: src, into: "."}}
	placements := make([]Placement, 0, len(r.Shared))

	for _, shared := range r.Shared {
		placement, err := r.place(src, shared, goPackage)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrCompose, plugin, err)
		}

		if placement != nil {
			placements = append(placements, *placement)
			copies = append(copies, treeCopy{
				from: filepath.Join(r.Dir, filepath.FromSlash(shared)),
				into: filepath.FromSlash(placement.At),
			})
		}
	}

	err := checkSizes(copies)
	if err != nil {
		return nil, err
	}

	err = copyInto(dst, copies)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCompose, err)
	}

	return placements, nil
}

// place returns where the shared directory goes in the files of the plugin in the directory src; nil when it
// does not go there (a Go plugin takes only shared directories holding Go files). The plugin may not hold that
// path.
func (r *Repo) place(src, shared, goPackage string) (*Placement, error) {
	placement := &Placement{Shared: shared, At: shared}

	if goPackage != "" {
		dir := filepath.Join(r.Dir, filepath.FromSlash(shared))

		hasGo, err := holdsGo(dir)
		if err != nil || !hasGo {
			return nil, err
		}

		importPath, err := modfetch.ImportPath(dir)
		if err != nil {
			return nil, fmt.Errorf("%s holds Go files, which Go plugins import by their import path: %w", shared, err)
		}

		placement.At = path.Join(goPackage, "vendor", importPath)
	}

	_, err := os.Lstat(filepath.Join(src, filepath.FromSlash(placement.At)))
	if err == nil {
		return nil, fmt.Errorf("%w %s, where %s is copied", errPlaced, placement.At, shared)
	}

	return placement, nil
}

// holdsGo reports whether dir or a directory below it holds a Go source file.
func holdsGo(dir string) (bool, error) {
	found := false

	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() && filepath.Ext(name) == ".go" {
			found = true

			return fs.SkipAll
		}

		return err
	})

	return found, err //nolint:wrapcheck // the error names the file
}

// checkSizes fails with plg.ErrPluginTooLarge when a regular file to copy is over plg.MaxPluginFileSize bytes or
// all of them over plg.MaxPluginSize bytes together, as a storage's limits would fail the plugin, so nothing too
// large is copied.
func checkSizes(copies []treeCopy) error {
	total := int64(0)

	for _, c := range copies {
		err := filepath.WalkDir(c.from, func(name string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() {
				return err
			}

			info, err := entry.Info()
			if err == nil {
				total += info.Size()
				err = checkSize(name, info.Size(), total)
			}

			return err
		})
		if err != nil {
			return fmt.Errorf("%w: %w", ErrCompose, err)
		}
	}

	return nil
}

// checkSize checks the size of the file name, total being what the files copied so far add up to with it.
func checkSize(name string, size, total int64) error {
	if size > plg.MaxPluginFileSize || total > plg.MaxPluginSize {
		return fmt.Errorf("%w: %s, with the plugin's other files, is over %d bytes per file or %d in all",
			plg.ErrPluginTooLarge, name, plg.MaxPluginFileSize, plg.MaxPluginSize)
	}

	return nil
}

// copyInto copies every tree into its directory below dst, which it creates, through a root opened on dst.
func copyInto(dst string, copies []treeCopy) error {
	err := os.MkdirAll(dst, dirMode)
	if err != nil {
		return err //nolint:wrapcheck // the error names the directory
	}

	root, err := os.OpenRoot(dst)
	if err != nil {
		return err //nolint:wrapcheck // the error names the directory
	}

	for _, c := range copies {
		err = copyTree(root, c.from, c.into)
		if err != nil {
			break
		}
	}

	return errors.Join(err, root.Close())
}

// copyTree copies the directory from into the directory into of the root dst, which it creates: directories,
// regular files and symbolic links (as links), each opened relative to its root so that no link leads out of it.
func copyTree(dst *os.Root, from, into string) error {
	err := dst.MkdirAll(into, dirMode)
	if err != nil {
		return err //nolint:wrapcheck // the error names the directory
	}

	src, err := os.OpenRoot(from)
	if err != nil {
		return err //nolint:wrapcheck // the error names the directory
	}

	defer src.Close() //nolint:errcheck // read only

	target, err := dst.OpenRoot(into)
	if err != nil {
		return err //nolint:wrapcheck // the error names the directory
	}

	err = fs.WalkDir(src.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}

		return copyEntry(src, target, name, entry.Type())
	})

	return errors.Join(err, target.Close())
}

// copyEntry copies the entry name, of type mode, from the root src to the root dst.
func copyEntry(src, dst *os.Root, name string, mode fs.FileMode) error {
	switch {
	case mode.IsDir():
		return dst.Mkdir(name, dirMode) //nolint:wrapcheck // the error names the directory
	case mode.IsRegular():
		return copyFile(src, dst, name)
	case mode&fs.ModeSymlink != 0:
		link, err := src.Readlink(name)
		if err != nil {
			return err //nolint:wrapcheck // the error names the link
		}

		return dst.Symlink(link, name) //nolint:wrapcheck // the error names the link
	default: // other irregular files are left out
		return nil
	}
}

// copyFile copies the regular file name from the root src to the new file name in the root dst.
func copyFile(src, dst *os.Root, name string) error {
	source, err := src.Open(name)
	if err != nil {
		return err //nolint:wrapcheck // the error names the file
	}

	defer source.Close() //nolint:errcheck // read only

	target, err := dst.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err //nolint:wrapcheck // the error names the file
	}

	_, err = io.Copy(target, source)

	return errors.Join(err, target.Close())
}
