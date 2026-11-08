package typings

import (
	"cmp"
	"fmt"
	"go/types"
	"slices"
	"strconv"

	"github.com/heynemann/go-plg/internal/gen"
	"github.com/heynemann/go-plg/internal/layout"
)

type node = layout.Node[types.Type]

// model is a contract planned as the wire codec plans it, with the names the declarations give its types.
type model struct {
	contract   *Contract
	iface      *ifaceModel // the implemented interface
	host       *ifaceModel // nil without a Host
	validation *node       // the validation input, nil without one
	visited    map[*node]bool
	nodes      map[*types.TypeName]*node // the planned type of each declared named type
	spaces     map[string]string         // namespace by package path
	decls      []*types.TypeName         // the named types the methods reach (time.Time aside), in declaration order
}

// ifaceModel is an interface of the contract and its methods.
type ifaceModel struct {
	obj     *types.TypeName
	methods []method
}

// method is a method as the wire encodes it: the parameters after the context, the results before the error.
type method struct {
	name    string
	params  []param
	results []*node
}

// param is a method's parameter: its name in the Go source ("" or "_" when it has none) and its type.
type param struct {
	node *node
	name string
}

// newModel plans the contract's interface, Host and validation input.
func newModel(contract *Contract) (*model, error) {
	planned := &model{
		contract: contract, visited: map[*node]bool{}, nodes: map[*types.TypeName]*node{},
		spaces: map[string]string{},
	}

	iface, ifaceSigs, err := signatures(contract.Interface)
	if err != nil {
		return nil, err
	}

	sigs := ifaceSigs

	var (
		host     *types.Interface
		hostSigs []layout.Signature[types.Type]
	)

	if contract.Host != nil {
		host, hostSigs, err = signatures(contract.Host)
		if err != nil {
			return nil, err
		}

		sigs = append(slices.Clip(sigs), hostSigs...)
	}

	if contract.Validation != nil {
		sigs = append(slices.Clip(sigs), layout.Signature[types.Type]{
			Name: "Validate", Params: []types.Type{contract.Validation},
		})
	}

	plan, err := gen.Plan(sigs)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrContract, err)
	}

	methods := plan.Methods
	planned.iface = describeInterface(contract.Interface.Obj(), iface, methods[:len(ifaceSigs)])
	methods = methods[len(ifaceSigs):]

	if contract.Host != nil {
		planned.host = describeInterface(contract.Host.Obj(), host, methods[:len(hostSigs)])
		methods = methods[len(hostSigs):]
	}

	if contract.Validation != nil {
		planned.validation = methods[0].Params[0]
	}

	planned.walk()

	return planned, nil
}

// signatures returns the interface named and the wire signatures of its methods.
func signatures(named *types.Named) (*types.Interface, []layout.Signature[types.Type], error) {
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s is not an interface", ErrContract, named.Obj().Name())
	}

	sigs, ok := gen.Signatures(iface)
	if !ok {
		return nil, nil, fmt.Errorf("%w: a method of %s does not take a context first and return an error last",
			ErrContract, named.Obj().Name())
	}

	return iface, sigs, nil
}

// describeInterface pairs the planned methods of the interface obj, iface, with their Go parameter names.
func describeInterface(obj *types.TypeName, iface *types.Interface, planned []layout.Method[types.Type]) *ifaceModel {
	described := &ifaceModel{obj: obj, methods: make([]method, len(planned))}

	for i, meth := range planned {
		goParams := iface.Method(i).Signature().Params()
		described.methods[i] = method{name: meth.Name, results: meth.Results}

		for k, typ := range meth.Params {
			described.methods[i].params = append(described.methods[i].params,
				param{name: goParams.At(k + 1).Name(), node: typ})
		}
	}

	return described
}

// walk finds the named types the methods reach and gives every package a namespace.
func (m *model) walk() {
	for _, iface := range []*ifaceModel{m.iface, m.host} {
		if iface == nil {
			continue
		}

		m.addSpace(iface.obj.Pkg())

		for _, meth := range iface.methods {
			for _, p := range meth.params {
				m.visit(p.node)
			}

			for _, r := range meth.results {
				m.visit(r)
			}
		}
	}

	if m.validation != nil {
		m.visit(m.validation)
	}

	m.assignNamespaces()
	slices.SortFunc(m.decls, func(a, b *types.TypeName) int { return cmp.Compare(m.ref(a), m.ref(b)) })
}

func (m *model) visit(n *node) {
	if m.visited[n] {
		return
	}

	m.visited[n] = true

	if obj := declared(n); obj != nil && m.nodes[obj] == nil {
		m.nodes[obj] = n
		m.decls = append(m.decls, obj)
		m.addSpace(obj.Pkg())
	}

	if n.Elem != nil {
		m.visit(n.Elem)
	}

	for _, field := range n.Fields {
		m.visit(field.Node)
	}
}

