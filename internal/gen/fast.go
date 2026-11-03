package gen

import (
	"bytes"
	"fmt"
	"go/types"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/heynemann/go-plg/internal/layout"
)

// plgimplImportPath is the import path of the run-time support of the fast paths.
const plgimplImportPath = plgImportPath + "/plgimpl"

// typesView is the [layout.View] of go/types, matching what layout.Reflect sees of the same types at run time.
type typesView struct{}

type typesClass = layout.Class[types.Type]

//nolint:gochecknoglobals // immutable lookup table
var basicKinds = map[types.BasicKind]layout.Kind{
	types.Bool: layout.Bool, types.Int: layout.Int, types.Int8: layout.Int8, types.Int16: layout.Int16,
	types.Int32: layout.Int32, types.Int64: layout.Int64, types.Uint: layout.Uint, types.Uint8: layout.Uint8,
	types.Uint16: layout.Uint16, types.Uint32: layout.Uint32, types.Uint64: layout.Uint64,
	types.Float32: layout.Float32, types.Float64: layout.Float64, types.String: layout.String,
}

// Classify implements [layout.View].
func (typesView) Classify(typ types.Type) (typesClass, error) {
	typ = types.Unalias(typ)
	if isNamed(typ, "time", "Time") {
		return typesClass{Kind: layout.Time}, nil
	}

	switch under := typ.Underlying().(type) {
	case *types.Basic:
		if kind, ok := basicKinds[under.Kind()]; ok {
			return typesClass{Kind: kind}, nil
		}
	case *types.Slice:
		return sliceClass(under), nil
	case *types.Map:
		if isBasic(under.Key(), types.String) {
			return typesClass{Kind: layout.Map, Elem: under.Elem()}, nil
		}
	case *types.Pointer:
		return typesClass{Kind: layout.Pointer, Elem: under.Elem()}, nil
	case *types.Struct:
		return typesClass{Kind: layout.Struct, Fields: structFields(under)}, nil
	default: // interfaces, channels, functions, arrays: unsupported
	}

	return typesClass{}, fmt.Errorf("%w: %s", layout.ErrUnsupported, typ)
}

// sliceClass classifies a slice: bytes when its elements are of a uint8 kind.
func sliceClass(slice *types.Slice) typesClass {
	if isBasic(slice.Elem(), types.Uint8) {
		return typesClass{Kind: layout.Bytes}
	}

	return typesClass{Kind: layout.Slice, Elem: slice.Elem()}
}

func structFields(st *types.Struct) []layout.RawField[types.Type] {
	fields := make([]layout.RawField[types.Type], st.NumFields())
	for i := range fields {
		field := st.Field(i)
		fields[i] = layout.RawField[types.Type]{
			Type: field.Type(), Name: field.Name(), Tag: reflect.StructTag(st.Tag(i)), Exported: field.Exported(),
		}
	}

	return fields
}

// Same implements [layout.View].
func (typesView) Same(a, b types.Type) bool {
	return types.Identical(a, b)
}

// isNamed reports whether t is the type name declared in the package with the given path.
func isNamed(t types.Type, pkgPath, name string) bool {
	named, ok := types.Unalias(t).(*types.Named)

	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkgPath && named.Obj().Name() == name &&
		named.TypeArgs() == nil
}

// isBasic reports whether t's underlying type is the basic kind.
func isBasic(t types.Type, kind types.BasicKind) bool {
	basic, ok := t.Underlying().(*types.Basic)

	return ok && basic.Kind() == kind
}

// signatures returns the methods of iface as the wire encodes their calls, or false when a method does not
// take a context first and return an error last (registration then fails, and plg gen generates no fast path).
func signatures(iface *types.Interface) ([]layout.Signature[types.Type], bool) {
	methods := make([]layout.Signature[types.Type], 0, iface.NumMethods())

	for method := range iface.Methods() {
		sig := method.Signature()
		params, results := sig.Params(), sig.Results()

		if params.Len() == 0 || !isNamed(params.At(0).Type(), "context", "Context") || results.Len() == 0 ||
			!types.Identical(results.At(results.Len()-1).Type(), types.Universe.Lookup("error").Type()) {
			return nil, false
		}

		wire := layout.Signature[types.Type]{Name: method.Name()}
		for i := 1; i < params.Len(); i++ {
			wire.Params = append(wire.Params, params.At(i).Type())
		}

		for i := range results.Len() - 1 {
			wire.Results = append(wire.Results, results.At(i).Type())
		}

		methods = append(methods, wire)
	}

	return methods, true
}

