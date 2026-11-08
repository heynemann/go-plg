package plg

import (
	"context"
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

func (goLoader) multiplexes() bool { return false }

func (goLoader) missingValidate(files *memfs.FS, entrypoint string, val *validation) string {
	return goValidateMissing(files, entrypoint, val.want)
}

func (goLoader) prepare(_ context.Context, _ *Container, reg *registration, src *source, files *memfs.FS) (
	launchSpec, error,
) {
	tree, err := goSourceTree(reg.contr, reg.dispatcher, files, src.desc.Entrypoint)
	if err != nil {
		return launchSpec{}, err
	}

	return launchSpec{payload: reg.goPayload, mounts: []engine.Mount{{FS: tree, GuestPath: goabi.GoPath}}}, nil
}

var (
	errEntrypointPath = errors.New("entrypoint is not a valid path")
	errEntrypointFile = errors.New("entrypoint is neither a directory nor a .go file")
	errVendored       = errors.New("a vendored package cannot take the import path of the contract package or plg's")
)

// goVendor is the directory of a Go plugin's package directory whose packages the plugin imports by their import
// paths, as Go vendors packages.
const goVendor = "vendor"

// renderGo renders the Go dispatcher of the registration's plugins and their init payload for the contract with
// its Host, if any, and its validation, if any. A dispatcher that cannot be rendered (embedded aliases the Host's
// types clash with) fails with [ErrInvalidHost], or [ErrInvalidOption] without a Host: the validation input is a
// named type, which renders as its name.
func (r *registration) renderGo() error {
	dispatcher, err := r.renderDispatcher()
	if err != nil && r.host != nil {
		return fmt.Errorf("%w %s: rendering the Go dispatcher: %w", ErrInvalidHost, r.host.name, err)
	}

	if err != nil {
		return fmt.Errorf("%w: rendering the Go dispatcher: %w", ErrInvalidOption, err)
	}

	payload := goabi.InitPayload{
		GoPath: goabi.GoPath, Dispatch: goabi.DispatchImportPath, Validate: r.validation != nil,
	}
	for _, meth := range r.contr.methods {
		payload.Methods = append(payload.Methods, meth.name)
	}

	if r.host != nil {
		payload.Host, payload.HostPath = r.host.name, r.host.pkgPath
		for _, meth := range r.host.methods {
			payload.HostMethods = append(payload.HostMethods, meth.name)
		}
	}

	encoded, err := payload.Append(nil)
	if err != nil { // the interfaces passed validation, so their names encode
		return fmt.Errorf("%w: encoding the Go init payload: %w", ErrInvalidOption, err)
	}

	r.dispatcher, r.goPayload = dispatcher, encoded

	return nil
}

// renderDispatcher renders the Go dispatcher for the contract with the registration's Host and validation input.
func (r *registration) renderDispatcher() ([]byte, error) {
	var (
		host  *goabi.Interface
		input *goabi.Type
	)

	if r.host != nil {
		host = r.host.described
	}

	if r.validation != nil {
		var err error

		input, err = goabi.TypeOf(r.validation.typ)
		if err != nil {
			return nil, fmt.Errorf("validate: %w", err)
		}
	}

	return goabi.Render(r.contr.described, host, input) //nolint:wrapcheck // renderGo wraps it
}

// goExt is the extension of Go source files.
const goExt = ".go"

// isGoSource reports whether the entry named name is a Go source file a plugin instance receives: a regular .go
// file that is not a test.
func isGoSource(name string, entry fs.DirEntry) bool {
	return entry.Type().IsRegular() && path.Ext(name) == goExt && !strings.HasSuffix(name, "_test.go")
}

// goValidateMissing explains a Go plugin version whose instance has no method want (Validate of the expected
// signature), from the plugin's package source (the directory of its entrypoint): the Validate methods it
// declares, with their positions, or that it declares none. yaegi binds a Validate of another signature as no
// method at all, so the source is the way to show what the plugin has instead.
func goValidateMissing(files fs.FS, entrypoint, want string) string {
	dir, err := goPackageDir(files, entrypoint)
	if err != nil { // the version loaded from this entrypoint
		return "the plugin instance has no method " + want
	}

	var declared []string

	entries, _ := fs.ReadDir(files, dir) //nolint:errcheck // the version loaded from this directory
	fset := token.NewFileSet()

	for _, entry := range entries {
		name := path.Join(dir, entry.Name())
		if isGoSource(name, entry) {
			declared = append(declared, declaredValidates(files, fset, name)...)
		}
	}

	if len(declared) == 0 {
		return "the plugin instance has no method " + want + ", and the plugin declares no method Validate"
	}

	return "the plugin instance has no method " + want + "; the plugin declares " + strings.Join(declared, ", ")
}

// declaredValidates returns the Validate methods the Go source file name declares, each as its signature
// followed by its position; none when the file cannot be read or parsed.
func declaredValidates(files fs.FS, fset *token.FileSet, name string) []string {
	src, err := fs.ReadFile(files, name)
	if err != nil {
		return nil
	}

	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}

	var declared []string

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Name.Name == goabi.ValidateMethod {
			head := string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.Type.End()).Offset])
			declared = append(declared, head+" ("+fset.Position(fn.Pos()).String()+")")
		}
	}

	return declared
}

// goSourceTree returns the tree a Go plugin instance mounts at [goabi.GoPath]: the contract package's source
// at its import path, the plugin's package directory at [goabi.PluginImportPath], the packages of its vendor
// directory at their import paths and the dispatcher at [goabi.DispatchImportPath]. It rejects plugin source
// containing go statements.
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

	err = tree.AddTree(path.Join("src", contr.pkgPath), contr.source, ".")
	if err == nil {
		err = tree.AddTree(path.Join("src", goabi.PluginImportPath), files, dir)
	}

	if err == nil {
		err = addVendored(&tree, contr.pkgPath, files, path.Join(dir, goVendor))
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

// addVendored adds the packages of the directory vendor of files, a Go plugin's vendor directory, to tree at their
// import paths (their paths below vendor) under src; a plugin without one has none. A vendored package may not
// take the contract package's import path, nor one of plg's own (plg.local/...), which fails with errVendored.
func addVendored(tree *memfs.Builder, contract string, files fs.FS, vendor string) error {
	info, err := fs.Stat(files, vendor)
	if errors.Is(err, fs.ErrNotExist) || err == nil && !info.IsDir() {
		return nil
	}

	reserved := path.Dir(goabi.PluginImportPath)

	err = fs.WalkDir(files, vendor, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		pkg := strings.TrimPrefix(path.Dir(name), vendor+"/")
		if pkg == contract || pkg == reserved || strings.HasPrefix(pkg, reserved+"/") {
			return fmt.Errorf("%w: %s", errVendored, name)
		}

		return nil
	})
	if err != nil {
		return err //nolint:wrapcheck // errVendored names the directory; WalkDir's errors name the path
	}

	return tree.AddTree("src", files, vendor) //nolint:wrapcheck // memfs's errors name the files
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
	case info.Mode().IsRegular() && path.Ext(entrypoint) == goExt:
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
		if err != nil || !isGoSource(name, entry) {
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
