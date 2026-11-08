package contractload

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path"
	"strings"

	"github.com/heynemann/go-plg/internal/goabi"
)

var (
	// ErrGoTypes reports a Go plugin whose source does not type-check against the contract.
	ErrGoTypes = errors.New("the plugin does not type-check against the contract")
	// ErrGoUnchecked reports a Go plugin whose imports could not all be type-checked: only the contract package
	// and the standard library can.
	ErrGoUnchecked = errors.New("the plugin was not type-checked")
)

// CheckGo type-checks the Go plugin package in the directory dir of files (its regular .go files but tests, as
// the Go guest receives them) against the contract package, as the Go compiler would: a plugin whose types do not
// implement the interface (a method of another signature, a missing one) fails with [ErrGoTypes], naming each
// error's file:line:column; the Go interpreter accepts such a plugin and fails only once the method is called. A
// plugin importing a package other than the contract and the standard library is left unchecked
// ([ErrGoUnchecked]).
func (l *Loaded) CheckGo(files fs.FS, dir string) error {
	sources, err := parseGo(l.fset, files, dir)
	if err != nil {
		return err
	}

	var (
		failures   []string
		unresolved string
	)

	config := types.Config{
		Importer: importerFunc(func(imp string) (*types.Package, error) {
			if imp == l.pkg.Path() {
				return l.pkg, nil
			}

			pkg, importErr := l.std.Import(imp)
			if importErr != nil && unresolved == "" {
				unresolved = imp
			}

			return pkg, importErr //nolint:wrapcheck // go/types reports it, with its position
		}),
		Error: func(err error) { failures = append(failures, err.Error()) },
	}

	_, err = config.Check(goabi.PluginImportPath, l.fset, sources, nil)
	if err != nil && len(failures) == 0 { // Error has the errors, the first among them
		failures = append(failures, err.Error())
	}

	switch {
	case unresolved != "":
		return fmt.Errorf("%w: it imports %s, and only the contract package and the standard library are "+
			"type-checked", ErrGoUnchecked, unresolved)
	case len(failures) > 0:
		return fmt.Errorf("%w: %s", ErrGoTypes, strings.Join(failures, "; "))
	default:
		return nil
	}
}

// parseGo parses the regular .go files of dir in files, tests left out, naming them by their path in files.
func parseGo(fset *token.FileSet, files fs.FS, dir string) ([]*ast.File, error) {
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return nil, fmt.Errorf("reading the plugin: %w", err)
	}

	var sources []*ast.File

	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || path.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}

		src, err := fs.ReadFile(files, path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("reading the plugin: %w", err)
		}

		file, err := parser.ParseFile(fset, path.Join(dir, name), src, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrGoTypes, err)
		}

		sources = append(sources, file)
	}

	return sources, nil
}

// importerFunc is a types.Importer.
type importerFunc func(imp string) (*types.Package, error)

func (f importerFunc) Import(imp string) (*types.Package, error) {
	return f(imp)
}
