package goabi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/heynemann/go-plg/plugin"
)

// initTemplate renders [InitFunc], which creates the plugin instance and binds its methods to the
// [CallPrefix]+<Method> variables, its optional Close to [CloseVar] and, when the plugins validate, its Validate
// to [ValidateVar]. yaegi's run-time interface assertions are unreliable, so Create is assigned to a variable of
// the exact expected type: yaegi rejects a Create with any other signature when it compiles the dispatcher. A nil
// instance is reported as a problem (a load failure), apart from Create's own error. With a Host, Create takes the
// dispatcher's Host stubs.
//
// The guest calls the bound method values directly: a forwarding function per method would cost a second
// interpreted call and a dynamic method lookup on every call, about half of a small call's time in the guest.
const initTemplate = `var create func(%[7]s) (%[1]s, error) = plugin.%[2]s

%[5]s
func %[3]s() (string, error) {
	v, err := create(%[8]s)
	if err != nil {
		return "", err
	}

	if v == nil {
		return %[4]s, nil
	}

%[6]s
	%[9]s = plgBindClose(v)
%[10]s
	return "", nil
}
`

// closeTemplate renders the binder of the instance's optional Close: a method Close() error, or Close(), that
// the guest calls when the host closes the instance. yaegi's interface assertions compare method names only,
// so an instance whose Close has another signature passes them and then panics when the method value is
// assigned: each binder recovers and reports no Close, as Go's assertion would.
const closeTemplate = `
var %[2]s func() error

type plgCloser interface{ Close() error }

type plgPlainCloser interface{ Close() }

func plgBindClose(v %[1]s) func() error {
	if f := plgBindCloser(v); f != nil {
		return f
	}

	if f := plgBindPlainCloser(v); f != nil {
		return func() error {
			f()

			return nil
		}
	}

	return nil
}

func plgBindCloser(v %[1]s) (f func() error) {
	defer func() {
		if recover() != nil {
			f = nil
		}
	}()

	if c, ok := v.(plgCloser); ok {
		f = c.Close
	}

	return f
}

func plgBindPlainCloser(v %[1]s) (f func()) {
	defer func() {
		if recover() != nil {
			f = nil
		}
	}()

	if c, ok := v.(plgPlainCloser); ok {
		f = c.Close
	}

	return f
}
`

// validateTemplate renders the binder of the instance's Validate, bound the way Close is: yaegi's interface
// assertions compare method names only, so an instance whose Validate has another signature passes them and then
// panics when the method is assigned to the variable of the exact type, and the binder recovers and reports no
// Validate, as Go's assertion would. That includes a Validate with one parameter more in front, which yaegi's
// assertion lets through as if it were the receiver (the same happens with Close). Type switches never match
// interpreted types in yaegi, so the assertion is the way to ask.
const validateTemplate = `
var %[2]s %[3]s

type plgValidator interface{ %[4]s%[5]s }

func plgBindValidate(v %[1]s) (f %[3]s) {
	defer func() {
		if recover() != nil {
			f = nil
		}
	}()

	if c, ok := v.(plgValidator); ok {
		f = c.%[4]s
	}

	return f
}
`

// bindTemplate renders the binder of one method. Binding a method that the instance promotes from a nil
// embedded interface (a plugin implementing only part of the interface) panics in yaegi, where Go panics only
// when the method is called: the binder then forwards each call instead, which panics like Go on the call.
const bindTemplate = `
func %[1]s%[2]s(v %[3]s) (f %[4]s) {
	defer func() {
		if recover() != nil {
			f = func(%[5]s)%[6]s { %[7]sv.%[2]s(%[8]s) }
		}
	}()

	return v.%[2]s
}
`

// hostStubTemplate renders one method of the Host stubs: it forwards to the [HostCallPrefix]+<Method>
// variable, which the guest fills with a call into the host before [InitFunc] runs.
const hostStubTemplate = `
func (%[1]s) %[2]s(%[3]s)%[4]s {
	%[5]s%[2]s(%[6]s)
}
`

// bindPrefix prefixes the dispatcher's binder of each method.
const bindPrefix = "plgBind_"

// hostStubType is the dispatcher's type implementing the Host interface, whose value Create receives.
const hostStubType = "plgHost"

// ErrRender reports an interface the dispatcher cannot be rendered for.
var ErrRender = errors.New("goabi: cannot render dispatcher")

