package goabi_test

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/wire"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestInitPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	for _, payload := range []goabi.InitPayload{
		{GoPath: "/gopath", Dispatch: goabi.DispatchImportPath, Methods: []string{"Foo", "Bar"}, HostMethods: []string{}},
		{GoPath: "", Dispatch: "", Methods: []string{}, HostMethods: []string{}},
		{GoPath: "/x", Dispatch: "y", Methods: []string{""}, HostMethods: []string{}},
		{
			GoPath: "/gopath", Dispatch: "d", Methods: []string{"Foo"}, Host: "contract.Host",
			HostPath: "example.com/contract", HostMethods: []string{"Lookup", "Sum"},
		},
	} {
		b, err := payload.Append(nil)
		require.NoError(t, err)

		got, err := goabi.DecodeInitPayload(b)
		require.NoError(t, err)
		assert.Equal(t, payload, got)
	}
}

func TestDecodeInitPayloadRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	valid, err := (&goabi.InitPayload{
		GoPath: "/g", Dispatch: "d", Methods: []string{"A", "B"}, Host: "c.H", HostPath: "c", HostMethods: []string{"M"},
	}).Append(nil)
	require.NoError(t, err)

	countOffset := 4 + 2 + 4 + 1                      // the two strings before the method count
	hostOffset := countOffset + 8 + 2*(4+1)           // the method count and the two methods
	hostCountOffset := hostOffset + (4 + 3) + (4 + 1) // the host and its path
	methodsEnd := hostOffset                          // where the methods end
	hostMethodsEnd := len(valid)                      // where the host methods end
	hugeCount := bytes.Clone(valid)
	copy(hugeCount[countOffset:], wire.AppendUint(nil, 1<<40))

	hugeHostCount := bytes.Clone(valid)
	copy(hugeHostCount[hostCountOffset:], wire.AppendUint(nil, 1<<40))

	tests := []struct {
		name string
		msg  string
		in   []byte
	}{
		{"empty", "go path", nil},
		{"truncated dispatch path", "dispatch path", valid[:7]},
		{"truncated count", "method count", valid[:countOffset+3]},
		{"count beyond the payload", "exceeds the payload", hugeCount},
		{"truncated method", "method 1", valid[:methodsEnd-1]},
		{"truncated host", "host: ", valid[:hostOffset+5]},
		{"truncated host path", "host path", valid[:hostOffset+8]},
		{"truncated host method count", "host method count", valid[:hostCountOffset+3]},
		{"host method count beyond the payload", "host method count exceeds the payload", hugeHostCount},
		{"truncated host method", "host method 0", valid[:hostMethodsEnd-1]},
		{"trailing bytes", "after host methods: wire: trailing", append(bytes.Clone(valid), 0)},
	}

	for _, tc := range tests {
		_, err := goabi.DecodeInitPayload(tc.in)
		require.ErrorIs(t, err, goabi.ErrPayload, tc.name)
		require.ErrorContains(t, err, tc.msg, tc.name)
	}
}

// FuzzDecodeInitPayload: the guest decodes this from host memory; it must never panic or over-allocate, and
// whatever decodes re-encodes to the same bytes.
func FuzzDecodeInitPayload(f *testing.F) {
	seed, err := (&goabi.InitPayload{GoPath: "/gopath", Dispatch: "d", Methods: []string{"M"}}).Append(nil)
	if err != nil {
		f.Fatal(err)
	}

	f.Add(seed)
	f.Add(seed[:5])
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, in []byte) {
		p, err := goabi.DecodeInitPayload(in)
		if err != nil {
			return
		}

		out, err := p.Append(nil)
		require.NoError(t, err)
		assert.Equal(t, in, out)
	})
}

