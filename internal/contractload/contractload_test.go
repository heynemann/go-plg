package contractload_test

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/contractload"
	"github.com/heynemann/go-plg/internal/fastcontract"
	"github.com/heynemann/go-plg/internal/gen"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/shadowcontract"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/wire"
)

// TestDescribesContractsAsReflectionDoes loads contracts from their source and holds what plg validate gets to
// what a service gets from reflection: the same dispatcher, byte for byte, and the same script schema, planned
// from the interpreter's types. The contracts cover every kind of data type, recursive types, embedded aliases,
// a Host and a package redeclaring predeclared types.
//
//nolint:paralleltest // yaegi patches recursive StructOf types in place, process-wide (see PLAN.md, internal/goguest)
func TestDescribesContractsAsReflectionDoes(t *testing.T) {
	tests := []struct {
		typ, hostType    reflect.Type
		dir, iface, host string
	}{
		{reflect.TypeFor[testcontract.Fooer](), nil, "../testcontract", "Fooer", ""},
		{
			reflect.TypeFor[testcontract.Hosted](), reflect.TypeFor[testcontract.Host](), "../testcontract", "Hosted",
			"Host",
		},
		{
			reflect.TypeFor[testcontract.Aliased](), reflect.TypeFor[testcontract.AliasHost](), "../testcontract",
			"Aliased", "AliasHost",
		},
		{reflect.TypeFor[shadowcontract.Tables](), nil, "../shadowcontract", "Tables", ""},
	}

	for _, tc := range tests {
		loaded, err := contractload.Load(tc.dir, tc.typ.PkgPath(), tc.iface, tc.host)
		require.NoError(t, err, tc.iface)

		contract := loaded.Contract

		want, err := goabi.RenderDispatcher(tc.typ, tc.hostType, nil)
		require.NoError(t, err, tc.iface)

		var host *goabi.Interface

		if contract.Host != nil {
			host = contract.Host.Interface
		}

		got, err := goabi.Render(contract.Interface, host, nil)
		require.NoError(t, err, tc.iface)
		assert.Equal(t, string(want), string(got), tc.iface)

		assert.Equal(t, schema(t, reflectMethods(tc.typ)), schema(t, contract.Methods), tc.iface)

		if tc.hostType != nil {
			assert.Equal(t, schema(t, reflectMethods(tc.hostType)), schema(t, contract.Host.Methods), tc.host)
		}
	}
}

// TestRefusesWhatTheInterpreterCannotLoad loads a contract whose types the Go interpreter cannot build (a slice
// of itself): plg validate reports it, as a Go plugin of it could not load either.
//
//nolint:paralleltest // as TestDescribesContractsAsReflectionDoes
func TestRefusesWhatTheInterpreterCannotLoad(t *testing.T) {
	_, err := contractload.Load("../fastcontract", reflect.TypeFor[fastcontract.Kinds]().PkgPath(), "Kinds", "")
	require.ErrorIs(t, err, contractload.ErrInterpret)
	require.ErrorContains(t, err, "method Kinds.")

	dir := contractDir(t, "type Self []Self\n\ntype H interface{ M(ctx context.Context, s Self) error }\n\n"+
		"type I interface{ M(ctx context.Context) error }\n")

	_, err = contractload.Load(dir, "example.com/c", "I", "H")
	require.ErrorIs(t, err, contractload.ErrInterpret)
	require.ErrorContains(t, err, "host: ")
	require.ErrorContains(t, err, "method H.M")
}

func reflectMethods(iface reflect.Type) []reflect.Method {
	methods := make([]reflect.Method, iface.NumMethod())
	for i := range methods {
		methods[i] = iface.Method(i)
	}

	return methods
}

// schema plans methods as plg plans the script guests' schema, and writes it as the guests read it: each method
// with the kinds, sizes and field names of its parameter and result types, a recursive reference as "^n" (the
// type n levels up). Distinct named types of one kind are one type in the guests, so the table's indexes differ
// between reflection and the interpreter's types; what the guests see does not.
func schema(t *testing.T, methods []reflect.Method) string {
	t.Helper()

	var (
		planner wire.Planner
		encoded wire.Schema
	)

	plan := func(typ reflect.Type) *wire.Codec {
		codec, err := planner.For(typ)
		require.NoError(t, err, typ.String())

		return codec
	}

	for _, method := range methods {
		params := make([]*wire.Codec, 0, method.Type.NumIn()-1)
		for k := 1; k < method.Type.NumIn(); k++ {
			params = append(params, plan(method.Type.In(k)))
		}

		results := make([]*wire.Codec, 0, method.Type.NumOut()-1)
		for k := range method.Type.NumOut() - 1 {
			results = append(results, plan(method.Type.Out(k)))
		}

		encoded.AddMethod(method.Name, params, results)
	}

	return decodeSchema(t, encoded.Append(nil))
}