// fastGen renders the fast paths of a file: per interface, an adapter calling plgimpl.Invoker and a call type
// per method; per composite type the interfaces' methods reach, an encoder and a decoder shared by the file.
type fastGen struct {
	n      *namer
	pairs  []string     // {placeholder} → name, for code templates
	codecs []*typeCodec // in the order they were first needed
	funcs  bytes.Buffer // the codecs' functions
}

// typeCodec is the encoder and decoder of one composite type.
type typeCodec struct {
	node     *layout.Node[types.Type]
	enc, dec string
}

// fastIface is what the file registers for an interface with fast paths.
type fastIface struct {
	adapter string
	layout  string
}

// renderFastPaths renders the fast paths of the interfaces ifaces, declared as names, into body and returns, per
// interface, the Contract fields registering them (empty for an interface that has none).
func renderFastPaths(body *bytes.Buffer, n *namer, names []string, ifaces []*types.Interface) []string {
	fast := newFastGen(n)
	registrations := make([]string, len(names))

	for i, name := range names {
		if paths, ok := fast.iface(body, name, ifaces[i]); ok {
			registrations[i] = fast.code("\t\tFast: func(inv {impl}.Invoker) any { return &{adapter}{{inv}: inv} },\n"+
				"\t\tLayout: {layout},\n", "{adapter}", paths.adapter, "{layout}", strconv.Quote(paths.layout))
		}
	}

	body.Write(fast.funcs.Bytes())

	if !slices.ContainsFunc(registrations, func(fields string) bool { return fields != "" }) {
		delete(n.imports, plgimplImportPath) // no interface could have fast paths
	}

	return registrations
}

// localNames are the variables of the generated functions. Each gets a name distinct from every package-level
// name of the contract package, so no type the code mentions is shadowed.
//
//nolint:gochecknoglobals // immutable list
var localNames = []string{"b", "v", "r", "depth", "err", "n", "m", "s", "p", "i", "k", "e", "present", "c", "inv"}

func newFastGen(n *namer) *fastGen {
	g := &fastGen{n: n, pairs: []string{"{impl}", n.importAs(plgimplImportPath, "plgimpl")}}
	for _, name := range localNames {
		g.pairs = append(g.pairs, "{"+name+"}", n.unique(name))
	}

	return g
}

// code expands the template: {impl} is plgimpl's local name, {b}, {v}, ... the local variables' names, and vars
// more {placeholder}, value pairs (named apart from those). Expanded values are not expanded again.
func (g *fastGen) code(template string, vars ...string) string {
	return strings.NewReplacer(append(slices.Clip(g.pairs), vars...)...).Replace(template)
}

// iface renders the fast paths of the interface iface, declared as name, into body, or reports false when
// the interface cannot have any: its methods break the interface contract or use types the wire encoding
// does not support (registration then fails at init, with the precise reason).
func (g *fastGen) iface(body *bytes.Buffer, name string, iface *types.Interface) (fastIface, bool) {
	sigs, ok := signatures(iface)
	if !ok {
		return fastIface{}, false
	}

	plan, err := layout.Build[types.Type](typesView{}, sigs)
	if err != nil {
		return fastIface{}, false
	}

	adapter := g.n.unique("plg" + name + "Fast")
	calls := make([]string, len(plan.Methods))

	for i, meth := range plan.Methods {
		calls[i] = g.n.unique("plg" + name + meth.Name + "Call")
	}

	body.WriteString(g.code(`
// {adapter} implements {iface} for plg with the fast paths plg gen generated: typed calls, and codecs
// specialized to the interface's types.
type {adapter} struct {
	{inv} {impl}.Invoker
}

var _ {iface} = (*{adapter})(nil)
`, "{adapter}", adapter, "{iface}", name))

	for i := range iface.NumMethods() {
		method := iface.Method(i)

		g.adapterMethod(body, adapter, calls[i], i, method.Name(), method.Signature())
		g.callType(body, name, calls[i], plan.Methods[i])
	}

	return fastIface{adapter: adapter, layout: plan.Fingerprint()}, true
}

