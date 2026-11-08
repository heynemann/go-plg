// Package contractload describes a contract package from its source, as `plg validate` needs it to load a plugin
// without the service that compiles the contract in: the package is type-checked as plg gen type-checks it, the
// interface and its Host are described for the Go dispatcher from go/types (the same description reflection gives
// a registered contract, see [goabi.Interface]), and their method types are the types the Go interpreter gives the
// contract's source, which plg plans the script guests' schema from (as the Go guest plans its codecs).
package contractload

import (
	"errors"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"reflect"
	"slices"
	"strconv"

	"github.com/traefik/yaegi/interp"

	"github.com/heynemann/go-plg/guestsrc/golang/symbols"
	"github.com/heynemann/go-plg/internal/contractsrc"
	"github.com/heynemann/go-plg/internal/gen"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/goguest"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/offline"
)

var (
	// ErrInterpret reports a contract package the Go interpreter cannot load.
	ErrInterpret = errors.New("contractload: the Go interpreter cannot load the contract")

	errPanicked = errors.New("the interpreter panicked")
)

// Loaded is a contract package plg validate loaded.
type Loaded struct {
	// Contract is the contract as plg loads plugins against it. It has no Module: the caller resolved it.
	Contract *offline.Contract
	pkg      *types.Package // the package, type-checked
	fset     *token.FileSet // the positions of the standard library packages std imported
	std      types.Importer // the standard library packages the package imports, shared with Go plugins' checks
}

// iface is an interface the contract package declares.
type iface struct {
	named     *types.Named
	methods   *types.Interface // named's underlying interface
	described *goabi.Interface
}

// Load describes the interface name of the contract package at import path pkgPath, whose source is the
// directory dir, and its Host interface host (none when empty). It fails when the package does not type-check
// (or imports what the Go guest does not allow, see [gen.Load]), when name or host is not an interface plg gen
// could generate for ([gen.Interface]), and when a type cannot be described ([goabi.ErrRender]) or interpreted
// ([ErrInterpret]).
func Load(dir, pkgPath, name, host string) (*Loaded, error) {
	fset := token.NewFileSet()
	std := importer.ForCompiler(fset, "gc", nil)

	pkg, err := gen.Load(dir, pkgPath, std)
	if err != nil {
		return nil, err //nolint:wrapcheck // gen's errors name the package and the problem
	}

	implemented, err := lookup(pkg, name)
	if err != nil {
		return nil, err
	}

	source, err := rootSource(dir)
	if err != nil {
		return nil, err
	}

	contract := &offline.Contract{Interface: implemented.described, Source: source}
	interpreter := newInterpreter(source, pkgPath)

	contract.Methods, err = interpreter.methods(implemented)
	if err != nil {
		return nil, err
	}

	if host != "" {
		contract.Host, err = loadHost(pkg, interpreter, host)
		if err != nil {
			return nil, fmt.Errorf("host: %w", err)
		}
	}

	return &Loaded{Contract: contract, pkg: pkg, fset: fset, std: std}, nil
}

// loadHost describes the Host interface name of pkg.
func loadHost(pkg *types.Package, interpreter *interpreter, name string) (*offline.Host, error) {
	host, err := lookup(pkg, name)
	if err != nil {
		return nil, err
	}

	methods, err := interpreter.methods(host)
	if err != nil {
		return nil, err
	}

	return &offline.Host{Interface: host.described, Methods: methods}, nil
}

// rootSource reads the regular files at the root of dir, the package's own files: subdirectories are other
// packages. The files plg gen generated and tests are left out, as the guest leaves them out.
func rootSource(dir string) (*memfs.FS, error) {
	pkgDir := os.DirFS(dir)

	entries, err := fs.ReadDir(pkgDir, ".")
	if err != nil {
		return nil, fmt.Errorf("reading the contract package: %w", err)
	}

	var tree memfs.Builder

	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}

		var data []byte

		data, err = fs.ReadFile(pkgDir, entry.Name())
		if err == nil && contractsrc.Include(entry.Name(), data) {
			err = tree.Add(entry.Name(), data)
		}

		if err != nil {
			return nil, fmt.Errorf("reading the contract package: %w", err)
		}
	}

	files, err := tree.Build()
	if err != nil {
		return nil, fmt.Errorf("reading the contract package: %w", err)
	}

	return files, nil
}

// lookup returns the interface pkg declares as name, described. An alias of an interface is refused: plugins name
// the interface they implement by its own name.
func lookup(pkg *types.Package, name string) (*iface, error) {
	named, methods, err := gen.Interface(pkg, name)
	if err != nil {
		return nil, err //nolint:wrapcheck // gen's errors name the package and the interface
	}

	if obj, ok := pkg.Scope().Lookup(name).(*types.TypeName); ok && obj.IsAlias() {
		return nil, fmt.Errorf("%w: %s is an alias of %s: name the interface itself", gen.ErrInterface, name,
			types.TypeString(named, nil))
	}

	described, err := describeInterface(named, methods)
	if err != nil {
		return nil, err
	}

	return &iface{named: named, methods: methods, described: described}, nil
}

