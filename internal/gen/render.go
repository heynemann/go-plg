package gen

import (
	"bytes"
	"cmp"
	"fmt"
	"go/format"
	"go/types"
	"iter"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/heynemann/go-plg/internal/contractsrc"
)

// namer hands out the generated file's package-level and import names, avoiding every name the package
// declares, so the file compiles whatever names the package uses.
type namer struct {
	pkg     *types.Package
	taken   map[string]bool
	imports map[string]string // import path → local name
}

func newNamer(pkg *types.Package) *namer {
	n := &namer{pkg: pkg, taken: map[string]bool{}, imports: map[string]string{}}
	for _, name := range pkg.Scope().Names() {
		n.taken[name] = true
	}

	return n
}

// unique returns base, or base with the smallest numeric suffix from 2 that is still free, and takes it.
func (n *namer) unique(base string) string {
	name := base
	for i := 2; n.taken[name]; i++ {
		name = base + strconv.Itoa(i)
	}

	n.taken[name] = true

	return name
}

// importAs returns the local name of the import of importPath, a package named name, importing it if needed.
func (n *namer) importAs(importPath, name string) string {
	local, ok := n.imports[importPath]
	if !ok {
		local = n.unique(name)
		n.imports[importPath] = local
	}

	return local
}

// predeclaredType returns how the generated file names the predeclared type name: by its name, unless the
// package redeclares it, then as an empty interface literal (any) or through plgimpl's alias, whose local
// name is impl.
func (n *namer) predeclaredType(name, impl string) string {
	switch {
	case !n.taken[name]:
		return name
	case name == "any":
		return "interface{}"
	default:
		return impl + "." + strings.ToUpper(name[:1]) + name[1:]
	}
}

// typeString returns how the generated file names typ: as types.TypeString does, except that every
// predeclared type the package redeclares is named through predeclaredType, so that the field types of a
// struct from another package (a unicode.RangeTable's int LatinOffset, in a package declaring int) still
// denote the predeclared types.
func (n *namer) typeString(typ types.Type, impl string) string {
	if _, shadowed := shadowedIn(n.pkg, typ); !shadowed {
		return types.TypeString(typ, n.qualify)
	}

	switch concrete := typ.(type) {
	case *types.Basic:
		return n.predeclaredType(concrete.Name(), impl)
	case *types.Pointer:
		return "*" + n.typeString(concrete.Elem(), impl)
	case *types.Slice:
		return "[]" + n.typeString(concrete.Elem(), impl)
	case *types.Map:
		return "map[" + n.typeString(concrete.Key(), impl) + "]" + n.typeString(concrete.Elem(), impl)
	case *types.Struct:
		return n.structString(concrete, impl)
	default: // any and error, the only named types without a package
		return n.predeclaredType(types.TypeString(typ, nil), impl)
	}
}

// structString returns how the generated file names the struct literal typ (see typeString).
func (n *namer) structString(typ *types.Struct, impl string) string {
	var text strings.Builder

	text.WriteString("struct{")

	for i := range typ.NumFields() {
		if i > 0 {
			text.WriteString("; ")
		}

		field := typ.Field(i)
		if !field.Embedded() {
			text.WriteString(field.Name() + " ")
		}

		text.WriteString(n.typeString(field.Type(), impl))

		if tag := typ.Tag(i); tag != "" {
			text.WriteString(" " + strconv.Quote(tag))
		}
	}

	text.WriteString("}")

	return text.String()
}

// checkUnshadowed fails when the package redeclares one of the predeclared identifiers names, which the
// generated file uses and cannot name otherwise.
func (n *namer) checkUnshadowed(names []string) error {
	var shadowed []string

	for _, name := range names {
		if n.pkg.Scope().Lookup(name) != nil {
			shadowed = append(shadowed, name)
		}
	}

	if len(shadowed) > 0 {
		return fmt.Errorf("%w: package %s declares %s", ErrPredeclared, n.pkg.Name(), strings.Join(shadowed, ", "))
	}

	return nil
}

// checkSignatures fails when a method of ifaces mentions a predeclared type the package redeclares, such as
// the int of an embedded io.Reader's Read in a package declaring int: the generated file, in the package,
// could not name it.
func checkSignatures(pkg *types.Package, names []string, ifaces []*types.Interface) error {
	for i, iface := range ifaces {
		for method := range iface.Methods() {
			if name, ok := shadowedIn(pkg, method.Type()); ok {
				return fmt.Errorf("%w: %s.%s mentions the predeclared %s, which package %s redeclares",
					ErrPredeclared, names[i], method.Name(), name, pkg.Name())
			}
		}
	}

	return nil
}