// adapterMethod renders the adapter's method with the given index: it calls the plugin through the invoker
// with a call value holding the arguments, and returns the results the call value then holds.
func (g *fastGen) adapterMethod(
	body *bytes.Buffer,
	adapter, call string,
	index int,
	name string,
	sig *types.Signature,
) {
	params := paramTypes(sig, g.n.qualify)

	fields := make([]string, 0, len(params))
	for i, typ := range params {
		params[i] = paramName(i) + " " + typ

		if i > 0 {
			fields = append(fields, paramName(i)+": "+paramName(i))
		}
	}

	vars := make([]string, 0, 18) //nolint:mnd // the pairs below, and {values}
	vars = append(vars,
		"{adapter}", adapter, "{method}", name, "{params}", strings.Join(params, ", "),
		"{results}", results(sig, g.n.qualify), "{call}", call, "{fields}", strings.Join(fields, ", "),
		"{ctx}", paramName(0), "{index}", strconv.Itoa(index),
	)

	if sig.Results().Len() == 1 {
		body.WriteString(g.code(`
func (a *{adapter}) {method}({params}){results} {
	{c} := {call}{{fields}}

	return a.{inv}.Invoke({ctx}, {index}, &{c})
}
`, vars...))

		return
	}

	values := make([]string, 0, sig.Results().Len())
	for i := range sig.Results().Len() - 1 {
		values = append(values, g.code("{c}.")+resultName(i))
	}

	body.WriteString(g.code(`
func (a *{adapter}) {method}({params}){results} {
	{c} := {call}{{fields}}

	{err} := a.{inv}.Invoke({ctx}, {index}, &{c})
	if {err} != nil {
		{c} = {call}{}
	}

	return {values}, {err}
}
`, append(vars, "{values}", strings.Join(values, ", "))...))
}

func resultName(i int) string {
	return "r" + strconv.Itoa(i)
}

// callType renders the call type of a method: its arguments and results as fields, and the plgimpl.Call
// methods encoding the ones and decoding the others.
func (g *fastGen) callType(body *bytes.Buffer, iface, call string, meth layout.Method[types.Type]) {
	var fields, enc, dec strings.Builder

	fallible := false

	for i, node := range meth.Params {
		field := g.code("{c}.") + paramName(i+1)
		fields.WriteString("\t" + paramName(i+1) + " " + g.typeString(node) + "\n")

		stmt, canFail := g.encode(node, field, "&"+field, "0")
		enc.WriteString("\t" + stmt + "\n")

		if canFail {
			fallible = true

			enc.WriteString(g.code("\tif {err} != nil {\n\t\treturn {b}, {impl}.Param({err}, {num})\n\t}\n\n",
				"{num}", strconv.Itoa(i+1)))
		}
	}

	for i, node := range meth.Results {
		field := g.code("{c}.") + resultName(i)
		fields.WriteString("\t" + resultName(i) + " " + g.typeString(node) + "\n")

		dec.WriteString("\t" + g.decode(node, field, "&"+field, "0") + "\n")
		dec.WriteString(g.code("\tif {err} != nil {\n\t\treturn {impl}.Result({err}, {num})\n\t}\n\n",
			"{num}", strconv.Itoa(i)))
	}

	encErr, decErr := "", ""
	if fallible {
		encErr = g.code("\tvar {err} error\n\n")
	}

	if len(meth.Results) > 0 {
		decErr = g.code("\tvar {err} error\n\n")
	}

	body.WriteString(g.code(`
// {call} is one call of {iface}.{method}: its arguments, then its results.
type {call} struct {
{fields}}

// AppendArgs implements plgimpl.Call.
func ({c} *{call}) AppendArgs({b} []byte) ([]byte, error) {
{encErr}{enc}	return {b}, nil
}

// DecodeResults implements plgimpl.Call.
func ({c} *{call}) DecodeResults({r} *{impl}.Reader) error {
{decErr}{dec}	return nil
}
`, "{call}", call, "{iface}", iface, "{method}", meth.Name, "{fields}", fields.String(), "{encErr}", encErr,
		"{enc}", enc.String(), "{decErr}", decErr, "{dec}", dec.String()))
}