func TestRenderDispatcherForTheTestContract(t *testing.T) {
	t.Parallel()

	src, err := goabi.RenderDispatcher(reflect.TypeFor[testcontract.Fooer](), nil)
	require.NoError(t, err)

	file := parse(t, src)
	assert.Equal(t, "dispatch", file.Name.Name)

	imports := map[string]string{}
	for _, imp := range file.Imports {
		imports[strings.Trim(imp.Path.Value, `"`)] = imp.Name.Name
	}

	assert.Equal(t, map[string]string{
		"context": "p0",
		reflect.TypeFor[testcontract.Fooer]().PkgPath(): "p1",
		goabi.PluginImportPath:                          "plugin",
	}, imports, "every package is aliased, so package names never matter")

	for _, want := range []string{
		"var create func() (p1.Fooer, error) = plugin.Create",
		"func PlgInit() (string, error) {",
		"var PlgCall_Foo func(p0.Context, string) (string, error)\n",
		"\tPlgCall_Foo = plgBind_Foo(v)\n", // PlgInit binds the instance's method
		"func plgBind_Foo(v p1.Fooer) (f func(p0.Context, string) (string, error)) {",
		// a method yaegi cannot bind is forwarded
		"f = func(a0 p0.Context, a1 string) (string, error) { return v.Foo(a0, a1) }",
		"var PlgCall_Boil func(p0.Context, p1.Celsius) (bool, float64, error)\n",
		"var PlgCall_Fail func(p0.Context, string) error\n", // a single result is not parenthesized
		"var PlgClose func() error\n",
		"\tPlgClose = plgBindClose(v)\n", // PlgInit binds the instance's optional Close
		"func plgBindClose(v p1.Fooer) func() error {",
	} {
		assert.Contains(t, string(src), want)
	}

	assert.NotContains(t, string(src), "plgHost", "without a Host, there are no stubs")

	again, err := goabi.RenderDispatcher(reflect.TypeFor[testcontract.Fooer](), nil)
	require.NoError(t, err)
	assert.Equal(t, src, again, "rendering is deterministic")
}

func TestRenderDispatcherWithAHost(t *testing.T) {
	t.Parallel()

	src, err := goabi.RenderDispatcher(reflect.TypeFor[testcontract.Hosted](), reflect.TypeFor[testcontract.Host]())
	require.NoError(t, err)
	parse(t, src)

	for _, want := range []string{
		"var create func(p1.Host) (p1.Hosted, error) = plugin.Create",
		"\tv, err := create(plgHost{})\n",
		"type plgHost struct{}\n",
		"var PlgHostCall_Lookup func(p0.Context, string) (p1.User, error)\n",
		"func (plgHost) Lookup(a0 p0.Context, a1 string) (p1.User, error) {\n\treturn PlgHostCall_Lookup(a0, a1)\n}",
		"func (plgHost) Fail(a0 p0.Context, a1 string) error {\n\treturn PlgHostCall_Fail(a0, a1)\n}",
		"func (plgHost) Sum(a0 p0.Context, a1 int, a2 ...int) (int, error) {\n\treturn PlgHostCall_Sum(a0, a1, a2...)\n}",
		"var PlgCall_Describe func(p0.Context, string) (p1.User, error)\n", // the plugin side is unchanged
	} {
		assert.Contains(t, string(src), want)
	}

	// A Host from another package is imported too; a method without results is forwarded without return.
	src, err = goabi.RenderDispatcher(reflect.TypeFor[testcontract.Fooer](), reflect.TypeFor[Shapes]())
	require.NoError(t, err)
	parse(t, src)

	for _, want := range []string{
		`p1 "github.com/heynemann/go-plg/internal/goabi_test"`,
		`p2 "github.com/heynemann/go-plg/internal/testcontract"`,
		"var create func(p1.Shapes) (p2.Fooer, error) = plugin.Create",
		"var PlgHostCall_Maps func(p0.Context, map[string]*p1.Point, p3.Time) (map[string][]int, error)\n",
		"func (plgHost) None() {\n\tPlgHostCall_None()\n}",
	} {
		assert.Contains(t, string(src), want)
	}
}