// describeInterface describes a named interface, as [goabi.InterfaceOf] describes one from reflection.
func describeInterface(named *types.Named, methods *types.Interface) (*goabi.Interface, error) {
	described := &goabi.Interface{
		Type:    describeNamed(named.Obj()),
		Methods: make([]goabi.Method, 0, methods.NumMethods()),
	}

	for method := range methods.Methods() {
		if !method.Exported() {
			return nil, fmt.Errorf("%w: method %s is unexported", goabi.ErrRender, method.Name())
		}

		fn, err := describeFunc(method.Signature())
		if err != nil {
			return nil, fmt.Errorf("method %s: %w", method.Name(), err)
		}

		described.Methods = append(described.Methods, goabi.Method{Name: method.Name(), Func: *fn})
	}

	return described, nil
}

func describeFunc(sig *types.Signature) (*goabi.Func, error) {
	described := &goabi.Func{Variadic: sig.Variadic()}

	for param := range sig.Params().Variables() {
		typ, err := describe(param.Type())
		if err != nil {
			return nil, err
		}

		described.Params = append(described.Params, typ)
	}

	for result := range sig.Results().Variables() {
		typ, err := describe(result.Type())
		if err != nil {
			return nil, err
		}

		described.Results = append(described.Results, typ)
	}

	return described, nil
}

// describe describes typ as [goabi.TypeOf] describes a reflect type: aliases resolved, predeclared types by the
// names reflection gives them (uint8 for byte), and the same types rejected with the same messages.
func describe(typ types.Type) (*goabi.Type, error) {
	switch typ := types.Unalias(typ).(type) {
	case *types.Basic:
		if typ.Kind() == types.UnsafePointer {
			return nil, fmt.Errorf("%w: unsafe.Pointer", goabi.ErrRender)
		}

		return &goabi.Type{Kind: goabi.KindNamed, Name: types.Typ[typ.Kind()].Name()}, nil
	case *types.Named:
		if typ.TypeArgs().Len() > 0 || typ.TypeParams().Len() > 0 {
			return nil, fmt.Errorf("%w: generic type %s", goabi.ErrRender, typ)
		}

		return describeNamed(typ.Obj()), nil
	case *types.Struct:
		return describeStruct(typ)
	default:
		return describeComposite(typ)
	}
}

// describeComposite describes an unnamed pointer, slice, array or map.
func describeComposite(typ types.Type) (*goabi.Type, error) {
	var (
		described *goabi.Type
		err       error
	)

	switch typ := typ.(type) {
	case *types.Pointer:
		described, err = describeElem(goabi.KindPointer, typ.Elem())
	case *types.Slice:
		described, err = describeElem(goabi.KindSlice, typ.Elem())
	case *types.Array:
		described, err = describeElem(goabi.KindArray, typ.Elem())
		if err == nil {
			described.Len = int(typ.Len())
		}
	case *types.Map:
		described, err = describeElem(goabi.KindMap, typ.Elem())
		if err == nil {
			described.Key, err = describe(typ.Key())
		}
	default:
		return nil, fmt.Errorf("%w: unnamed %s type %s", goabi.ErrRender, kindOf(typ), typ)
	}

	if err != nil {
		return nil, err
	}

	return described, nil
}

// describeElem describes a pointer, slice, array or map of kind with the element elem.
func describeElem(kind goabi.Kind, elem types.Type) (*goabi.Type, error) {
	described, err := describe(elem)
	if err != nil {
		return nil, err
	}

	return &goabi.Type{Kind: kind, Elem: described}, nil
}

func describeStruct(typ *types.Struct) (*goabi.Type, error) {
	described := &goabi.Type{Kind: goabi.KindStruct, Fields: make([]goabi.Field, 0, typ.NumFields())}

	for i := range typ.NumFields() {
		field := typ.Field(i)
		if !field.Exported() {
			return nil, fmt.Errorf("%w: unexported field %s in %s", goabi.ErrRender, field.Name(), typ)
		}

		fieldType, err := describe(field.Type())
		if err != nil {
			return nil, err
		}

		described.Fields = append(described.Fields, goabi.Field{
			Type: fieldType, Name: field.Name(), Tag: typ.Tag(i), Embedded: field.Embedded(),
		})
	}

	return described, nil
}

// describeNamed describes the named type obj.
func describeNamed(obj *types.TypeName) *goabi.Type {
	described := &goabi.Type{Kind: goabi.KindNamed, Name: obj.Name()}
	if pkg := obj.Pkg(); pkg != nil {
		described.Path, described.Pkg = pkg.Path(), pkg.Name()
	}

	return described
}