func (g *fastGen) typeString(node *layout.Node[types.Type]) string {
	return types.TypeString(node.Type, g.n.qualify)
}

// reflectName is the name reflect gives a basic-kinded type: `pkg.Name` for a named one (by package name, as
// reflect does), else the kind's name.
func reflectName(node *layout.Node[types.Type]) string {
	if named, ok := types.Unalias(node.Type).(*types.Named); ok && named.Obj().Pkg() != nil {
		return named.Obj().Pkg().Name() + "." + named.Obj().Name()
	}

	return node.Kind.String()
}

// scalarCode is how generated code encodes and decodes a scalar kind: the statement appending {lv}, whether it
// can fail, and the plgimpl call reading one value of type {T} (named {name} as reflect names it).
type scalarCode struct {
	encode   string
	read     string
	fallible bool
}

//nolint:gochecknoglobals // immutable lookup table
var scalarCodes = func() map[layout.Kind]scalarCode {
	codes := map[layout.Kind]scalarCode{
		layout.Bool:    {"{b} = {impl}.AppendBool({b}, bool({lv}))", "ReadBool[{T}]({r})", false},
		layout.Float32: {"{b} = {impl}.AppendFloat({b}, float64({lv}))", "ReadFloat32[{T}]({r}, {name})", false},
		layout.Float64: {"{b} = {impl}.AppendFloat({b}, float64({lv}))", "ReadFloat64[{T}]({r})", false},
		layout.String:  {"{b}, {err} = {impl}.AppendString({b}, string({lv}))", "ReadString[{T}]({r})", true},
		layout.Time:    {"{b}, {err} = {impl}.AppendTime({b}, {lv})", "ReadTime({r})", true},
	}

	for _, kind := range []layout.Kind{layout.Int, layout.Int8, layout.Int16, layout.Int32, layout.Int64} {
		codes[kind] = scalarCode{"{b}, {err} = {impl}.AppendInt({b}, int64({lv}))", "ReadInt[{T}]({r}, {name})", true}
	}

	for _, kind := range []layout.Kind{layout.Uint, layout.Uint8, layout.Uint16, layout.Uint32, layout.Uint64} {
		codes[kind] = scalarCode{
			"{b}, {err} = {impl}.AppendUint({b}, uint64({lv}))",
			"ReadUint[{T}]({r}, {name})",
			true,
		}
	}

	return codes
}()

// codeOf returns the scalarCode of a scalar node. Byte slices of byte itself convert to []byte; those of a
// named uint8 type go through a generic helper.
func codeOf(node *layout.Node[types.Type]) scalarCode {
	if node.Kind != layout.Bytes {
		return scalarCodes[node.Kind]
	}

	if slice, ok := node.Type.Underlying().(*types.Slice); ok && types.Identical(slice.Elem(), types.Typ[types.Uint8]) {
		return scalarCode{"{b}, {err} = {impl}.AppendBytes({b}, []byte({lv}))", "ReadBytes[{T}]({r})", true}
	}

	return scalarCode{"{b}, {err} = {impl}.AppendByteSlice({b}, {lv})", "ReadByteSlice[{T}]({r})", true}
}

// encode returns the statement appending the value lv (of the node's type, whose address is ptr) to the
// buffer, at the given depth, and whether it can fail (setting the error variable).
func (g *fastGen) encode(node *layout.Node[types.Type], lv, ptr, depth string) (string, bool) {
	if node.Kind.Composite() {
		return g.code("{b}, {err} = {enc}({b}, {ptr}, {at})", "{enc}", g.codec(node).enc, "{ptr}", ptr, "{at}", depth),
			true
	}

	code := codeOf(node)

	return g.code(code.encode, "{lv}", lv), code.fallible
}