// shadowedIn returns the name of a predeclared type that the type string of typ mentions and pkg redeclares.
func shadowedIn(pkg *types.Package, typ types.Type) (string, bool) {
	var obj types.Object

	switch concrete := typ.(type) {
	case *types.Basic:
		return concrete.Name(), pkg.Scope().Lookup(concrete.Name()) != nil
	case *types.Alias:
		obj = concrete.Obj()
	case *types.Named:
		obj = concrete.Obj()
	default:
	}

	if obj != nil && obj.Pkg() == nil { // any or error
		return obj.Name(), pkg.Scope().Lookup(obj.Name()) != nil
	}

	for _, part := range typeParts(typ) {
		if name, ok := shadowedIn(pkg, part); ok {
			return name, ok
		}
	}

	return "", false
}

// typeParts returns the types the type string of t mentions, other than t itself.
func typeParts(t types.Type) []types.Type {
	var parts []types.Type

	switch typ := t.(type) {
	case *types.Named:
		parts = slices.Collect(typ.TypeArgs().Types())
	case *types.Map:
		parts = []types.Type{typ.Key(), typ.Elem()}
	case interface{ Elem() types.Type }: // pointers, slices, arrays and channels
		parts = []types.Type{typ.Elem()}
	case *types.Signature:
		parts = append(varTypes(typ.Params().Variables()), varTypes(typ.Results().Variables())...)
	case *types.Struct:
		parts = varTypes(typ.Fields())
	case *types.Interface:
		for method := range typ.ExplicitMethods() {
			parts = append(parts, method.Type())
		}

		parts = slices.AppendSeq(parts, typ.EmbeddedTypes())
	default: // basic types, aliases, type parameters and unions: the contract's interfaces are not generic
	}

	return parts
}

// varTypes returns the types of vars.
func varTypes(vars iter.Seq[*types.Var]) []types.Type {
	var typs []types.Type

	for v := range vars {
		typs = append(typs, v.Type())
	}

	return typs
}

// qualify is the generated file's types.Qualifier: the package's own types are unqualified, every other
// package is imported.
func (n *namer) qualify(pkg *types.Package) string {
	if pkg == n.pkg {
		return ""
	}

	return n.importAs(pkg.Path(), pkg.Name())
}

// render returns the generated file for the interfaces ifaces, declared as names in pkg, with their fast paths
// unless opts.Generic.
func render(pkg *types.Package, names []string, ifaces []*types.Interface, opts Options) ([]byte, error) {
	err := checkSignatures(pkg, names, ifaces)
	if err == nil {
		err = newNamer(pkg).checkUnshadowed([]string{predeclaredNil}) // every adapter asserts (*adapter)(nil)
	}

	if err != nil {
		return nil, err
	}

	n := newNamer(pkg)
	embedPkg := n.importAs("embed", "embed")
	reflectPkg := n.importAs("reflect", "reflect")
	plgPkg := n.importAs(plgImportPath, "plg")
	source := n.unique("plg" + names[0] + "Source")

	adapters := make([]string, len(names))
	for i, name := range names {
		adapters[i] = n.unique("plg" + name)
	}

	var body bytes.Buffer

	fmt.Fprintf(&body, "// %s is the package's Go source, which the Go guest interprets. plg leaves out the test\n"+
		"// files and the files plg gen generated.\n//\n//go:embed *.go\nvar %[1]s %s.FS\n", source, embedPkg)

	for i, name := range names {
		renderAdapter(&body, n, name, adapters[i], ifaces[i])
	}

	registrations := make([]string, len(names))
	if !opts.Generic {
		registrations, err = renderFastPaths(&body, n, names, ifaces)
		if err != nil {
			return nil, err
		}
	}

	body.WriteString("\nfunc init() {\n")

	for i, name := range names {
		fmt.Fprintf(&body, "\t%[1]s.MustRegisterContract(%[1]s.Contract{\n"+
			"\t\tInterface: %[2]s.TypeFor[%[3]s](),\n\t\tAdapter: %[2]s.TypeFor[%[4]s](),\n\t\tSource: %[5]s,\n%[6]s\t})\n",
			plgPkg, reflectPkg, name, adapters[i], source, registrations[i])
	}

	body.WriteString("}\n")

	var file bytes.Buffer

	file.WriteString(contractsrc.Header + "\n\npackage " + pkg.Name() + "\n\n")
	renderImports(&file, n.imports)
	file.Write(body.Bytes())

	formatted, err := format.Source(file.Bytes())
	if err != nil { // unreachable: every piece above is valid Go, and type strings come from go/types
		return nil, fmt.Errorf("plg gen rendered invalid Go: %w", err)
	}

	return formatted, nil
}