// RenderDispatcher returns the Go source of the dispatcher package for the named interface iface: its
// [InitFunc] calls the plugin package's Create and binds each method of the instance to a variable
// [CallPrefix]+<Method> of the method's type. With a non-nil host interface, Create takes a value of the
// dispatcher's type implementing host, whose methods call the variables [HostCallPrefix]+<Method>. With a non-nil
// validate type V (a named type), it also binds the instance's Validate(context.Context, V) ([]plugin.Issue,
// error), if it has one, to [ValidateVar]. Every package a signature refers to is imported under a generated
// alias, so the output does not depend on package names. The output is deterministic.
func RenderDispatcher(iface, host, validate reflect.Type) ([]byte, error) {
	rnd := renderer{aliases: map[string]string{}, embedded: map[string]reflect.Type{}}

	err := rnd.collectInterface(iface)
	if err == nil && host != nil {
		err = rnd.collectInterface(host)
		if err != nil {
			err = fmt.Errorf("host: %w", err)
		}
	}

	if err == nil && validate != nil {
		err = rnd.collectFunc(ValidateType(validate))
		if err != nil {
			err = fmt.Errorf("validate: %w", err)
		}
	}

	if err != nil {
		return nil, err
	}

	rnd.assignAliases()

	return rnd.render(iface, host, validate), nil
}

// ValidateType returns the type of the Validate method of plugins validated with an input of type validate:
// func(context.Context, validate) ([]plugin.Issue, error).
func ValidateType(validate reflect.Type) reflect.Type {
	return reflect.FuncOf(
		[]reflect.Type{reflect.TypeFor[context.Context](), validate},
		[]reflect.Type{reflect.TypeFor[[]plugin.Issue](), reflect.TypeFor[error]()}, false)
}

type renderer struct {
	aliases  map[string]string       // import path -> alias; filled with "" by collect, assigned by assignAliases
	embedded map[string]reflect.Type // the dispatcher's own aliases, see collectEmbedded: name -> denoted type
	b        strings.Builder
}

// collectInterface records the import paths of a named interface and of its method signatures.
func (r *renderer) collectInterface(iface reflect.Type) error {
	if iface.Kind() != reflect.Interface || iface.Name() == "" || iface.PkgPath() == "" {
		return fmt.Errorf("%w: %s is not a named interface type", ErrRender, iface)
	}

	r.aliases[iface.PkgPath()] = ""

	for m := range iface.Methods() {
		if !m.IsExported() {
			return fmt.Errorf("%w: method %s is unexported", ErrRender, m.Name)
		}

		err := r.collectFunc(m.Type)
		if err != nil {
			return fmt.Errorf("method %s: %w", m.Name, err)
		}
	}

	return nil
}

func (r *renderer) collectFunc(fn reflect.Type) error {
	for typ := range fn.Ins() {
		err := r.collect(typ)
		if err != nil {
			return err
		}
	}

	for typ := range fn.Outs() {
		err := r.collect(typ)
		if err != nil {
			return err
		}
	}

	return nil
}

// collect records the import paths typ refers to and rejects what cannot be written as a type expression.
func (r *renderer) collect(typ reflect.Type) error {
	if typ.Kind() == reflect.UnsafePointer {
		return fmt.Errorf("%w: unsafe.Pointer", ErrRender)
	}

	if typ.Name() != "" {
		return r.collectNamed(typ)
	}

	switch typ.Kind() { //nolint:exhaustive // every other kind is named (handled above) or rejected by default
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return r.collect(typ.Elem())
	case reflect.Map:
		err := r.collect(typ.Key())
		if err != nil {
			return err
		}

		return r.collect(typ.Elem())
	case reflect.Struct:
		return r.collectFields(typ)
	default:
		return fmt.Errorf("%w: unnamed %s type %s", ErrRender, typ.Kind(), typ)
	}
}

// collectNamed records a named type's package: named types are referred to by name, their definition stays
// in their package.
func (r *renderer) collectNamed(typ reflect.Type) error {
	if strings.ContainsRune(typ.Name(), '[') {
		return fmt.Errorf("%w: generic type %s", ErrRender, typ)
	}

	if typ.PkgPath() != "" {
		r.aliases[typ.PkgPath()] = ""
	}

	return nil
}