// decode returns the statement decoding a value into lv (of the node's type, whose address is ptr), at the
// given depth, setting the error variable.
func (g *fastGen) decode(node *layout.Node[types.Type], lv, ptr, depth string) string {
	if node.Kind.Composite() {
		return g.code("{err} = {dec}({r}, {ptr}, {at})", "{dec}", g.codec(node).dec, "{ptr}", ptr, "{at}", depth)
	}

	read := codeOf(node).read
	vars := []string{"{lv}", lv}

	// Expand the type and the name only where they appear: rendering a type imports its package.
	if strings.Contains(read, "{T}") {
		vars = append(vars, "{T}", g.typeString(node), "{name}", strconv.Quote(reflectName(node)))
	}

	return g.code("{lv}, {err} = {impl}."+read, vars...)
}

// codec returns the encoder and decoder names of a composite node, rendering the functions the first time.
func (g *fastGen) codec(node *layout.Node[types.Type]) *typeCodec {
	for _, codec := range g.codecs {
		if types.Identical(codec.node.Type, node.Type) {
			return codec
		}
	}

	name := "" // a named type's codecs are named after it
	if named, ok := types.Unalias(node.Type).(*types.Named); ok {
		name = named.Obj().Name()
	}

	codec := &typeCodec{node: node, enc: g.n.unique("plgEncode" + name), dec: g.n.unique("plgDecode" + name)}
	g.codecs = append(g.codecs, codec)

	// Rendering may need more codecs, which render themselves meanwhile: this one is written after them.
	funcs := g.encoder(codec) + g.decoder(codec)
	g.funcs.WriteString(funcs)

	return codec
}

// encoder renders the encoder of a composite type, as internal/wire's reflective codec encodes it.
func (g *fastGen) encoder(codec *typeCodec) string {
	var body string

	switch node := codec.node; node.Kind { //nolint:exhaustive // only composite kinds have codecs
	case layout.Slice:
		body = g.sliceEncoder(node)
	case layout.Map:
		body = g.mapEncoder(node)
	case layout.Pointer:
		body = g.pointerEncoder(node)
	default:
		body = g.structEncoder(node)
	}

	return g.code("\nfunc {enc}({b} []byte, {v} *{T}, {depth} int) ([]byte, error) {\n"+body+"}\n",
		"{enc}", codec.enc, "{T}", g.typeString(codec.node))
}

func (g *fastGen) sliceEncoder(node *layout.Node[types.Type]) string {
	stmt, canFail := g.encode(node.Elem, g.code("(*{v})[{i}]"), g.code("&(*{v})[{i}]"), g.code("{depth}+1"))

	check := ""
	if canFail {
		check = "\n\t\tif {err} != nil {\n\t\t\treturn {b}, {impl}.AtIndex({err}, {i})\n\t\t}"
	}

	return `	if *{v} == nil {
		return {impl}.AppendNil({b}), nil
	}

	{b}, {err} := {impl}.AppendLen({b}, len(*{v}), {depth})
	if {err} != nil {
		return {b}, {err}
	}

	for {i} := range *{v} {
		` + stmt + check + `
	}

	return {b}, nil
`
}

func (g *fastGen) mapEncoder(node *layout.Node[types.Type]) string {
	stmt, _ := g.encode(node.Elem, g.code("{e}"), g.code("&{e}"), g.code("{depth}+1"))

	return `	if *{v} == nil {
		return {impl}.AppendNil({b}), nil
	}

	{b}, {err} := {impl}.AppendLen({b}, len(*{v}), {depth})
	if {err} != nil {
		return {b}, {err}
	}

	for {k}, {e} := range *{v} {
		{b}, {err} = {impl}.AppendString({b}, string({k}))
		if {err} == nil {
			` + stmt + `
		}

		if {err} != nil {
			return {b}, {impl}.AtKey({err}, string({k}))
		}
	}

	return {b}, nil
`
}

func (g *fastGen) pointerEncoder(node *layout.Node[types.Type]) string {
	stmt, canFail := g.encode(node.Elem, g.code("(**{v})"), g.code("*{v}"), g.code("{depth}+1"))

	result := "nil"
	if canFail {
		result = "{err}"
	}

	return `	if *{v} == nil {
		return {impl}.AppendBool({b}, false), nil
	}

	{err} := {impl}.Deeper({depth})
	if {err} != nil {
		return {b}, {err}
	}

	{b} = {impl}.AppendBool({b}, true)
	` + stmt + `

	return {b}, ` + result + "\n"
}

