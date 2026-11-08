package typings

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"
	"go/types"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/heynemann/go-plg/internal/layout"
	"github.com/heynemann/go-plg/internal/offline"
	"github.com/heynemann/go-plg/internal/testcase"
)

// falseLiteral is false in Go, Lua, TypeScript and YAML.
const falseLiteral = "false"

// LanguageGo is the Go language, as plugin.yaml names it: Go plugins get a skeleton, no typings.
const LanguageGo = "go"

// ScaffoldTests is the example test case file a scaffold holds.
const ScaffoldTests = "tests/example.yaml"

// The zero time.Time of a language's skeleton, as test cases write it: Go's zero time, and the scripts' time of
// zero seconds and nanoseconds (the Unix epoch).
const (
	goZeroTime     = "0001-01-01T00:00:00Z"
	scriptZeroTime = "1970-01-01T00:00:00Z"
)

// goPluginPackage is the import path of plg's plugin package, whose Issue a Go plugin's Validate returns.
const goPluginPackage = "github.com/heynemann/go-plg/plugin"

// Scaffold is a new plugin's source, which plg init writes.
type Scaffold struct {
	// Entrypoint is the plugin's entrypoint, plugin.yaml's: the first of Files.
	Entrypoint string
	// Files are the entrypoint, a skeleton implementing every method of the interface with its results' zero
	// values (and Validate, when the contract has a validation input, returning no issues), and ScaffoldTests,
	// a test case per method (and Validate) calling it with zero inputs and expecting what the skeleton returns.
	Files []File
}

// NewScaffold writes the skeleton of a plugin in language (go, lua or typescript) implementing the contract's
// interface, whose Create takes the contract's Host when it has one (Lua and TypeScript plugins reach it through
// the host global). name is the plugin's key, which names a Go plugin's package.
func NewScaffold(language string, contract *Contract, name string) (*Scaffold, error) {
	planned, err := newModel(contract)
	if err != nil {
		return nil, err
	}

	var (
		entrypoint string
		source     []byte
		zeroTime   = scriptZeroTime
	)

	switch language {
	case LanguageGo:
		entrypoint, zeroTime = "plugin.go", goZeroTime

		source, err = planned.goSkeleton(name)
		if err != nil {
			return nil, err
		}
	case LanguageLua:
		entrypoint, source = "plugin.lua", planned.luaSkeleton()
	case LanguageTypeScript:
		entrypoint, source = "plugin.ts", planned.tsSkeleton()
	default:
		return nil, fmt.Errorf("%w: %q", ErrLanguage, language)
	}

	tests, err := planned.exampleTests(zeroTime)
	if err != nil {
		return nil, err
	}

	return &Scaffold{Entrypoint: entrypoint, Files: []File{
		{Name: entrypoint, Data: source},
		{Name: ScaffoldTests, Data: tests},
	}}, nil
}

// hasValidate reports whether the skeleton implements Validate: the contract has a validation input and the
// interface no method of that name.
func (m *model) hasValidate() bool {
	return m.validation != nil && !m.iface.has(testcase.ValidateCall)
}

// className is the name of the skeleton's class: the interface's, unless the declarations use it for a namespace
// or a global.
func (m *model) className() string {
	name := typeName(m.iface.obj.Name())

	for _, space := range m.spaces {
		if space == name {
			return name + "Plugin"
		}
	}

	if reservedName(name) {
		return name + "Plugin"
	}

	return name
}

// tsSkeleton renders a TypeScript plugin's skeleton.
func (m *model) tsSkeleton() []byte {
	var out strings.Builder

	class := m.className()

	fmt.Fprintf(&out, "// A %s plugin. Each method returns its results' zero values: make it do what %s does.\n",
		m.ref(m.iface.obj), goName(m.iface.obj))
	out.WriteString("// Its types are plg.d.ts's (plg types writes them).\n\n")
	fmt.Fprintf(&out, "class %s implements %s {\n", class, m.ref(m.iface.obj))

	for i, meth := range m.iface.methods {
		if i > 0 {
			out.WriteString("\n")
		}

		names := paramNames(meth.params)
		params := make([]string, len(meth.params))

		for k, p := range meth.params {
			params[k] = names[k] + ": " + m.tsValue(p.node, handedOver)
		}

		values := make([]string, len(meth.results))
		zeros := make([]string, len(meth.results))

		for k, r := range meth.results {
			values[k], zeros[k] = m.tsValue(r, handedBack), m.tsZero(r)
		}

		switch len(values) {
		case 0:
			fmt.Fprintf(&out, "  %s(%s): void {}\n", meth.name, strings.Join(params, ", "))
		case 1:
			fmt.Fprintf(&out, "  %s(%s): %s {\n    return %s;\n  }\n", meth.name, strings.Join(params, ", "),
				values[0], zeros[0])
		default:
			fmt.Fprintf(&out, "  %s(%s): [%s] {\n    return [%s];\n  }\n", meth.name, strings.Join(params, ", "),
				strings.Join(values, ", "), strings.Join(zeros, ", "))
		}
	}

	if m.hasValidate() {
		out.WriteString("\n  /** Validates the plugin against the service's input: no issues. */\n")
		fmt.Fprintf(&out, "  Validate(input: %s): plg.Issue[] {\n    return [];\n  }\n", m.ts(m.validation))
	}

	out.WriteString("}\n\n")
	fmt.Fprintf(&out, "export function Create(): %s {\n  return new %s();\n}\n", m.ref(m.iface.obj), class)

	return []byte(out.String())
}