// kindOf names the kind of an unnamed type the dispatcher cannot write, as reflect.Kind names it.
func kindOf(typ types.Type) string {
	switch typ.(type) {
	case *types.Signature:
		return "func"
	case *types.Chan:
		return "chan"
	case *types.Interface:
		return "interface"
	default:
		return "unsupported"
	}
}

// interpreter interprets the contract package with yaegi, linked with the Go guest's symbol tables, so the
// contract's types are the reflect types the Go guest gives them.
type interpreter struct {
	yaegi   *interp.Interpreter
	err     error                     // why the interpreter could not start
	aliases map[*types.Package]string // the alias of each package a method's types name
	pkgPath string                    // the contract package's import path
	imports []string                  // the import paths imported so far
}

// newInterpreter returns an interpreter of the contract package at pkgPath, whose source is source.
func newInterpreter(source fs.FS, pkgPath string) *interpreter {
	interpreter := &interpreter{aliases: map[*types.Package]string{}, pkgPath: pkgPath}

	goPath, err := goPathOf(source, pkgPath)
	if err != nil {
		interpreter.err = fmt.Errorf("%w: %w", ErrInterpret, err)

		return interpreter
	}

	interpreter.yaegi = interp.New(interp.Options{GoPath: ".", SourcecodeFilesystem: goPath})

	err = interpreter.yaegi.Use(symbols.Symbols)
	if err != nil {
		interpreter.err = fmt.Errorf("%w: loading the standard library: %w", ErrInterpret, err)
	}

	return interpreter
}

// goPathOf returns a GOPATH holding source as the package pkgPath, its Go files rewritten as the Go guest
// rewrites them (see [goguest.NameParams]).
func goPathOf(source fs.FS, pkgPath string) (*memfs.FS, error) {
	var tree memfs.Builder

	err := fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		data, err := fs.ReadFile(source, name)
		if err != nil {
			return err //nolint:wrapcheck // wrapped by newInterpreter
		}

		if path.Ext(name) == ".go" {
			data = goguest.NameParams(data)
		}

		return tree.Add(path.Join("src", pkgPath, name), data)
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by newInterpreter
	}

	return tree.Build() //nolint:wrapcheck // wrapped by newInterpreter
}

// methods returns the methods of the interface with the types the interpreter gives them, in method index order
// (go/types and reflection both sort an interface's methods by name). An unexported method keeps its package path,
// so plg reports it as unexported.
func (in *interpreter) methods(declared *iface) ([]reflect.Method, error) {
	if in.err != nil {
		return nil, in.err
	}

	name := declared.named.Obj().Name()
	methods := make([]reflect.Method, 0, declared.methods.NumMethods())

	for method := range declared.methods.Methods() {
		fn, err := in.funcType(name+"_"+method.Name(), method.Signature())
		if err != nil {
			return nil, fmt.Errorf("%w: method %s.%s: %w", ErrInterpret, name, method.Name(), err)
		}

		described := reflect.Method{Name: method.Name(), Type: fn}
		if !method.Exported() {
			described.PkgPath = in.pkgPath
		}

		methods = append(methods, described)
	}

	return methods, nil
}

// funcType returns the type the interpreter gives sig, declaring a variable of that type named after id. yaegi
// panics on some types it cannot build (a slice of itself): the panic is the error.
func (in *interpreter) funcType(id string, sig *types.Signature) (_ reflect.Type, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("%w: %v", errPanicked, v)
		}
	}()

	expr := types.TypeString(sig, in.qualifier)

	for _, imp := range in.pending() {
		_, err = in.yaegi.Eval("import " + in.aliasOf(imp) + " " + strconv.Quote(imp))
		if err != nil {
			return nil, err //nolint:wrapcheck // wrapped by methods
		}
	}

	name := "plgType_" + id

	_, err = in.yaegi.Eval("var " + name + " " + expr)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by methods
	}

	fn, err := in.yaegi.Eval(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by methods
	}

	return fn.Type(), nil
}

// qualifier writes every package by its alias.
func (in *interpreter) qualifier(pkg *types.Package) string {
	if _, ok := in.aliases[pkg]; !ok {
		in.aliases[pkg] = "plgpkg" + strconv.Itoa(len(in.aliases))
	}

	return in.aliases[pkg]
}

// pending returns the import paths the qualifier named that are not imported yet, and records them as imported.
func (in *interpreter) pending() []string {
	var paths []string

	for pkg := range in.aliases {
		if !slices.Contains(in.imports, pkg.Path()) {
			paths = append(paths, pkg.Path())
			in.imports = append(in.imports, pkg.Path())
		}
	}

	slices.Sort(paths)

	return paths
}

// aliasOf returns the alias of the package with the import path imp.
func (in *interpreter) aliasOf(imp string) string {
	for pkg, alias := range in.aliases {
		if pkg.Path() == imp {
			return alias
		}
	}

	return ""
}