func (g *fastGen) structEncoder(node *layout.Node[types.Type]) string {
	var fields strings.Builder

	for _, field := range node.Fields {
		lv := g.code("{v}.") + field.GoName
		stmt, canFail := g.encode(field.Node, lv, "&"+lv, g.code("{depth}+1"))
		fields.WriteString("\t" + stmt + "\n")

		if canFail {
			fields.WriteString("\tif {err} != nil {\n\t\treturn {b}, {impl}.AtField({err}, " +
				strconv.Quote(field.GoName) + ")\n\t}\n\n")
		}
	}

	return `	{err} := {impl}.Deeper({depth})
	if {err} != nil {
		return {b}, {err}
	}

` + fields.String() + "\treturn {b}, nil\n"
}

// decoder renders the decoder of a composite type, as internal/wire's reflective codec decodes it. It writes
// into a zero value.
func (g *fastGen) decoder(codec *typeCodec) string {
	var body string

	switch node := codec.node; node.Kind { //nolint:exhaustive // only composite kinds have codecs
	case layout.Slice:
		body = g.sliceDecoder(node)
	case layout.Map:
		body = g.mapDecoder(node)
	case layout.Pointer:
		body = g.pointerDecoder(node)
	default:
		body = g.structDecoder(node)
	}

	return g.code("\nfunc {dec}({r} *{impl}.Reader, {v} *{T}, {depth} int) error {\n"+body+"}\n",
		"{dec}", codec.dec, "{T}", g.typeString(codec.node))
}

func (g *fastGen) sliceDecoder(node *layout.Node[types.Type]) string {
	return `	{n}, {err} := {impl}.ReadSliceLen({r}, {depth}, ` + strconv.Itoa(node.Elem.MinSize) + `)
	if {err} != nil || {n} < 0 {
		return {err}
	}

	{s} := make({T}, {n})
	for {i} := range {s} {
		` + g.decode(node.Elem, g.code("{s}[{i}]"), g.code("&{s}[{i}]"), g.code("{depth}+1")) + `
		if {err} != nil {
			return {impl}.AtIndex({err}, {i})
		}
	}

	*{v} = {s}

	return nil
`
}

func (g *fastGen) mapDecoder(node *layout.Node[types.Type]) string {
	return `	{n}, {err} := {impl}.ReadMapLen({r}, {depth}, ` + strconv.Itoa(node.Elem.MinSize) + `)
	if {err} != nil || {n} < 0 {
		return {err}
	}

	{m} := make({T}, {n})

	for range {n} {
		{k}, {err} := {impl}.ReadKey({r}, {m})
		if {err} != nil {
			return {err}
		}

		var {e} ` + g.typeString(node.Elem) + `

		` + g.decode(node.Elem, g.code("{e}"), g.code("&{e}"), g.code("{depth}+1")) + `
		if {err} != nil {
			return {impl}.AtKey({err}, string({k}))
		}

		{m}[{k}] = {e}
	}

	if len({m}) != {n} {
		return {impl}.DuplicateKeys()
	}

	*{v} = {m}

	return nil
`
}

func (g *fastGen) pointerDecoder(node *layout.Node[types.Type]) string {
	return `	{present}, {err} := {impl}.ReadPresent({r}, {depth})
	if {err} != nil || !{present} {
		return {err}
	}

	{p} := new(` + g.typeString(node.Elem) + `)

	` + g.decode(node.Elem, g.code("*{p}"), g.code("{p}"), g.code("{depth}+1")) + `
	if {err} != nil {
		return {err}
	}

	*{v} = {p}

	return nil
`
}

func (g *fastGen) structDecoder(node *layout.Node[types.Type]) string {
	var fields strings.Builder

	for _, field := range node.Fields {
		lv := g.code("{v}.") + field.GoName
		fields.WriteString("\t" + g.decode(field.Node, lv, "&"+lv, g.code("{depth}+1")) + "\n")
		fields.WriteString("\tif {err} != nil {\n\t\treturn {impl}.AtField({err}, " + strconv.Quote(field.GoName) +
			")\n\t}\n\n")
	}

	return `	{err} := {impl}.Deeper({depth})
	if {err} != nil {
		return {err}
	}

` + fields.String() + "\treturn nil\n"
}