// tsZero returns the zero value of n as a TypeScript expression.
func (m *model) tsZero(n *node) string {
	switch n.Kind { //nolint:exhaustive // the number kinds are the default
	case layout.Bool:
		return falseLiteral
	case layout.String:
		return `""`
	case layout.Time:
		return "{ seconds: 0, nanos: 0 }"
	case layout.Bytes, layout.Slice, layout.Map, layout.Pointer:
		return "null"
	case layout.Struct:
		if len(n.Fields) == 0 {
			return "{}"
		}

		fields := make([]string, len(n.Fields))
		for i, field := range n.Fields {
			fields[i] = tsProperty(field.Name) + ": " + m.tsZero(field.Node)
		}

		return "{ " + strings.Join(fields, ", ") + " }"
	default:
		return "0"
	}
}

// luaSkeleton renders a Lua plugin's skeleton.
func (m *model) luaSkeleton() []byte {
	var out strings.Builder

	class := m.className()

	fmt.Fprintf(&out, "-- A %s plugin. Each method returns its results' zero values: make it do what %s does.\n",
		m.ref(m.iface.obj), goName(m.iface.obj))
	out.WriteString("-- Its types are plg.d.lua's (plg types writes them).\n\n")
	fmt.Fprintf(&out, "---@class %s: %s\nlocal %s = {}\n%s.__index = %s\n", class, m.ref(m.iface.obj), class, class,
		class)

	for _, meth := range m.iface.methods {
		names := paramNames(meth.params)

		fmt.Fprintf(&out, "\nfunction %s:%s(%s)\n", class, meth.name, strings.Join(names, ", "))

		switch len(meth.results) {
		case 0:
		case 1:
			fmt.Fprintf(&out, "  return %s\n", m.luaZero(meth.results[0]))
		default:
			zeros := make([]string, len(meth.results))
			for k, r := range meth.results {
				zeros[k] = m.luaZero(r)
			}

			fmt.Fprintf(&out, "  return { %s }\n", strings.Join(zeros, ", "))
		}

		out.WriteString("end\n")
	}

	if m.hasValidate() {
		out.WriteString("\n-- Validates the plugin against the service's input: no issues.\n")
		fmt.Fprintf(&out, "function %s:Validate(input)\n  return nil\nend\n", class)
	}

	fmt.Fprintf(&out, "\nfunction Create()\n  return setmetatable({}, %s)\nend\n", class)

	return []byte(out.String())
}

// luaZero returns the zero value of n as a Lua expression.
func (m *model) luaZero(n *node) string {
	switch n.Kind { //nolint:exhaustive // the integer kinds are the default
	case layout.Bool:
		return falseLiteral
	case layout.String:
		return `""`
	case layout.Float32, layout.Float64:
		return "0.0"
	case layout.Time:
		return "{ seconds = 0, nanos = 0 }"
	case layout.Bytes, layout.Slice, layout.Map, layout.Pointer:
		return "nil"
	case layout.Struct:
		if len(n.Fields) == 0 {
			return "{}"
		}

		fields := make([]string, len(n.Fields))
		for i, field := range n.Fields {
			fields[i] = luaKey(field.Name) + " = " + m.luaZero(field.Node)
		}

		return "{ " + strings.Join(fields, ", ") + " }"
	default:
		return "0"
	}
}