// renderAdapter renders the adapter type of the interface iface, declared as name: one W field per method, of
// the method's type, the methods forwarding to them and the assertion that it implements the interface.
func renderAdapter(body *bytes.Buffer, n *namer, name, adapter string, iface *types.Interface) {
	fmt.Fprintf(body, "\n// %s implements %s for plg, which fills each W field with a call into a plugin.\n"+
		"type %[1]s struct {\n", adapter, name)

	for method := range iface.Methods() {
		fmt.Fprintf(body, "\tW%s func(%s)%s\n", method.Name(),
			strings.Join(paramTypes(method.Signature(), n.qualify), ", "), results(method.Signature(), n.qualify))
	}

	fmt.Fprintf(body, "}\n\nvar _ %s = (*%s)(nil)\n", name, adapter)

	for method := range iface.Methods() {
		sig := method.Signature()

		params := paramTypes(sig, n.qualify)
		for i, typ := range params {
			params[i] = paramName(i) + " " + typ
		}

		fmt.Fprintf(body, "\nfunc (a *%s) %s(%s)%s {\n\t", adapter, method.Name(), strings.Join(params, ", "),
			results(sig, n.qualify))

		if sig.Results().Len() > 0 {
			body.WriteString("return ")
		}

		fmt.Fprintf(body, "a.W%s(%s)\n}\n", method.Name(), args(sig))
	}
}

// paramTypes renders the types of sig's parameters.
func paramTypes(sig *types.Signature, qualify types.Qualifier) []string {
	list := make([]string, sig.Params().Len())
	for i := range list {
		list[i] = types.TypeString(sig.Params().At(i).Type(), qualify)
	}

	if sig.Variadic() {
		last := len(list) - 1
		list[last] = "..." + strings.TrimPrefix(list[last], "[]") // a variadic parameter's type is its slice
	}

	return list
}

// paramName is the name of a forwarding method's parameter i: parameter names in the interface may clash with
// the generated file's imports, these cannot.
func paramName(i int) string {
	return "p" + strconv.Itoa(i)
}

// args renders the arguments forwarding sig's parameters.
func args(sig *types.Signature) string {
	list := make([]string, sig.Params().Len())
	for i := range list {
		list[i] = paramName(i)
	}

	if sig.Variadic() {
		list[len(list)-1] += "..."
	}

	return strings.Join(list, ", ")
}

// results renders sig's result list, with its leading space.
func results(sig *types.Signature, qualify types.Qualifier) string {
	list := make([]string, sig.Results().Len())
	for i := range list {
		list[i] = types.TypeString(sig.Results().At(i).Type(), qualify)
	}

	switch len(list) {
	case 0:
		return ""
	case 1:
		return " " + list[0]
	default:
		return " (" + strings.Join(list, ", ") + ")"
	}
}

// renderImports renders the import declaration: standard library first, then the others, each group sorted by
// path. An import is named when its local name is not its path's last element.
func renderImports(file *bytes.Buffer, imports map[string]string) {
	paths := slices.SortedFunc(maps.Keys(imports), func(a, b string) int {
		return cmp.Or(cmp.Compare(isThirdParty(a), isThirdParty(b)), strings.Compare(a, b))
	})

	file.WriteString("import (\n")

	for i, importPath := range paths {
		if i > 0 && isThirdParty(importPath) != isThirdParty(paths[i-1]) {
			file.WriteString("\n")
		}

		file.WriteString("\t")

		if local := imports[importPath]; local != path.Base(importPath) {
			file.WriteString(local + " ")
		}

		file.WriteString(strconv.Quote(importPath) + "\n")
	}

	file.WriteString(")\n\n")
}

// isThirdParty reports, as 0 or 1, whether importPath is outside the standard library (its first element has
// a dot).
func isThirdParty(importPath string) int {
	first, _, _ := strings.Cut(importPath, "/")
	if strings.Contains(first, ".") {
		return 1
	}

	return 0
}
