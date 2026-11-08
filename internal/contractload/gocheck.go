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
	// ErrGoUnchecked reports a Go plugin whose imports could not all be type-checked: only the contract package,
	// the plugin's vendored packages and the standard library can.
	ErrGoUnchecked = errors.New("the plugin was not type-checked")

	errImportCycle = errors.New("import cycle")
)

// CheckGo type-checks the Go plugin package in the directory dir of files (its regular .go files but tests, as
// the Go guest receives them) against the contract package, as the Go compiler would: a plugin whose types do not
// implement the interface (a method of another signature, a missing one) fails with [ErrGoTypes], naming each
// error's file:line:column; the Go interpreter accepts such a plugin and fails only once the method is called. The
// packages of dir's vendor directory, which the guest mounts at their import paths, are checked as the plugin
// imports them. A plugin importing a package other than the contract, a vendored one and the standard library is
// left unchecked ([ErrGoUnchecked]).
func (l *Loaded) CheckGo(files fs.FS, dir string) error {
	sources, err := parseGo(l.fset, files, dir)
	if err != nil {
		return err
	}

	check := &goCheck{loaded: l, files: files, vendor: path.Join(dir, "vendor"), vendored: map[string]*types.Package{}}

	_, err = check.config().Check(goabi.PluginImportPath, l.fset, sources, nil)
	if err != nil && len(check.failures) == 0 { // Error has the errors, the first among them
		check.failures = append(check.failures, err.Error())
	}

	switch {
	case check.unresolved != "":
		return fmt.Errorf("%w: it imports %s, and only the contract package, vendored packages and the standard "+
			"library are type-checked", ErrGoUnchecked, check.unresolved)
	case len(check.failures) > 0:
		return fmt.Errorf("%w: %s", ErrGoTypes, strings.Join(check.failures, "; "))
	default:
		return nil
	}
}

// goCheck is one CheckGo: the errors so far, the first import it could not resolve, and the vendored packages
// checked (nil while one is being checked).
type goCheck struct {
	loaded     *Loaded
	files      fs.FS
	vendored   map[string]*types.Package
	vendor     string
	unresolved string
	failures   []string
}

// config is the configuration type-checking the plugin and its vendored packages.
func (c *goCheck) config() *types.Config {
	return &types.Config{
		Importer: importerFunc(c.importPackage),
		Error:    func(err error) { c.failures = append(c.failures, err.Error()) },
	}
}

// importPackage resolves the import imp: the contract package, a vendored package (type-checked once), or a
// standard library package.
func (c *goCheck) importPackage(imp string) (*types.Package, error) {
	if imp == c.loaded.pkg.Path() {
		return c.loaded.pkg, nil
	}

	pkg, err := c.importVendored(imp)
	if pkg != nil || err != nil {
		return pkg, err
	}

	pkg, err = c.loaded.std.Import(imp)
	if err != nil && c.unresolved == "" {
		c.unresolved = imp
	}

	return pkg, err //nolint:wrapcheck // go/types reports it, with its position
}

// importVendored type-checks the vendored package imp, once; it returns nil without an error when the plugin
// vendors no such package.
func (c *goCheck) importVendored(imp string) (*types.Package, error) {
	if pkg, seen := c.vendored[imp]; seen {
		if pkg == nil {
			return nil, fmt.Errorf("%w: %s", errImportCycle, imp)
		}

		return pkg, nil
	}

	dir := path.Join(c.vendor, imp)
	if !isDir(c.files, dir) {
		return nil, nil //nolint:nilnil // not vendored: the standard library may have it
	}

	sources, err := parseGo(c.loaded.fset, c.files, dir)
	if err != nil {
		return nil, err
	}

	c.vendored[imp] = nil

	// The configuration's Error collects every error, with its position; Check returns the package regardless.
	pkg, _ := c.config().Check(imp, c.loaded.fset, sources, nil) //nolint:errcheck // as above

	c.vendored[imp] = pkg

	return pkg, nil
}

// isDir reports whether name is a directory of files.
func isDir(files fs.FS, name string) bool {
	info, err := fs.Stat(files, name)

	return err == nil && info.IsDir()
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