// goSkeleton renders a Go plugin's skeleton, in the package named after the plugin.
func (m *model) goSkeleton(name string) ([]byte, error) {
	iface, ok := m.contract.Interface.Underlying().(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not an interface", ErrContract, m.contract.Interface.Obj().Name())
	}

	src := newGoFile(goTypeName(m.contract.Interface.Obj().Name()))
	recv := "*" + src.typeName

	var body strings.Builder

	field := ""
	if m.contract.Host != nil {
		field = "host " + src.qualified(m.contract.Host)
	}

	fmt.Fprintf(&body, "type %s struct{ %s }\n\n", src.typeName, field)

	iref := src.qualified(m.contract.Interface)

	if m.contract.Host != nil {
		fmt.Fprintf(&body, "// Create keeps the service's Host.\nfunc Create(host %s) (%s, error) {\n"+
			"\treturn &%s{host: host}, nil\n}\n", src.qualified(m.contract.Host), iref, src.typeName)
	} else {
		fmt.Fprintf(&body, "// Create returns the plugin.\nfunc Create() (%s, error) {\n\treturn &%s{}, nil\n}\n",
			iref, src.typeName)
	}

	for meth := range iface.Methods() {
		src.method(&body, recv, meth.Name(), meth.Signature())
	}

	if m.hasValidate() {
		body.WriteString("\n// Validate validates the plugin against the service's input: no issues.\n")
		fmt.Fprintf(&body, "func (%s) Validate(ctx %s.Context, in %s) ([]%s.Issue, error) {\n\treturn nil, nil\n}\n",
			recv, src.alias(types.NewPackage("context", "context")), src.typeString(m.contract.Validation),
			src.alias(types.NewPackage(goPluginPackage, "plugin")))
	}

	var out bytes.Buffer

	fmt.Fprintf(
		&out,
		"// Package %s implements %s. Each method returns its results' zero values: make it do what the\n"+
			"// interface says.\npackage %s\n\n",
		goPackageName(name),
		goName(m.contract.Interface.Obj()),
		goPackageName(name),
	)
	out.WriteString(src.importDecl())
	out.WriteString(body.String())

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("typings: the Go skeleton does not format: %w", err)
	}

	return formatted, nil
}

// goFile collects the imports of a Go skeleton: each package under its name, a name taken by another package or
// by the skeleton's own declarations gaining a number.
type goFile struct {
	aliases  map[string]string // by import path
	taken    map[string]bool
	typeName string
	order    []string // the import paths, in order of first use
}

func newGoFile(typeName string) *goFile {
	return &goFile{
		aliases: map[string]string{}, typeName: typeName,
		taken: map[string]bool{typeName: true, "Create": true, "host": true},
	}
}

// alias returns the name pkg is imported under, importing it.
func (g *goFile) alias(pkg *types.Package) string {
	if alias, ok := g.aliases[pkg.Path()]; ok {
		return alias
	}

	alias := pkg.Name()
	for i := 2; g.taken[alias]; i++ {
		alias = pkg.Name() + strconv.Itoa(i)
	}

	g.taken[alias] = true
	g.aliases[pkg.Path()] = alias
	g.order = append(g.order, pkg.Path())

	return alias
}

// typeString writes typ as the skeleton's source names it.
func (g *goFile) typeString(typ types.Type) string {
	return types.TypeString(typ, g.alias)
}

// qualified writes a named type as the skeleton's source names it.
func (g *goFile) qualified(named *types.Named) string {
	return g.typeString(named)
}

// method writes the method name of signature sig, returning its results' zero values.
func (g *goFile) method(out *strings.Builder, recv, name string, sig *types.Signature) {
	params := make([]string, sig.Params().Len())
	for i := range sig.Params().Len() {
		pname := sig.Params().At(i).Name()
		if pname == "" || g.taken[pname] || token.IsKeyword(pname) {
			pname = "_"
		}

		typ := g.typeString(sig.Params().At(i).Type())
		if sig.Variadic() && i == sig.Params().Len()-1 {
			typ = "..." + strings.TrimPrefix(typ, "[]")
		}

		params[i] = pname + " " + typ
	}

	results := make([]string, sig.Results().Len())
	zeros := make([]string, sig.Results().Len())

	for i := range sig.Results().Len() {
		typ := sig.Results().At(i).Type()
		results[i] = g.typeString(typ)
		zeros[i] = g.zero(typ)
	}

	resultList := strings.Join(results, ", ")
	if len(results) > 1 {
		resultList = "(" + resultList + ")"
	}

	fmt.Fprintf(out, "\nfunc (%s) %s(%s) %s {\n\treturn %s\n}\n", recv, name, strings.Join(params, ", "), resultList,
		strings.Join(zeros, ", "))
}

// zero returns the zero value of typ as a Go expression; the final error's is nil.
func (g *goFile) zero(typ types.Type) string {
	switch under := typ.Underlying().(type) {
	case *types.Basic:
		switch {
		case under.Info()&types.IsBoolean != 0:
			return falseLiteral
		case under.Info()&types.IsString != 0:
			return `""`
		default:
			return "0"
		}
	case *types.Struct, *types.Array:
		return g.typeString(typ) + "{}"
	default: // pointers, slices, maps, interfaces (the error), channels and functions
		return "nil"
	}
}

// importDecl writes the import declaration of the packages used.
func (g *goFile) importDecl() string {
	if len(g.order) == 0 {
		return ""
	}

	var out strings.Builder

	out.WriteString("import (\n")

	for _, path := range g.order {
		alias := g.aliases[path]
		if alias == lastElem(path) {
			fmt.Fprintf(&out, "\t%q\n", path)
		} else {
			fmt.Fprintf(&out, "\t%s %q\n", alias, path)
		}
	}

	out.WriteString(")\n\n")

	return out.String()
}