func parse(t *testing.T, src []byte) *ast.File {
	t.Helper()

	f, err := parser.ParseFile(token.NewFileSet(), "dispatch.go", src, parser.AllErrors)
	require.NoError(t, err, string(src))

	return f
}

type (
	Point struct {
		X, Y int
	}
	Inner  struct{}
	Shapes interface {
		Slices(ctx context.Context, a []string, b [][]byte, c [3]int) ([]Point, error)
		Maps(ctx context.Context, m map[string]*Point, t time.Time) (map[string][]int, error)
		Anonymous(ctx context.Context, s struct {
			Inner
			*Point

			Name string `json:"name"`
		}) error
		Variadic(ctx context.Context, format string, args ...int) error
		None()
	}
)

func TestRenderDispatcherTypeExpressions(t *testing.T) {
	t.Parallel()

	src, err := goabi.RenderDispatcher(reflect.TypeFor[Shapes](), nil)
	require.NoError(t, err)
	parse(t, src)

	text := string(src)
	for _, want := range []string{
		`p1 "github.com/heynemann/go-plg/internal/goabi_test"`,
		`p2 "time"`,
		"var PlgCall_Slices func(p0.Context, []string, [][]uint8, [3]int) ([]p1.Point, error)\n",
		"var PlgCall_Maps func(p0.Context, map[string]*p1.Point, p2.Time) (map[string][]int, error)\n",
		"var PlgCall_Anonymous func(p0.Context, struct{ p1.Inner; *p1.Point; Name string \"json:\\\"name\\\"\" }) error\n",
		"var PlgCall_Variadic func(p0.Context, string, ...int) error\n",
		"var PlgCall_None func()\n",
		"\tPlgCall_None = plgBind_None(v)\n",
		"f = func() { v.None() }",
		"f = func(a0 p0.Context, a1 string, a2 ...int) error { return v.Variadic(a0, a1, a2...) }",
	} {
		assert.Contains(t, text, want)
	}
}

type (
	Number = int
	Pair   = struct{ A, B int }
	Time   = time.Time // named as its type: embedded as p.Time
	// Embedded is another alias of the name testcontract.Embedded has.
	Embedded = Point
	PlgClose = Point

	PlgHostCall_Point = Point //nolint:revive,staticcheck // var-naming, ST1003: the name of a Host stub variable
	aliasShapes       interface {
		M(ctx context.Context, s struct {
			*Pair
			Time
			Number
			testcontract.Counter
		}) (struct{ *testcontract.Embedded }, error)
	}
	conflictingAliases interface {
		M(ctx context.Context, a struct{ testcontract.Embedded }, b struct{ *Embedded }) error
	}
	reservedAlias interface {
		M(ctx context.Context, s struct{ PlgClose }) error
	}
	reservedPrefixAlias interface {
		M(ctx context.Context, s struct{ PlgHostCall_Point }) error
	}
)

// TestRenderDispatcherKeepsEmbeddedAliases checks embedded fields whose name is not their type's: reflection
// resolves the alias they embed, so the dispatcher declares its own alias of the name and embeds it, keeping the
// field's name. Fields embedding a type by its own name are unchanged.
func TestRenderDispatcherKeepsEmbeddedAliases(t *testing.T) {
	t.Parallel()

	src, err := goabi.RenderDispatcher(reflect.TypeFor[testcontract.Aliased](),
		reflect.TypeFor[testcontract.AliasHost]())
	require.NoError(t, err)
	parse(t, src)

	text := string(src)
	assert.Equal(t, 1, strings.Count(text, "type Embedded = "), "one alias serves every field:\n%s", text)

	for _, want := range []string{
		"\tplugin \"plg.local/plugin\"\n)\n\ntype Embedded = p1.Counter\n\nvar create func(p1.AliasHost) (p1.Aliased, error)",
		"var PlgCall_Bump func(p0.Context, struct{ Embedded; By int }) (struct{ Embedded; By int }, error)\n",
		"var PlgHostCall_Double func(p0.Context, struct{ Embedded; By int }) (struct{ *Embedded }, error)\n",
		"func (plgHost) Double(a0 p0.Context, a1 struct{ Embedded; By int }) (struct{ *Embedded }, error) {\n",
	} {
		assert.Contains(t, text, want)
	}

	src, err = goabi.RenderDispatcher(reflect.TypeFor[aliasShapes](), nil)
	require.NoError(t, err)
	parse(t, src)

	text = string(src)
	for _, want := range []string{
		"type Embedded = p2.Counter\ntype Number = int\ntype Pair = struct{ A int; B int }\n\n",
		"var PlgCall_M func(p0.Context, struct{ *Pair; p3.Time; Number; p2.Counter }) (struct{ *Embedded }, error)\n",
	} {
		assert.Contains(t, text, want)
	}
}

