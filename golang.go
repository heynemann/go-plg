package plg

import (
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/internal/memfs"
)

// goGuestImport is the package that links the Go guest.
const goGuestImport = "github.com/heynemann/go-plg/guest/golang"

// guestFor returns the linked guest that runs lang plugins.
func guestFor(lang Language) (guestreg.Guest, error) {
	if lang != LanguageGo {
		return guestreg.Guest{}, fmt.Errorf("%w %q", ErrUnsupportedLanguage, lang)
	}

	guest, ok := guestreg.Lookup(string(lang))
	if !ok {
		return guestreg.Guest{}, fmt.Errorf("%w: %s plugins need the program to import _ %q",
			ErrGuestNotLinked, lang, goGuestImport)
	}

	return guest, nil
}

var (
	errEntrypointPath = errors.New("entrypoint is not a valid path")
	errEntrypointFile = errors.New("entrypoint is neither a directory nor a .go file")
)

// goSourceTree returns the tree a Go plugin instance mounts at [goabi.GoPath]: the contract package's source
// at its import path, the plugin's package directory at [goabi.PluginImportPath] and the dispatcher at
// [goabi.DispatchImportPath].
func goSourceTree(contr *contract, dispatcher []byte, files fs.FS, entrypoint string) (*memfs.FS, error) {
	dir, err := goPackageDir(files, entrypoint)
	if err != nil {
		return nil, err
	}

	var tree memfs.Builder

	err = tree.AddTree(path.Join("src", contr.iface.PkgPath()), contr.source, ".")
	if err == nil {
		err = tree.AddTree(path.Join("src", goabi.PluginImportPath), files, dir)
	}

	if err == nil {
		err = tree.Add(path.Join("src", goabi.DispatchImportPath, "dispatch.go"), dispatcher)
	}

	if err != nil {
		return nil, fmt.Errorf("copying sources: %w", err)
	}

	mounted, err := tree.Build()
	if err != nil {
		return nil, fmt.Errorf("copying sources: %w", err)
	}

	return mounted, nil
}

// goPackageDir returns the plugin's package directory: the entrypoint itself, or the directory of an
// entrypoint .go file.
func goPackageDir(files fs.FS, entrypoint string) (string, error) {
	if !fs.ValidPath(entrypoint) {
		return "", fmt.Errorf("%w: %q", errEntrypointPath, entrypoint)
	}

	info, err := fs.Stat(files, entrypoint)
	if err != nil {
		return "", fmt.Errorf("entrypoint: %w", err)
	}

	switch {
	case info.IsDir():
		return entrypoint, nil
	case info.Mode().IsRegular() && path.Ext(entrypoint) == ".go":
		return path.Dir(entrypoint), nil
	default:
		return "", fmt.Errorf("%w: %q", errEntrypointFile, entrypoint)
	}
}
