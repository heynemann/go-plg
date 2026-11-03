package plg

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"strings"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/memfs"
)

// goMemoryLimit is the default memory limit of each Go plugin instance; yaegi alone takes about 29 MiB.
const goMemoryLimit = 128 << 20

// goLoader loads Go plugins: the generic yaegi guest interprets the plugin's package directory, the contract
// package's source and the rendered dispatcher, mounted as one GOPATH.
type goLoader struct{}

func (goLoader) guestImport() string { return "github.com/heynemann/go-plg/guest/golang" }

func (goLoader) memoryLimit() uint64 { return goMemoryLimit }

func (goLoader) prepare(reg *registration, files fs.FS, entrypoint string) (launchSpec, error) {
	tree, err := goSourceTree(reg.contr, reg.dispatcher, files, entrypoint)
	if err != nil {
		return launchSpec{}, err
	}

	return launchSpec{payload: reg.goPayload, mounts: []engine.Mount{{FS: tree, GuestPath: goabi.GoPath}}}, nil
}

var (
	errEntrypointPath = errors.New("entrypoint is not a valid path")
	errEntrypointFile = errors.New("entrypoint is neither a directory nor a .go file")
)

// goSourceTree returns the tree a Go plugin instance mounts at [goabi.GoPath]: the contract package's source
// at its import path, the plugin's package directory at [goabi.PluginImportPath] and the dispatcher at
// [goabi.DispatchImportPath]. It rejects plugin source containing go statements.
func goSourceTree(contr *contract, dispatcher []byte, files fs.FS, entrypoint string) (*memfs.FS, error) {
	dir, err := goPackageDir(files, entrypoint)
	if err != nil {
		return nil, err
	}

	err = checkNoGoStatements(files, dir)
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

// checkNoGoStatements parses every regular .go file below dir except tests, the source a Go plugin instance
// receives (symbolic links are not copied into it), and fails with [ErrGoStatement] naming the position
// (file:line:column, the file relative to the plugin's fs.FS) of every go statement. A file that does not parse
// fails the load as the guest would.
func checkNoGoStatements(files fs.FS, dir string) error {
	fset := token.NewFileSet()

	var found []string

	err := fs.WalkDir(files, dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() || path.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			return err
		}

		src, err := fs.ReadFile(files, name)
		if err != nil {
			return fmt.Errorf("reading the plugin source: %w", err)
		}

		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("%w: %w", errGuestLoad, err)
		}

		ast.Inspect(file, func(node ast.Node) bool {
			if stmt, ok := node.(*ast.GoStmt); ok {
				found = append(found, fset.Position(stmt.Go).String())
			}

			return true
		})

		return nil
	})
	if err != nil {
		return err //nolint:wrapcheck // the callback's errors are wrapped already; WalkDir adds the path to its own
	}

	if len(found) > 0 {
		return fmt.Errorf("%w: %s", ErrGoStatement, strings.Join(found, ", "))
	}

	return nil
}