type (
	generic[T any] struct{ V T }
	hidden         interface{ m() }
	badFunc        interface {
		M(ctx context.Context, f func()) error
	}
	badChan interface {
		M(ctx context.Context) (chan int, error)
	}
	badAnyIface interface {
		M(ctx context.Context, v interface{ X() }) error
	}
	badGeneric interface {
		M(ctx context.Context, g generic[int]) error
	}
	badField interface {
		M(ctx context.Context, s struct{ x int }) error
	}
	badUnsafe interface {
		M(ctx context.Context, s []unsafe.Pointer) error
	}
	badNested interface {
		M(ctx context.Context, m map[string][]func()) error
	}
	badKey interface {
		M(ctx context.Context, m map[chan int]string) error
	}
	badFieldType interface {
		M(ctx context.Context, s struct{ F func() }) error
	}
)

func TestRenderDispatcherRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  reflect.Type
		msg  string
	}{
		{"not an interface", reflect.TypeFor[Point](), "not a named interface"},
		{"unnamed interface", reflect.TypeFor[interface{ M() }](), "not a named interface"},
		{"unexported method", reflect.TypeFor[hidden](), "method m is unexported"},
		{"func parameter", reflect.TypeFor[badFunc](), "method M: goabi: cannot render dispatcher: unnamed func"},
		{"chan result", reflect.TypeFor[badChan](), "unnamed chan"},
		{"unnamed interface parameter", reflect.TypeFor[badAnyIface](), "unnamed interface"},
		{"generic type", reflect.TypeFor[badGeneric](), "generic type"},
		{"unexported field", reflect.TypeFor[badField](), "unexported field x"},
		{"unsafe pointer", reflect.TypeFor[badUnsafe](), "unsafe.Pointer"},
		{"nested func", reflect.TypeFor[badNested](), "unnamed func"},
		{"chan map key", reflect.TypeFor[badKey](), "unnamed chan"},
		{"func struct field", reflect.TypeFor[badFieldType](), "unnamed func"},
		{
			"one name, two aliases", reflect.TypeFor[conflictingAliases](),
			"embedded fields named Embedded have the types testcontract.Counter and goabi_test.Point: the " +
				"dispatcher can declare only one alias Embedded",
		},
		{
			"alias named as the dispatcher's variable", reflect.TypeFor[reservedAlias](),
			"embedded field PlgClose (of type goabi_test.Point) has a name the dispatcher declares",
		},
		{
			"alias named as a Host stub variable", reflect.TypeFor[reservedPrefixAlias](),
			"embedded field PlgHostCall_Point (of type goabi_test.Point) has a name the dispatcher declares",
		},
	}

	for _, tc := range tests {
		_, err := goabi.RenderDispatcher(tc.typ, nil)
		require.ErrorIs(t, err, goabi.ErrRender, tc.name)
		require.ErrorContains(t, err, tc.msg, tc.name)

		_, err = goabi.RenderDispatcher(reflect.TypeFor[testcontract.Fooer](), tc.typ)
		require.ErrorIs(t, err, goabi.ErrRender, tc.name+" as the host")
		require.ErrorContains(t, err, "host: ", tc.name+" as the host")
		require.ErrorContains(t, err, tc.msg, tc.name+" as the host")
	}
}