// declared returns the named type n is, which the declarations declare; nil for unnamed types, predeclared ones
// and time.Time (plg.Time).
func declared(n *node) *types.TypeName {
	if n.Kind == layout.Time {
		return nil
	}

	named, ok := types.Unalias(n.Type).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return nil
	}

	return named.Obj()
}

// addSpace notes that the declarations need a namespace for pkg, named after it until assignNamespaces settles
// names.
func (m *model) addSpace(pkg *types.Package) {
	if _, ok := m.spaces[pkg.Path()]; !ok {
		m.spaces[pkg.Path()] = pkg.Name()
	}
}

// assignNamespaces names each package's namespace after the package, in path order: a name the declarations reserve
// or a keyword gains a "_", and a name already taken a number.
func (m *model) assignNamespaces() {
	paths := make([]string, 0, len(m.spaces))
	for path := range m.spaces {
		paths = append(paths, path)
	}

	slices.Sort(paths)

	taken := map[string]bool{}

	for _, path := range paths {
		base := m.spaces[path]
		if reservedName(base) {
			base += "_"
		}

		name := base
		for i := 2; taken[name]; i++ {
			name = base + "_" + strconv.Itoa(i)
		}

		taken[name] = true
		m.spaces[path] = name
	}
}

// ref returns the declarations' name of a named type: its namespace and its name.
func (m *model) ref(obj *types.TypeName) string {
	return m.spaces[obj.Pkg().Path()] + "." + typeName(obj.Name())
}

// typeName returns the declarations' name of a Go type name: TypeScript's own type names gain a "_".
func typeName(name string) string {
	if tsTypeNames[name] {
		return name + "_"
	}

	return name
}

// reachesTime reports whether a value of type n may hold a time.Time.
func reachesTime(n *node) bool {
	return walksToTime(n, map[*node]bool{})
}

// walksToTime reports whether a value of type n may hold a time.Time through nodes not yet visited: a revisited
// node, part of a recursive type, reaches a time through the type's other parts or not at all. A node's answer
// depends on what the walk visited before it, so none is kept beyond its walk.
func walksToTime(n *node, visited map[*node]bool) bool {
	if visited[n] {
		return false
	}

	visited[n] = true

	if n.Kind == layout.Time || (n.Elem != nil && walksToTime(n.Elem, visited)) {
		return true
	}

	return slices.ContainsFunc(n.Fields, func(field layout.Field[types.Type]) bool {
		return walksToTime(field.Node, visited)
	})
}

// paramNames returns the names the parameters have in the declarations: their Go names, p1, p2, ... for
// parameters without one, keywords of either language followed by "_"; a name another parameter has gains more.
func paramNames(params []param) []string {
	names := make([]string, len(params))
	taken := map[string]bool{}

	for _, p := range params {
		taken[p.name] = true
	}

	for i, p := range params {
		name := p.name
		if name == "" || name == "_" {
			name = "p" + strconv.Itoa(i+1)
		} else if reservedName(name) {
			name += "_"
		}

		for name != p.name && taken[name] {
			name += "_"
		}

		names[i] = name
		taken[name] = true
	}

	return names
}

// reservedName reports whether name cannot name a namespace or a parameter: a keyword of TypeScript or Lua, or a
// global the declarations declare.
func reservedName(name string) bool {
	return tsKeywords[name] || luaKeywords[name] || globals[name]
}

//nolint:gochecknoglobals // immutable lookup tables
var (
	// The globals the declarations declare (TypeScript's and Lua's), and the names that cannot name a parameter
	// in strict mode.
	globals = setOf("plg", "host", "console", "fetch", "Headers", "Response", "http", "json", "secrets",
		"globalThis", "arguments", "eval", "self")
	tsKeywords = setOf("break", "case", "catch", "class", "const", "continue", "debugger", "default", "delete",
		"do", "else", "enum", "export", "extends", "false", "finally", "for", "function", "if", "import", "in",
		"instanceof", "new", "null", "return", "super", "switch", "this", "throw", "true", "try", "typeof", "var",
		"void", "while", "with", "yield", "let", "static", "implements", "interface", "package", "private",
		"protected", "public", "await", "undefined")
	luaKeywords = setOf("and", "break", "do", "else", "elseif", "end", "false", "for", "function", "goto", "if",
		"in", "local", "nil", "not", "or", "repeat", "return", "then", "true", "until", "while")
	// TypeScript's predeclared type names, which no declaration may take.
	tsTypeNames = setOf("any", "unknown", "never", "number", "string", "boolean", "symbol", "object", "bigint",
		"undefined", "void", "null", "this")
)

func setOf(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}

	return set
}