// schemaType is a type of a decoded schema.
type schemaType struct {
	fields []string // structs: the field names
	elems  []uint32 // the element's index, or the fields'
	kind   wire.Kind
	bits   byte
}

// schemaReader reads a schema.
type schemaReader struct {
	t *testing.T
	b []byte
}

func (r *schemaReader) u32() uint32 {
	require.GreaterOrEqual(r.t, len(r.b), 4)

	v := binary.LittleEndian.Uint32(r.b)
	r.b = r.b[4:]

	return v
}

func (r *schemaReader) name() string {
	n := r.u32()
	name := string(r.b[:n])
	r.b = r.b[n:]

	return name
}

func (r *schemaReader) typ() schemaType {
	typ := schemaType{kind: wire.Kind(r.b[0])}
	r.b = r.b[1:]

	switch typ.kind { //nolint:exhaustive // the other kinds carry nothing
	case wire.KindInt, wire.KindUint, wire.KindFloat:
		typ.bits, r.b = r.b[0], r.b[1:]
	case wire.KindSlice, wire.KindMap, wire.KindPointer:
		typ.elems = []uint32{r.u32()}
	case wire.KindStruct:
		for range r.u32() {
			typ.fields = append(typ.fields, r.name())
			typ.elems = append(typ.elems, r.u32())
		}
	default:
	}

	return typ
}

func decodeSchema(t *testing.T, b []byte) string {
	t.Helper()

	reader := &schemaReader{t: t, b: b}

	types := make([]schemaType, reader.u32())
	for i := range types {
		types[i] = reader.typ()
	}

	var out strings.Builder

	for range reader.u32() {
		out.WriteString(reader.name())

		for _, list := range []string{"(", ") ("} {
			out.WriteString(list)

			for range reader.u32() {
				writeType(&out, types, reader.u32(), nil)
				out.WriteString(" ")
			}
		}

		out.WriteString(")\n")
	}

	require.Empty(t, reader.b, "the whole schema is read")

	return out.String()
}

// writeType writes the type of index i, reached through the types of the indexes path.
func writeType(out *strings.Builder, types []schemaType, i uint32, path []uint32) {
	if at := slices.Index(path, i); at >= 0 {
		fmt.Fprintf(out, "^%d", len(path)-at)

		return
	}

	typ := &types[i]
	fmt.Fprintf(out, "%d/%d", typ.kind, typ.bits)

	path = append(path, i)

	for k, elem := range typ.elems {
		out.WriteString("{")

		if typ.fields != nil {
			out.WriteString(typ.fields[k] + ":")
		}

		writeType(out, types, elem, path)
		out.WriteString("}")
	}
}

//nolint:paralleltest // as TestDescribesContractsAsReflectionDoes
func TestRejects(t *testing.T) {
	tests := []struct {
		name, iface, host string
		want              error
		msg               string
	}{
		{"no such type", "Nope", "", gen.ErrInterface, "package testcontract declares no type Nope"},
		{"not an interface", "Order", "", gen.ErrInterface, "Order is a struct"},
		{"no such Host", "Fooer", "Nope", gen.ErrInterface, "host: "},
	}

	for _, tc := range tests {
		_, err := contractload.Load("../testcontract", "example.com/tc", tc.iface, tc.host)
		require.ErrorIs(t, err, tc.want, tc.name)
		require.ErrorContains(t, err, tc.msg, tc.name)
	}

	_, err := contractload.Load(t.TempDir(), "example.com/empty", "Fooer", "")
	require.ErrorIs(t, err, gen.ErrPackage, "no Go files")
}

// TestCheckGoTypeChecksPluginsAgainstTheContract type-checks Go plugins against testcontract.Fooer: the test
// plugin implementing it passes, a plugin of another method set fails naming its file, line and column, as does
// one that does not parse, and one importing a package beyond the contract and the standard library is left
// unchecked.
//
//nolint:paralleltest // as TestDescribesContractsAsReflectionDoes
func TestCheckGoTypeChecksPluginsAgainstTheContract(t *testing.T) {
	loaded, err := contractload.Load("../testcontract", reflect.TypeFor[testcontract.Fooer]().PkgPath(), "Fooer", "")
	require.NoError(t, err)

	plugins := os.DirFS(filepath.Join("..", "..", "testdata", "plugins", "go"))

	require.NoError(t, loaded.CheckGo(plugins, "fooer"))

	err = loaded.CheckGo(fstest.MapFS{"p/plugin.go": {Data: []byte(`package p

import (
	"context"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type half struct{}

func (half) Foo(context.Context, string) (int, error) { return 0, nil }

func Create() (testcontract.Fooer, error) { return half{}, nil }
`)}}, "p")
	require.ErrorIs(t, err, contractload.ErrGoTypes)
	require.ErrorContains(t, err, "p/plugin.go:13:52: cannot use half{} (value of struct type half) as "+
		"testcontract.Fooer value in return statement: half does not implement testcontract.Fooer (missing method ")

	err = loaded.CheckGo(plugins, "syntaxerror")
	require.ErrorIs(t, err, contractload.ErrGoTypes)
	require.ErrorContains(t, err, "syntaxerror/plugin.go:3:16: expected ')', found ','")

	err = loaded.CheckGo(
		fstest.MapFS{"plugin.go": {Data: []byte("package p\n\nimport _ \"example.com/other\"\n")}},
		".",
	)
	require.ErrorIs(t, err, contractload.ErrGoUnchecked)
	require.ErrorContains(t, err, "it imports example.com/other")

	err = loaded.CheckGo(plugins, "missing")
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.ErrorContains(t, err, "reading the plugin: ")
}