func (r *renderer) collectFields(typ reflect.Type) error {
	for field := range typ.Fields() {
		if !field.IsExported() {
			return fmt.Errorf("%w: unexported field %s in %s", ErrRender, field.Name, typ)
		}

		err := r.collect(field.Type)
		if err == nil {
			err = r.collectEmbedded(&field)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// collectEmbedded records the dispatcher's own alias for a field that embeds an alias (type Embedded =
// Counter): reflection resolves the alias, so struct{ p.Counter } would be a different type than the
// contract's struct{ Embedded }. The dispatcher declares type Embedded = p.Counter and embeds that instead:
// the field's name and type are then the contract's. The name is exported, so its package does not matter,
// but the dispatcher has one Embedded only and must not declare that name already.
func (r *renderer) collectEmbedded(field *reflect.StructField) error {
	if !embedsAlias(field) {
		return nil
	}

	target := embeddedType(field.Type)
	if reservedName(field.Name) {
		return fmt.Errorf("%w: embedded field %s (of type %s) has a name the dispatcher declares", ErrRender,
			field.Name, target)
	}

	if prev := r.embedded[field.Name]; prev != nil && prev != target {
		return fmt.Errorf("%w: embedded fields named %s have the types %s and %s: the dispatcher can declare "+
			"only one alias %s", ErrRender, field.Name, prev, target, field.Name)
	}

	r.embedded[field.Name] = target

	return nil
}

// embedsAlias reports whether field is embedded under another name than its type's: it embeds an alias.
func embedsAlias(field *reflect.StructField) bool {
	return field.Anonymous && embeddedType(field.Type).Name() != field.Name
}

// embeddedType returns the type an embedded field of type typ names: T for both T and *T (T cannot be a
// pointer type).
func embeddedType(typ reflect.Type) reflect.Type {
	if typ.Kind() == reflect.Pointer && typ.Name() == "" {
		return typ.Elem()
	}

	return typ
}

// reservedName reports whether the dispatcher declares name, or may for some method.
func reservedName(name string) bool {
	return name == InitFunc || name == CloseVar || name == ValidateVar || strings.HasPrefix(name, CallPrefix) ||
		strings.HasPrefix(name, HostCallPrefix)
}

func (r *renderer) sortedPaths() []string {
	paths := make([]string, 0, len(r.aliases))
	for p := range r.aliases {
		paths = append(paths, p)
	}

	slices.Sort(paths)

	return paths
}

func (r *renderer) assignAliases() {
	for i, p := range r.sortedPaths() {
		r.aliases[p] = "p" + strconv.Itoa(i)
	}
}

// renderEmbedded declares the dispatcher's aliases of the names fields embed aliases under, see collectEmbedded.
func (r *renderer) renderEmbedded() {
	for _, name := range slices.Sorted(maps.Keys(r.embedded)) {
		r.printf("type %s = %s\n", name, r.typeExpr(r.embedded[name]))
	}

	if len(r.embedded) > 0 {
		r.printf("\n")
	}
}

func (r *renderer) render(iface, host, validate reflect.Type) []byte {
	r.printf("// Code generated by plg for %s. DO NOT EDIT.\n\npackage dispatch\n\nimport (\n", iface)

	for _, p := range r.sortedPaths() {
		r.printf("\t%s %s\n", r.aliases[p], strconv.Quote(p))
	}

	r.printf("\tplugin %s\n)\n\n", strconv.Quote(PluginImportPath))

	r.renderEmbedded()

	ifaceExpr := r.typeExpr(iface)
	sigs := make([]signature, iface.NumMethod())

	var vars, binds strings.Builder

	for i := range sigs {
		name := iface.Method(i).Name
		sigs[i] = r.signature(iface.Method(i).Type)
		fmt.Fprintf(&vars, "var %s%s %s\n", CallPrefix, name, sigs[i].funcExpr())
		fmt.Fprintf(&binds, "\t%s%s = %s%s(v)\n", CallPrefix, name, bindPrefix, name)
	}

	hostParam, hostArg := "", ""
	if host != nil {
		hostParam, hostArg = r.typeExpr(host), hostStubType+"{}"
	}

	bindValidate := ""
	if validate != nil {
		bindValidate = "\t" + ValidateVar + " = plgBindValidate(v)\n"
	}

	r.printf(initTemplate, ifaceExpr, CreateFunc, InitFunc,
		strconv.Quote(CreateFunc+" returned a nil "+iface.String()), vars.String(), binds.String(), hostParam, hostArg,
		CloseVar, bindValidate)
	r.printf(closeTemplate, ifaceExpr, CloseVar)

	if validate != nil {
		sig := r.signature(ValidateType(validate))
		r.printf(validateTemplate, ifaceExpr, ValidateVar, sig.funcExpr(), ValidateMethod,
			strings.TrimPrefix(sig.funcExpr(), "func"))
	}

	for i, sig := range sigs {
		ret := ""
		if sig.results != "" {
			ret = "return "
		}

		r.printf(bindTemplate, bindPrefix, iface.Method(i).Name, ifaceExpr, sig.funcExpr(),
			strings.Join(sig.params, ", "), sig.results, ret, strings.Join(sig.args, ", "))
	}

	if host != nil {
		r.renderHostStubs(host)
	}

	return []byte(r.b.String())
}

// renderHostStubs renders the type implementing host and the variables its methods call. Every method of a
// valid Host returns at least an error, but the stubs do not depend on it.
func (r *renderer) renderHostStubs(host reflect.Type) {
	r.printf("\n// %s implements %s by calling the host.\ntype %s struct{}\n\n", hostStubType, r.typeExpr(host),
		hostStubType)

	sigs := make([]signature, host.NumMethod())
	for i := range sigs {
		sigs[i] = r.signature(host.Method(i).Type)
		r.printf("var %s%s %s\n", HostCallPrefix, host.Method(i).Name, sigs[i].funcExpr())
	}

	for i, sig := range sigs {
		ret := ""
		if sig.results != "" {
			ret = "return "
		}

		r.printf(hostStubTemplate, hostStubType, host.Method(i).Name, strings.Join(sig.params, ", "), sig.results,
			ret+HostCallPrefix, strings.Join(sig.args, ", "))
	}
}

// signature is a method signature written as Go source.
type signature struct {
	results string   // the result list, with its leading space; empty without results
	types   []string // parameter types, the variadic one as ...T
	params  []string // parameter declarations a0 T0, a1 T1, ...
	args    []string // arguments forwarding the parameters: a0, a1, ..., the variadic one as aN...
}

func (r *renderer) signature(fn reflect.Type) signature {
	var sig signature

	for typ := range fn.Ins() {
		name := "a" + strconv.Itoa(len(sig.args))
		expr, arg := r.typeExpr(typ), name

		if fn.IsVariadic() && len(sig.args) == fn.NumIn()-1 {
			expr, arg = "..."+r.typeExpr(typ.Elem()), name+"..."
		}

		sig.types = append(sig.types, expr)
		sig.params = append(sig.params, name+" "+expr)
		sig.args = append(sig.args, arg)
	}

	results := make([]string, 0, fn.NumOut())
	for typ := range fn.Outs() {
		results = append(results, r.typeExpr(typ))
	}

	switch len(results) {
	case 0:
	case 1:
		sig.results = " " + results[0]
	default:
		sig.results = " (" + strings.Join(results, ", ") + ")"
	}

	return sig
}

func (s *signature) funcExpr() string {
	return "func(" + strings.Join(s.types, ", ") + ")" + s.results
}

// typeExpr writes typ as a type expression; collect has validated it.
func (r *renderer) typeExpr(typ reflect.Type) string {
	if typ.Name() != "" {
		if typ.PkgPath() == "" {
			return typ.Name() // predeclared
		}

		return r.aliases[typ.PkgPath()] + "." + typ.Name()
	}

	switch typ.Kind() { //nolint:exhaustive // collect admits only these unnamed kinds
	case reflect.Pointer:
		return "*" + r.typeExpr(typ.Elem())
	case reflect.Slice:
		return "[]" + r.typeExpr(typ.Elem())
	case reflect.Array:
		return "[" + strconv.Itoa(typ.Len()) + "]" + r.typeExpr(typ.Elem())
	case reflect.Map:
		return "map[" + r.typeExpr(typ.Key()) + "]" + r.typeExpr(typ.Elem())
	default: // reflect.Struct
		return r.structExpr(typ)
	}
}

func (r *renderer) structExpr(typ reflect.Type) string {
	fields := make([]string, 0, typ.NumField())

	for field := range typ.Fields() {
		expr := r.fieldExpr(&field)
		if field.Tag != "" {
			expr += " " + strconv.Quote(string(field.Tag))
		}

		fields = append(fields, expr)
	}

	return "struct{ " + strings.Join(fields, "; ") + " }"
}

// fieldExpr writes a struct field without its tag. A field embedding an alias embeds the dispatcher's alias of
// its name, see collectEmbedded.
func (r *renderer) fieldExpr(field *reflect.StructField) string {
	switch {
	case !field.Anonymous:
		return field.Name + " " + r.typeExpr(field.Type)
	case !embedsAlias(field):
		return r.typeExpr(field.Type)
	case field.Type != embeddedType(field.Type):
		return "*" + field.Name
	default:
		return field.Name
	}
}

func (r *renderer) printf(format string, args ...any) {
	fmt.Fprintf(&r.b, format, args...)
}
