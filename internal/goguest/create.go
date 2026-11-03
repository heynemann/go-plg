package goguest

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/heynemann/go-plg/internal/goabi"
)

var errCreateHost = errors.New("plugin Create must take the Host interface")

// checkHostParam checks, in the plugin package's source, that Create's parameter is the Host interface host
// (as Go source refers to it, "pkg.Name") of package hostPath. yaegi represents every interpreted interface by
// one reflect type and assigns func values across interface types without checking them, so a Create taking
// another interface would load and receive the Host stubs, then fail when it calls a method they lack. The
// source tells them apart: the parameter must be written as the Host's qualified name through the file's
// import of hostPath (under its package name or an explicit one) or, with a dot import, as its bare name.
// Other spellings (a local alias of the type) are rejected. Files that do not parse are left to the
// interpreter, which has loaded the plugin already.
func checkHostParam(goPath, host, hostPath string) error {
	dir := filepath.Join(goPath, "src", filepath.FromSlash(goabi.PluginImportPath))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading plugin source: %w", err)
	}

	pkgName, hostName, _ := strings.Cut(host, ".")

	var seen []string

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || path.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}

		param, err := fileCreateParam(filepath.Join(dir, name), hostPath, pkgName, hostName)
		if err != nil {
			return err
		}

		if param.isHost {
			return nil
		}

		if param.written != "" {
			seen = append(seen, param.written)
		}
	}

	if len(seen) == 0 {
		return nil // no single-parameter Create in parseable source; the interpreter has checked what it loaded
	}

	return fmt.Errorf("%w %s (from %q); it takes %s", errCreateHost, host, hostPath, strings.Join(seen, ", "))
}

// hostParam is the parameter of a source file's func Create.
type hostParam struct {
	written string // its type as written; "" for a file without a single-parameter Create or that does not parse
	isHost  bool   // the type is the Host
}

// fileCreateParam reads one source file and returns the parameter of its func Create.
func fileCreateParam(name, hostPath, pkgName, hostName string) (hostParam, error) {
	src, err := os.ReadFile(name) //nolint:gosec // G304: inside the read-only mount
	if err != nil {
		return hostParam{}, fmt.Errorf("reading plugin source: %w", err)
	}

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return hostParam{}, nil //nolint:nilerr // left to the interpreter, see checkHostParam
	}

	param, found := createParam(file)
	if !found {
		return hostParam{}, nil
	}

	return hostParam{
		written: string(src[fset.Position(param.Pos()).Offset:fset.Position(param.End()).Offset]),
		isHost:  namesHost(file, param, hostPath, pkgName, hostName),
	}, nil
}

// createParam returns the type of the parameter of the file's func Create, if it has exactly one.
func createParam(file *ast.File) (ast.Expr, bool) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != goabi.CreateFunc {
			continue
		}

		params := fn.Type.Params.List
		if len(params) == 1 && len(params[0].Names) <= 1 {
			return params[0].Type, true
		}
	}

	return nil, false
}

// namesHost reports whether expr refers to the type hostName of package hostPath, named pkgName, in file.
func namesHost(file *ast.File, expr ast.Expr, hostPath, pkgName, hostName string) bool {
	switch typ := expr.(type) {
	case *ast.SelectorExpr:
		pkg, ok := typ.X.(*ast.Ident)

		return ok && typ.Sel.Name == hostName && importedAs(file, hostPath, pkgName) == pkg.Name
	case *ast.Ident:
		return typ.Name == hostName && importedAs(file, hostPath, pkgName) == "."
	default:
		return false
	}
}

// importedAs returns the name file imports hostPath under ("." for a dot import): its explicit name, or the
// package's name pkgName. It returns "" when file does not import hostPath.
func importedAs(file *ast.File, hostPath, pkgName string) string {
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil || imported != hostPath {
			continue
		}

		if spec.Name != nil {
			return spec.Name.Name
		}

		return pkgName
	}

	return ""
}