func lastElem(path string) string {
	return path[strings.LastIndexByte(path, '/')+1:]
}

// goTypeName is the skeleton's type: the interface's name, lowered.
func goTypeName(iface string) string {
	runes := []rune(iface)
	runes[0] = unicode.ToLower(runes[0])

	name := string(runes)
	if token.IsKeyword(name) || types.Universe.Lookup(name) != nil {
		name += "Plugin"
	}

	return name
}

// goPackageName is the Go package of a plugin named name: its letters and digits, lowered, else plugin.
func goPackageName(name string) string {
	var out strings.Builder

	for _, r := range strings.ToLower(name) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			out.WriteRune(r)
		}
	}

	pkg := out.String()
	if pkg == "" || !unicode.IsLetter(rune(pkg[0])) || token.IsKeyword(pkg) {
		return "plugin"
	}

	return pkg
}

// exampleTests writes the example test case file: a case per method calling it with zero inputs and expecting the
// skeleton's results, and one calling Validate expecting no issues. zeroTime is the skeleton's zero time.
func (m *model) exampleTests(zeroTime string) ([]byte, error) {
	cases := &yaml.Node{Kind: yaml.SequenceNode}

	for _, meth := range m.iface.methods {
		cases.Content = append(cases.Content, m.exampleCase(&meth, zeroTime))
	}

	if m.hasValidate() {
		cases.Content = append(cases.Content, mapping(blockStyle,
			str("name"), str("Validate reports no issues"), str("call"), str(testcase.ValidateCall),
			str("input"), mapping(blockStyle, str(testcase.ValidateParam), m.yamlZero(m.validation, zeroTime)),
			str("expect"), mapping(blockStyle, str("issues"), &yaml.Node{
				Kind: yaml.SequenceNode, Style: yaml.FlowStyle,
			}),
		))
	}

	doc := mapping(blockStyle, str("cases"), cases)
	doc.HeadComment = "Example test cases plg init wrote: each calls a method with zero values and expects what the\n" +
		"skeleton returns. Make them cases of what the plugin does: plg validate runs every " + testcase.Glob + "."

	var out bytes.Buffer

	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)

	err := enc.Encode(doc)
	if err == nil {
		err = enc.Close()
	}

	if err != nil {
		return nil, fmt.Errorf("typings: writing the example tests: %w", err)
	}

	return out.Bytes(), nil
}

// exampleCase writes the case calling meth with zero values and expecting the skeleton's results.
func (m *model) exampleCase(meth *method, zeroTime string) *yaml.Node {
	tc := mapping(blockStyle, str("name"), str(meth.name+" returns the skeleton's zero values"),
		str("call"), str(meth.name))

	if len(meth.params) > 0 {
		input := mapping(blockStyle)

		for k, p := range meth.params {
			name := p.name
			if name == "" || name == "_" {
				name = offline.ParamName(k + 1)
			}

			input.Content = append(input.Content, str(name), m.yamlZero(p.node, zeroTime))
		}

		tc.Content = append(tc.Content, str("input"), input)
	}

	if len(meth.results) == 0 {
		return tc
	}

	result := m.yamlZero(meth.results[0], zeroTime)

	if len(meth.results) > 1 {
		result = &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, r := range meth.results {
			result.Content = append(result.Content, m.yamlZero(r, zeroTime))
		}
	}

	tc.Content = append(tc.Content, str("expect"), mapping(blockStyle, str("result"), result))

	return tc
}

// yamlZero returns the zero value of n as a test case writes it, zeroTime being the skeleton's zero time.
func (m *model) yamlZero(n *node, zeroTime string) *yaml.Node {
	switch n.Kind { //nolint:exhaustive // the number kinds are the default
	case layout.Bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: falseLiteral}
	case layout.String:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "", Style: yaml.DoubleQuotedStyle}
	case layout.Time:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!timestamp", Value: zeroTime}
	case layout.Bytes, layout.Slice, layout.Map, layout.Pointer:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	case layout.Struct:
		fields := mapping(yaml.FlowStyle)
		for _, field := range n.Fields {
			fields.Content = append(fields.Content, str(field.Name), m.yamlZero(field.Node, zeroTime))
		}

		return fields
	case layout.Float32, layout.Float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: "0.0"}
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "0"}
	}
}

// mapping returns a mapping node of style holding content (keys and values in turn).
func mapping(style yaml.Style, content ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Style: style, Content: content}
}

// str returns a string scalar node.
func str(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

// blockStyle is yaml's default style: block mappings and sequences.
const blockStyle yaml.Style = 0