// contractDir writes a contract package of the given declarations, importing context, to a temporary directory.
func contractDir(t *testing.T, decls string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.go"),
		[]byte("package c\n\nimport \"context\"\n\nvar _ context.Context\n\n"+decls), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o750), "a subpackage is not the contract's source")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "s.go"), []byte("package sub\n"), 0o600))

	return dir
}

// TestDescribesEveryKindOfType loads a contract whose methods use every kind of type a dispatcher writes, and
// renders its dispatcher; the contract's source is its own files, not its subpackages'.
//
//nolint:paralleltest // as TestDescribesContractsAsReflectionDoes
func TestDescribesEveryKindOfType(t *testing.T) {
	dir := contractDir(t, `type S struct {
	Name string `+"`json:\"name\"`"+`
}

type Kinds interface {
	M(ctx context.Context, a [3]int, p *S, s []S, m map[string][]byte, more ...int) (map[string]*S, error)
}
`)

	loaded, err := contractload.Load(dir, "example.com/c", "Kinds", "")
	require.NoError(t, err)

	src, err := goabi.Render(loaded.Contract.Interface, nil, nil)
	require.NoError(t, err)
	assert.Contains(t, string(src), "var PlgCall_M func(p0.Context, [3]int, *p1.S, []p1.S, map[string][]uint8, "+
		"...int) (map[string]*p1.S, error)\n")

	entries, err := fs.ReadDir(loaded.Contract.Source, ".")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "c.go", entries[0].Name())

	require.Len(t, loaded.Contract.Methods, 1)
	assert.True(t, loaded.Contract.Methods[0].Type.IsVariadic())
	assert.Equal(t, 6, loaded.Contract.Methods[0].Type.NumIn())
}

// TestRejectsWhatTheDispatcherCannotWrite loads contracts whose interfaces refer to types the Go dispatcher
// cannot write, with the messages reflection gives a registered contract's.
//
//nolint:paralleltest // as TestDescribesContractsAsReflectionDoes
func TestRejectsWhatTheDispatcherCannotWrite(t *testing.T) {
	tests := []struct {
		name, decls string
		want        error
		msg         string
	}{
		{
			"unexported method", "type I interface{ m(ctx context.Context) error }", goabi.ErrRender,
			"method m is unexported",
		},
		{
			"func parameter", "type I interface{ M(ctx context.Context, f func()) error }", goabi.ErrRender,
			"method M: goabi: cannot render dispatcher: unnamed func type func()",
		},
		{
			"chan result", "type I interface{ M(ctx context.Context) (chan int, error) }", goabi.ErrRender,
			"unnamed chan type chan int",
		},
		{
			"interface parameter", "type I interface{ M(ctx context.Context, v interface{ X() }) error }",
			goabi.ErrRender, "unnamed interface type",
		},
		{
			"generic type", "type G[T any] struct{ V T }\n\ntype I interface{ M(ctx context.Context, g G[int]) error }",
			goabi.ErrRender, "generic type example.com/c.G[int]",
		},
		{
			"unexported field", "type I interface{ M(ctx context.Context, s struct{ x int }) error }", goabi.ErrRender,
			"unexported field x in struct{x int}",
		},
		{
			"map key", "type I interface{ M(ctx context.Context, m map[chan int]string) error }", goabi.ErrRender,
			"unnamed chan type",
		},
		{
			"slice element", "type I interface{ M(ctx context.Context, s []func()) error }", goabi.ErrRender,
			"unnamed func type",
		},
		{
			"struct field", "type I interface{ M(ctx context.Context, s struct{ F func() }) error }", goabi.ErrRender,
			"unnamed func type",
		},
		{
			"result", "type I interface{ M(ctx context.Context) (func(), error) }", goabi.ErrRender,
			"unnamed func type",
		},
		{
			"alias", "type J interface{ M(ctx context.Context) error }\n\ntype I = J", gen.ErrInterface,
			"I is an alias of example.com/c.J: name the interface itself",
		},
	}

	for _, tc := range tests {
		_, err := contractload.Load(contractDir(t, tc.decls), "example.com/c", "I", "")
		require.ErrorIs(t, err, tc.want, tc.name)
		require.ErrorContains(t, err, tc.msg, tc.name)
	}
}
