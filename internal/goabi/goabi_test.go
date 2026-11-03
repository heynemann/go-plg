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
		{GoPath: "/gopath", Dispatch: goabi.DispatchImportPath, Methods: []string{"Foo", "Bar"}},
		{GoPath: "", Dispatch: "", Methods: []string{}},
		{GoPath: "/x", Dispatch: "y", Methods: []string{""}},
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

	valid, err := (&goabi.InitPayload{GoPath: "/g", Dispatch: "d", Methods: []string{"A", "B"}}).Append(nil)
	require.NoError(t, err)

	countOffset := 4 + 2 + 4 + 1 // the two strings before the method count

	hugeCount := bytes.Clone(valid)
	copy(hugeCount[countOffset:], wire.AppendUint(nil, 1<<40))

	tests := []struct {
		name string
		msg  string
		in   []byte
	}{
		{"empty", "go path", nil},
		{"truncated dispatch path", "dispatch path", valid[:7]},
		{"truncated count", "method count", valid[:countOffset+3]},
		{"count beyond the payload", "exceeds the payload", hugeCount},
		{"truncated method", "method 1", valid[:len(valid)-1]},
		{"trailing bytes", "trailing", append(bytes.Clone(valid), 0)},
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

	src, err := goabi.RenderDispatcher(reflect.TypeFor[testcontract.Fooer]())
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

	s := string(src)
	assert.Contains(t, s, "var create func() (p1.Fooer, error) = plugin.Create")
	assert.Contains(t, s, "func PlgInit() (string, error) {")
	assert.Contains(t, s, "func PlgCall_Foo(a0 p0.Context, a1 string) (string, error) {\n\treturn inst.Foo(a0, a1)\n}")
	assert.Contains(t, s, "func PlgCall_Boil(a0 p0.Context, a1 p1.Celsius) (bool, float64, error) {")
	assert.Contains(t, s, "func PlgCall_Fail(a0 p0.Context, a1 string) (error) {")

	again, err := goabi.RenderDispatcher(reflect.TypeFor[testcontract.Fooer]())
	require.NoError(t, err)
	assert.Equal(t, src, again, "rendering is deterministic")
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

	src, err := goabi.RenderDispatcher(reflect.TypeFor[Shapes]())
	require.NoError(t, err)
	parse(t, src)

	text := string(src)
	for _, want := range []string{
		`p1 "github.com/heynemann/go-plg/internal/goabi_test"`,
		`p2 "time"`,
		"func PlgCall_Slices(a0 p0.Context, a1 []string, a2 [][]uint8, a3 [3]int) ([]p1.Point, error) {",
		"func PlgCall_Maps(a0 p0.Context, a1 map[string]*p1.Point, a2 p2.Time) (map[string][]int, error) {",
		"func PlgCall_Anonymous(a0 p0.Context, a1 struct{ p1.Inner; *p1.Point; Name string \"json:\\\"name\\\"\" }) (error) {",
		"func PlgCall_Variadic(a0 p0.Context, a1 string, a2 ...int) (error) {\n\treturn inst.Variadic(a0, a1, a2...)\n}",
		"func PlgCall_None() () {\n\tinst.None()\n}",
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
	}

	for _, tc := range tests {
		_, err := goabi.RenderDispatcher(tc.typ)
		require.ErrorIs(t, err, goabi.ErrRender, tc.name)
		assert.ErrorContains(t, err, tc.msg, tc.name)
	}
}
