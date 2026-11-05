package gen_test

import (
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/contractsrc"
	"github.com/heynemann/go-plg/internal/gen"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestCommittedContractsAreUpToDate regenerates internal/testcontract, whose generated file the plg tests run
// on, and internal/fastcontract, whose generated codecs they hold to the reflective codec: each must be exactly
// what is committed.
func TestCommittedContractsAreUpToDate(t *testing.T) {
	t.Parallel()

	for pkg, ifaces := range map[string][]string{
		"testcontract": {"Fooer", "Shaper", "Hosted", "Aliased", "Family"},
		"fastcontract": {"Kinds"},
	} {
		dir := filepath.Join("..", pkg)

		file, err := gen.Generate(dir, ifaces, gen.Options{})
		require.NoError(t, err)
		assert.Equal(t, strings.ToLower(ifaces[0])+"_plg.go", file.Name)

		committed, err := fs.ReadFile(os.DirFS(dir), file.Name)
		require.NoError(t, err)
		assert.Equal(t, string(committed), string(file.Source), "run go generate ./%s", dir)
	}
}

// TestGenerateGenericLeavesTheFastPathsOut: with Options.Generic the file has only the generic adapters, as
// plg gen generated before fast paths existed.
func TestGenerateGenericLeavesTheFastPathsOut(t *testing.T) {
	t.Parallel()

	dir := writeFiles(t, map[string]string{"c.go": `package c

import "context"

type Fooer interface {
	Foo(ctx context.Context, s string) (string, error)
}
`})

	file, err := gen.Generate(dir, []string{"Fooer"}, gen.Options{Generic: true})
	require.NoError(t, err)
	require.NoError(t, gen.Write(dir, file))

	pkg := typeCheck(t, dir)
	requireAdapter(t, pkg, "Fooer", "plgFooer")
	assert.NotContains(t, string(file.Source), "plgimpl")
	assert.NotContains(t, string(file.Source), "Fast")
	assert.Nil(t, pkg.Scope().Lookup("plgFooerFast"))

	fast, err := gen.Generate(dir, []string{"Fooer"}, gen.Options{})
	require.NoError(t, err)
	assert.Contains(t, string(fast.Source), "Fast:")
}

// plgStub and plgimplStub are the parts of the plg and plgimpl packages' API generated code uses. Type-checking
// generated code against them keeps these tests in process; the real packages are compiled against generated
// code in internal/testcontract, internal/fastcontract and the cmd/plg integration test.
const (
	plgStub = `package plg

import (
	"io/fs"
	"reflect"

	"github.com/heynemann/go-plg/plgimpl"
)

type Contract struct {
	Interface reflect.Type
	Adapter   reflect.Type
	Source    fs.FS
	Fast      func(plgimpl.Invoker) any
	Layout    string
}

func MustRegisterContract(Contract) {}
`
	plgimplStub = `package plgimpl

import (
	"context"
	"time"
)

type Reader struct{}

type Call interface {
	AppendArgs(b []byte) ([]byte, error)
	DecodeResults(r *Reader) error
}

type Invoker interface {
	Invoke(ctx context.Context, method int, call Call) error
}

type (
	Signed interface{ ~int | ~int8 | ~int16 | ~int32 | ~int64 }
	Unsigned interface{ ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 }
)

type (
	Bool    = bool
	Byte    = byte
	Error   = error
	Float64 = float64
	Int     = int
	Int64   = int64
	String  = string
	Uint64  = uint64
)

func AppendBool(b []byte, v bool) []byte                               { return b }
func AppendInt(b []byte, v int64) ([]byte, error)                      { return b, nil }
func AppendUint(b []byte, v uint64) ([]byte, error)                    { return b, nil }
func AppendFloat(b []byte, v float64) []byte                           { return b }
func AppendString(b []byte, v string) ([]byte, error)                  { return b, nil }
func AppendBytes(b, v []byte) ([]byte, error)                          { return b, nil }
func AppendByteSlice[S ~[]E, E ~uint8](b []byte, v S) ([]byte, error) { return b, nil }
func AppendTime(b []byte, t time.Time) ([]byte, error)                 { return b, nil }
func AppendNil(b []byte) []byte                                        { return b }
func AppendLen(b []byte, n, depth int) ([]byte, error)               { return b, nil }
func Deeper(depth int) error                                           { return nil }
func ReadBool[T ~bool](r *Reader) (T, error)                           { return false, nil }
func ReadInt[T Signed](r *Reader, typeName string) (T, error)          { return 0, nil }
func ReadUint[T Unsigned](r *Reader, typeName string) (T, error)       { return 0, nil }
func ReadFloat32[T ~float32](r *Reader, typeName string) (T, error)    { return 0, nil }
func ReadFloat64[T ~float64](r *Reader) (T, error)                     { return 0, nil }
func ReadString[T ~string](r *Reader) (T, error)                       { return "", nil }
func ReadBytes[S ~[]byte](r *Reader) (S, error)                        { return nil, nil }
func ReadByteSlice[S ~[]E, E ~uint8](r *Reader) (S, error)             { return nil, nil }
func ReadTime(r *Reader) (time.Time, error)                            { return time.Time{}, nil }
func ReadSliceLen[S ~[]E, E any](r *Reader, _ *S, depth, minSize int) (int, error) { return 0, nil }
func ReadMapLen[M ~map[K]V, K ~string, V any](r *Reader, _ *M, depth, minSize int) (int, error) { return 0, nil }
func ReadKey[M ~map[K]V, K ~string, V any](r *Reader, _ M) (K, error) { return "", nil }
func ReadPresent[P ~*E, E any](r *Reader, _ *P, depth int) (bool, error) { return false, nil }
func DuplicateKeys() error                                             { return nil }
func AtField(err error, goName string) error                           { return err }
func AtIndex(err error, i int) error                                   { return err }
func AtKey(err error, k string) error                                  { return err }
func Param(err error, k int) error                                     { return err }
func Result(err error, k int) error                                    { return err }
`
)

// stubImporter imports the plg and plgimpl stubs, and the standard library.
type stubImporter struct {
	std  types.Importer
	pkgs map[string]*types.Package
}

func (i stubImporter) Import(path string) (*types.Package, error) {
	if pkg, ok := i.pkgs[path]; ok {
		return pkg, nil
	}

	return i.std.Import(path) //nolint:wrapcheck // test double
}

// typeCheck type-checks the Go package in dir as the compiler would: generated file included, test files
// excluded, build constraints applied.
func typeCheck(t *testing.T, dir string) *types.Package {
	t.Helper()

	fset := token.NewFileSet()
	imports := stubImporter{std: importer.ForCompiler(fset, "gc", nil), pkgs: map[string]*types.Package{}}

	for _, stub := range []struct{ path, src string }{
		{"github.com/heynemann/go-plg/plgimpl", plgimplStub},
		{"github.com/heynemann/go-plg", plgStub},
	} {
		file, err := parser.ParseFile(fset, filepath.Base(stub.path)+".go", stub.src, 0)
		require.NoError(t, err)

		imports.pkgs[stub.path], err = (&types.Config{Importer: imports}).Check(stub.path, fset, []*ast.File{file}, nil)
		require.NoError(t, err)
	}

	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)

	var files []*ast.File

	for _, name := range names {
		if file := compiledFile(t, fset, name); file != nil {
			files = append(files, file)
		}
	}

	pkg, err := (&types.Config{Importer: imports}).Check("contract", fset, files, nil)
	require.NoError(t, err)

	return pkg
}

// compiledFile parses the Go file at path if the compiler would build it here, and returns nil otherwise.
func compiledFile(t *testing.T, fset *token.FileSet, path string) *ast.File {
	t.Helper()

	if strings.HasSuffix(path, "_test.go") {
		return nil
	}

	match, err := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path))
	require.NoError(t, err)

	if !match {
		return nil
	}

	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	require.NoError(t, err)

	return file
}

// requireAdapter checks what RegisterContract checks at run time, on the generated adapter of iface: one
// W<Method> field per method, of the method's type, and a pointer that implements the interface.
func requireAdapter(t *testing.T, pkg *types.Package, iface, adapter string) {
	t.Helper()

	ifaceObj, ok := pkg.Scope().Lookup(iface).(*types.TypeName)
	require.True(t, ok, iface)

	adapterObj, ok := pkg.Scope().Lookup(adapter).(*types.TypeName)
	require.True(t, ok, adapter)

	ifaceType, ok := ifaceObj.Type().Underlying().(*types.Interface)
	require.True(t, ok)

	structType, ok := adapterObj.Type().Underlying().(*types.Struct)
	require.True(t, ok)
	require.Equal(t, ifaceType.NumMethods(), structType.NumFields(), "one field per method")

	for method := range ifaceType.Methods() {
		field, _, _ := types.LookupFieldOrMethod(adapterObj.Type(), false, pkg, "W"+method.Name())
		fieldVar, ok := field.(*types.Var)
		require.True(t, ok, "field W%s", method.Name())
		assert.True(t, types.Identical(fieldVar.Type(), method.Type()), "W%s is %s, method is %s", method.Name(),
			fieldVar.Type(), method.Type())
	}

	assert.True(t, types.Implements(types.NewPointer(adapterObj.Type()), ifaceType),
		"*%s implements %s", adapter, iface)
}

// requireImplements requires the pointer to the generated type adapter to implement the interface iface.
func requireImplements(t *testing.T, pkg *types.Package, iface, adapter string) {
	t.Helper()

	ifaceObj, ok := pkg.Scope().Lookup(iface).(*types.TypeName)
	require.True(t, ok, iface)

	adapterObj, ok := pkg.Scope().Lookup(adapter).(*types.TypeName)
	require.True(t, ok, adapter)

	ifaceType, ok := ifaceObj.Type().Underlying().(*types.Interface)
	require.True(t, ok)
	assert.True(
		t,
		types.Implements(types.NewPointer(adapterObj.Type()), ifaceType),
		"*%s implements %s",
		adapter,
		iface,
	)
}

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, src := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
	}

	return dir
}

// generate runs plg gen in dir and writes its output, checking that running it again changes nothing.
func generate(t *testing.T, dir string, ifaces ...string) gen.File {
	t.Helper()

	file, err := gen.Generate(dir, ifaces, gen.Options{})
	require.NoError(t, err)
	require.NoError(t, gen.Write(dir, file))

	again, err := gen.Generate(dir, ifaces, gen.Options{})
	require.NoError(t, err)
	assert.Equal(t, string(file.Source), string(again.Source), "regenerating next to its own output is a no-op")

	return file
}

func TestGenerate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		files  map[string]string
		ifaces []string
		// adapters maps each interface to its adapter type, fast to its fast-path adapter type.
		adapters, fast map[string]string
		// want and unwanted are lines of the generated file, compared without their indentation.
		want, unwanted []string
	}{
		{
			name: "parameter shapes",
			files: map[string]string{"c.go": `package c

import "context"

type Shaper interface {
	Unnamed(context.Context, string) (string, error)
	Join(ctx context.Context, sep string, parts ...string) (string, error)
	Ping(ctx context.Context)
	One(ctx context.Context) error
	Pair(ctx context.Context) (a, b int)
}
`},
			ifaces:   []string{"Shaper"},
			adapters: map[string]string{"Shaper": "plgShaper"},
			want: []string{
				"WJoin    func(context.Context, string, ...string) (string, error)",
				"return a.WJoin(p0, p1, p2...)",
				"a.WPing(p0)",
				"func (a *plgShaper) Pair(p0 context.Context) (int, int) {",
				"func (a *plgShaper) One(p0 context.Context) error {",
				"var _ Shaper = (*plgShaper)(nil)",
				"//go:embed *.go",
				"var plgShaperSource embed.FS",
				"Source:    plgShaperSource,",
			},
			unwanted: []string{"return a.WPing(p0)", "plgimpl", "Fast:"}, // Ping breaks the contract: no fast paths
		},
		{
			name: "types of the package and of allowlisted packages",
			files: map[string]string{
				"c.go": `package c

import (
	"context"
	"math/big"
	mrand "math/rand"
	"math/rand/v2"
	"time"
)

type Box[T any] struct{ V T }

type Celsius float64

type Typed interface {
	Get(ctx context.Context, at time.Time, n *big.Int, r *mrand.Rand, p *rand.PCG) (Box[Celsius], error)
	Map(ctx context.Context, m map[string][]time.Duration, s struct{ A int ` + "`json:\"a\"`" + ` }) error
}
`,
			},
			ifaces:   []string{"Typed"},
			adapters: map[string]string{"Typed": "plgTyped"},
			want: []string{
				`"math/rand"`,
				`rand2 "math/rand/v2"`,
				`"math/big"`,
				"WGet func(context.Context, time.Time, *big.Int, *rand.Rand, *rand2.PCG) (Box[Celsius], error)",
				"WMap func(context.Context, map[string][]time.Duration, struct {",
			},
			unwanted: []string{"Fast:"}, // *big.Int has unexported fields: no fast paths
		},
		{
			name: "fast paths",
			files: map[string]string{"c.go": `package c

import (
	"context"
	"time"
)

type Code string

type Octet byte

type Point struct {
	X, Y  int
	Label Code ` + "`json:\"label\"`" + `
	Skip  bool ` + "`json:\"-\"`" + `
	Near  *Point
	When  time.Time
	Raw   []byte
	Bits  []Octet
	Tags  map[Code][]float32
}

type Fooer interface {
	Move(ctx context.Context, p Point, by ...int) (*Point, error)
	Many(ctx context.Context) (uint8, int64, float32, bool, error)
	Ping(ctx context.Context) error
}
`},
			ifaces:   []string{"Fooer"},
			adapters: map[string]string{"Fooer": "plgFooer"},
			fast:     map[string]string{"Fooer": "plgFooerFast"},
			want: []string{
				`"github.com/heynemann/go-plg/plgimpl"`,
				"var _ Fooer = (*plgFooerFast)(nil)",
				"Fast:      func(inv plgimpl.Invoker) any { return &plgFooerFast{inv: inv} },",
				"c := plgFooerMoveCall{p1: p1, p2: p2}",
				"return a.inv.Invoke(p0, 2, &c)",
				"func plgEncodePoint(b []byte, v *Point, depth int) ([]byte, error) {",
				"func plgDecodePoint(r *plgimpl.Reader, v *Point, depth int) error {",
				`v.Label, err = plgimpl.ReadString[Code](r)`,
				`c.r2, err = plgimpl.ReadFloat32[float32](r, "float32")`,
				"b, err = plgimpl.AppendByteSlice(b, v.Bits)",
				"v.Bits, err = plgimpl.ReadByteSlice[[]Octet](r)",
				"b, err = plgimpl.AppendBytes(b, []byte(v.Raw))",
				`return plgimpl.AtField(err, "Near")`,
			},
			unwanted: []string{"Skip"},
		},
		{
			name: "no fast paths for what the codec cannot encode",
			files: map[string]string{"c.go": `package c

import "context"

type hidden struct{ a int }

type Clash struct {
	A int ` + "`json:\"x\"`" + `
	B int ` + "`json:\"x\"`" + `
}

type (
	Chans    interface{ M(ctx context.Context, c chan int) error }
	IntKeys  interface{ M(ctx context.Context, m map[int]string) error }
	Arrays   interface{ M(ctx context.Context) ([2]int, error) }
	Hidden   interface{ M(ctx context.Context, h hidden) error }
	Clashing interface{ M(ctx context.Context, c []Clash) error }
	Empty    interface{ M(ctx context.Context, e []struct{}) error }
	NoCtx    interface{ M(s string) error }
	NoErr    interface{ M(ctx context.Context) string }
	NoResult interface{ M(ctx context.Context) }
)
`},
			ifaces: []string{"Chans", "IntKeys", "Arrays", "Hidden", "Clashing", "Empty", "NoCtx", "NoErr", "NoResult"},
			adapters: map[string]string{
				"Chans": "plgChans", "IntKeys": "plgIntKeys", "Arrays": "plgArrays", "Hidden": "plgHidden",
				"Clashing": "plgClashing", "Empty": "plgEmpty", "NoCtx": "plgNoCtx", "NoErr": "plgNoErr",
				"NoResult": "plgNoResult",
			},
			unwanted: []string{"plgimpl", "Fast:", "Layout:"},
		},
		{
			name: "names the package already declares",
			files: map[string]string{
				"c.go": `package c

import ctx "context"

var context, reflect, plg, embed, plgimpl = 1, 2, 3, 4, 5

var b, v, r, depth, err, c, inv = 1, 2, 3, 4, 5, 6, 7

type plgFooer struct{}

type plgFooerFast struct{}

type Point struct{ X int }

type plgEncodePoint int

type plgFooerSource int

type Fooer interface {
	Foo(c ctx.Context, p Point) (Point, error)
}
`,
			},
			ifaces:   []string{"Fooer"},
			adapters: map[string]string{"Fooer": "plgFooer2"},
			fast:     map[string]string{"Fooer": "plgFooerFast2"},
			want: []string{
				`context2 "context"`,
				`embed2 "embed"`,
				`reflect2 "reflect"`,
				`plg2 "github.com/heynemann/go-plg"`,
				`plgimpl2 "github.com/heynemann/go-plg/plgimpl"`,
				"var plgFooerSource2 embed2.FS",
				"plg2.MustRegisterContract(plg2.Contract{",
				"Interface: reflect2.TypeFor[Fooer](),",
				"Adapter:   reflect2.TypeFor[plgFooer2](),",
				"Fast:      func(inv plgimpl2.Invoker) any { return &plgFooerFast2{inv2: inv} },",
				"func plgEncodePoint2(b2 []byte, v2 *Point, depth2 int) ([]byte, error) {",
			},
		},
		{
			name: "several interfaces, embedded, aliased and unexported",
			files: map[string]string{
				"c.go": `package c

import "context"

type Base interface {
	Base(ctx context.Context) error
}

type Fooer interface {
	Base
	Foo(ctx context.Context) error
}

type Alias = Base

type barer interface {
	bar(ctx context.Context) error
}
`,
			},
			ifaces:   []string{"Fooer", "Alias", "barer"},
			adapters: map[string]string{"Fooer": "plgFooer", "Alias": "plgAlias", "barer": "plgbarer"},
			want: []string{
				"WBase func(context.Context) error",
				"Wbar func(context.Context) error",
				"Interface: reflect.TypeFor[Alias](),",
				"Interface: reflect.TypeFor[barer](),",
				"Source:    plgFooerSource,",
			},
			unwanted: []string{"plgAliasSource", "plgbarerSource"},
		},
		{
			name: "files the guest does not receive",
			files: map[string]string{
				"c.go": `package c

import "context"

type Fooer interface {
	Foo(ctx context.Context) error
}
`,
				// They may import anything: plg itself (stubbed by typeCheck), here.
				"c_test.go":    "package c\n\nimport \"github.com/heynemann/go-plg\"\n\nvar _ = plg.MustRegisterContract\n",
				"other_plg.go": contractsrc.Header + "\n\npackage c\n\nimport \"github.com/heynemann/go-plg\"\n\nvar _ plg.Contract\n",
				"README.md":    "not Go",
			},
			ifaces:   []string{"Fooer"},
			adapters: map[string]string{"Fooer": "plgFooer"},
		},
		{
			name: "files of other platforms are checked, not compiled",
			files: map[string]string{
				"c.go": `package c

import "context"

type Fooer interface {
	Foo(ctx context.Context) error
}
`,
				"ignored.go": "//go:build ignore\n\npackage c\n\nimport \"strings\"\n\ntype Fooer = strings.Builder\n",
				"_skip.go":   "package c\n\nimport \"strings\"\n\ntype Fooer = strings.Builder\n",
			},
			ifaces:   []string{"Fooer"},
			adapters: map[string]string{"Fooer": "plgFooer"},
		},
		{
			name: "redeclared predeclared types",
			files: map[string]string{"c.go": `package c

import (
	"context"
	"encoding/json"
	"time"
)

type (
	int     = uint16
	any     = int
	bool    = int
	byte    = int
	float64 = int
	int64   = int
	string  = int
	uint64  = int
)

type Rec struct {
	N int8
	U uint32
	F float32
	B []uint8
	M map[json.Number]*Rec
	S []Rec
	T time.Time
	J json.Number
	X any
}

type Svc interface {
	Do(ctx context.Context, r Rec, n json.Number, x any) (*Rec, error)
}
`},
			ifaces:   []string{"Svc"},
			adapters: map[string]string{"Svc": "plgSvc"},
			fast:     map[string]string{"Svc": "plgSvcFast"},
			want: []string{
				"Fast:      func(inv plgimpl.Invoker) interface{} { return &plgSvcFast{inv: inv} },",
				"func (c *plgSvcDoCall) AppendArgs(b []plgimpl.Byte) ([]plgimpl.Byte, error) {",
				"func plgEncode(b []plgimpl.Byte, v *map[json.Number]*Rec, depth plgimpl.Int) ([]plgimpl.Byte, error) {",
			},
		},
		{
			name: "redeclared error",
			files: map[string]string{"c.go": `package c

import "context"

type error interface{ Error() int }

type Fooer interface {
	Foo(ctx context.Context) error
}
`},
			ifaces:   []string{"Fooer"},
			adapters: map[string]string{"Fooer": "plgFooer"},
			unwanted: []string{"plgimpl", "Fast:"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := writeFiles(t, tc.files)
			src := string(generate(t, dir, tc.ifaces...).Source)
			pkg := typeCheck(t, dir)

			assert.True(t, strings.HasPrefix(src, contractsrc.Header+"\n"), "the generated file is marked")
			assert.True(t, contractsrc.IsGenerated([]byte(src)))

			for iface, adapter := range tc.adapters {
				requireAdapter(t, pkg, iface, adapter)
			}

			for iface, adapter := range tc.fast {
				requireImplements(t, pkg, iface, adapter)
			}

			lines := map[string]bool{}
			for line := range strings.Lines(src) {
				lines[strings.TrimSpace(line)] = true
			}

			for _, line := range tc.want {
				assert.True(t, lines[line], "generated file lacks the line %q:\n%s", line, src)
			}

			for _, line := range tc.unwanted {
				assert.NotContains(t, src, line)
			}
		})
	}
}

func TestGenerateNamesTheFileAfterTheFirstInterface(t *testing.T) {
	t.Parallel()

	dir := writeFiles(t, map[string]string{"c.go": `package c

import "context"

type HTTPFooer interface{ Foo(ctx context.Context) error }

type Barer interface{ Bar(ctx context.Context) error }
`})

	for _, tc := range []struct {
		want   string
		ifaces []string
	}{
		{ifaces: []string{"HTTPFooer"}, want: "httpfooer_plg.go"},
		{ifaces: []string{"Barer", "HTTPFooer"}, want: "barer_plg.go"},
	} {
		file, err := gen.Generate(dir, tc.ifaces, gen.Options{})
		require.NoError(t, err)
		assert.Equal(t, tc.want, file.Name)
	}
}

func TestGenerateErrors(t *testing.T) {
	t.Parallel()

	const fooer = "package c\n\nimport \"context\"\n\ntype Fooer interface{ Foo(ctx context.Context) error }\n"

	tests := []struct {
		name   string
		files  map[string]string
		ifaces []string
		err    error
		msg    []string
	}{
		{name: "no interface", files: map[string]string{"c.go": fooer}, err: gen.ErrInterfaceName},
		{
			name: "not an identifier", files: map[string]string{"c.go": fooer}, ifaces: []string{"-o"},
			err: gen.ErrInterfaceName, msg: []string{`"-o" is not a Go identifier`},
		},
		{
			name: "qualified name", files: map[string]string{"c.go": fooer}, ifaces: []string{"c.Fooer"},
			err: gen.ErrInterfaceName,
		},
		{
			name: "listed twice", files: map[string]string{"c.go": fooer}, ifaces: []string{"Fooer", "Fooer"},
			err: gen.ErrInterfaceName, msg: []string{"Fooer is listed twice"},
		},
		{name: "empty directory", ifaces: []string{"Fooer"}, err: gen.ErrPackage, msg: []string{"no Go source"}},
		{
			name: "only test files", files: map[string]string{"c_test.go": fooer}, ifaces: []string{"Fooer"},
			err: gen.ErrPackage, msg: []string{"no Go source"},
		},
		{
			name: "no file for this platform", ifaces: []string{"Fooer"}, err: gen.ErrPackage,
			files: map[string]string{"c.go": "//go:build ignore\n\n" + fooer},
		},
		{
			name:   "disallowed imports",
			ifaces: []string{"Fooer"},
			files: map[string]string{
				"c.go":     fooer,
				"io.go":    "package c\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nvar _, _ = fmt.Sprint, os.Args\n",
				"other.go": "//go:build ignore\n\npackage c\n\nimport _ \"github.com/heynemann/go-plg\"\n",
				"cgo.go":   "package c\n\nimport \"C\"\n",
			},
			err: gen.ErrImport,
			msg: []string{
				"\tcgo.go:3:8: \"C\"\n\tio.go:5:2: \"os\"\n\tother.go:5:10: \"github.com/heynemann/go-plg\"\n",
				"allowed: bufio bytes cmp context",
			},
		},
		{
			name: "syntax error", ifaces: []string{"Fooer"}, err: gen.ErrPackage, msg: []string{"bad.go:3:6"},
			files: map[string]string{"c.go": fooer, "bad.go": "package c\n\nfunc {\n"},
		},
		{
			name: "malformed build constraint", ifaces: []string{"Fooer"}, err: gen.ErrPackage,
			files: map[string]string{"c.go": fooer, "bad.go": "//go:build (linux\n\npackage c\n"},
		},
		{
			name: "type error", ifaces: []string{"Fooer"}, err: gen.ErrPackage, msg: []string{"undefined: Missing"},
			files: map[string]string{"c.go": fooer, "bad.go": "package c\n\nvar _ Missing\n"},
		},
		{
			name: "two package names", ifaces: []string{"Fooer"}, err: gen.ErrPackage,
			files: map[string]string{"c.go": fooer, "d.go": "package d\n"},
		},
		{
			name: "undeclared", files: map[string]string{"c.go": fooer}, ifaces: []string{"Barer"},
			err: gen.ErrInterface, msg: []string{"package c declares no type Barer"},
		},
		{
			name: "not a type", ifaces: []string{"Barer"}, err: gen.ErrInterface,
			files: map[string]string{"c.go": fooer + "\nfunc Barer() {}\n"},
		},
		{
			name:   "not an interface",
			ifaces: []string{"Barer"},
			err:    gen.ErrInterface,
			msg:    []string{"Barer is a struct"},
			files:  map[string]string{"c.go": fooer + "\ntype Barer struct{}\n"},
		},
		{
			name:   "generic",
			ifaces: []string{"Barer"},
			err:    gen.ErrInterface,
			msg:    []string{"Barer is generic"},
			files: map[string]string{
				"c.go": fooer + "\ntype Barer[T any] interface{ Bar(ctx context.Context) (T, error) }\n",
			},
		},
		{
			name:   "generic alias",
			ifaces: []string{"Barer"},
			err:    gen.ErrInterface,
			msg:    []string{"Barer is generic"},
			files: map[string]string{
				"c.go": fooer + "\ntype I[T any] interface{ Bar(T) }\n\ntype Barer[T any] = I[T]\n",
			},
		},
		{
			name: "constraint", ifaces: []string{"Barer"}, err: gen.ErrInterface, msg: []string{"type constraint"},
			files: map[string]string{"c.go": fooer + "\ntype Barer interface{ ~int; Bar() }\n"},
		},
		{
			name: "W field clashing with a method", ifaces: []string{"Barer"}, err: gen.ErrInterface,
			msg:   []string{"Barer has methods Bar and WBar"},
			files: map[string]string{"c.go": fooer + "\ntype Barer interface{ Bar(); WBar() }\n"},
		},
		{
			name: "second interface invalid", ifaces: []string{"Fooer", "Barer"}, err: gen.ErrInterface,
			files: map[string]string{"c.go": fooer},
		},
		{
			name: "nil redeclared", ifaces: []string{"Fooer"}, err: gen.ErrPredeclared,
			msg:   []string{"package c declares nil"},
			files: map[string]string{"c.go": fooer + "\nvar nil = 0\n"},
		},
		{
			name: "builtins the fast paths use redeclared", ifaces: []string{"Fooer"}, err: gen.ErrPredeclared,
			msg:   []string{"package c declares len, make"},
			files: map[string]string{"c.go": fooer + "\nfunc len() {}\n\nvar make = 0\n"},
		},
		{
			name:   "redeclared predeclared type in an embedded method",
			ifaces: []string{"Reader"},
			err:    gen.ErrPredeclared,
			msg:    []string{"Reader.Read mentions the predeclared int, which package c redeclares"},
			files: map[string]string{
				"c.go": "package c\n\nimport \"io\"\n\ntype int = string\n\ntype Reader interface{ io.Reader }\n",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			file, err := gen.Generate(writeFiles(t, tc.files), tc.ifaces, gen.Options{})
			require.ErrorIs(t, err, tc.err)
			assert.Equal(t, gen.File{}, file)

			for _, msg := range tc.msg {
				assert.ErrorContains(t, err, msg)
			}
		})
	}
}

func TestGenerateUnreadablePackage(t *testing.T) {
	t.Parallel()

	_, err := gen.Generate(filepath.Join(t.TempDir(), "missing"), []string{"Fooer"}, gen.Options{})
	require.ErrorIs(t, err, gen.ErrPackage)
	require.ErrorIs(t, err, os.ErrNotExist)

	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}

	dir := writeFiles(t, map[string]string{"c.go": "package c\n"})
	require.NoError(t, os.Chmod(filepath.Join(dir, "c.go"), 0))

	_, err = gen.Generate(dir, []string{"Fooer"}, gen.Options{})
	require.ErrorIs(t, err, gen.ErrPackage)
	require.ErrorIs(t, err, os.ErrPermission)
}

func TestWrite(t *testing.T) {
	t.Parallel()

	generated := []byte(contractsrc.Header + "\n\npackage c\n")
	updated := []byte(contractsrc.Header + "\n\npackage c\n\n// updated\n")
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	t.Run("new file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, gen.Write(dir, gen.File{Name: "c_plg.go", Source: generated}))

		got, err := fs.ReadFile(os.DirFS(dir), "c_plg.go")
		require.NoError(t, err)
		assert.Equal(t, generated, got)
	})

	t.Run("replaces its own output", func(t *testing.T) {
		t.Parallel()

		dir := writeFiles(t, map[string]string{"c_plg.go": string(generated)})
		require.NoError(t, gen.Write(dir, gen.File{Name: "c_plg.go", Source: updated}))

		got, err := fs.ReadFile(os.DirFS(dir), "c_plg.go")
		require.NoError(t, err)
		assert.Equal(t, updated, got)
	})

	t.Run("leaves an identical file untouched", func(t *testing.T) {
		t.Parallel()

		dir := writeFiles(t, map[string]string{"c_plg.go": string(generated)})
		target := filepath.Join(dir, "c_plg.go")
		require.NoError(t, os.Chtimes(target, old, old))
		require.NoError(t, gen.Write(dir, gen.File{Name: "c_plg.go", Source: generated}))

		info, err := os.Stat(target)
		require.NoError(t, err)
		assert.True(t, info.ModTime().Equal(old), "not rewritten, so builds stay cached")
	})

	t.Run("refuses a hand-written file", func(t *testing.T) {
		t.Parallel()

		dir := writeFiles(t, map[string]string{"c_plg.go": "package c\n"})
		err := gen.Write(dir, gen.File{Name: "c_plg.go", Source: generated})
		require.ErrorIs(t, err, gen.ErrNotGenerated)

		got, err := fs.ReadFile(os.DirFS(dir), "c_plg.go")
		require.NoError(t, err)
		assert.Equal(t, "package c\n", string(got))
	})

	t.Run("unreadable target", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "c_plg.go"), 0o750))
		require.ErrorContains(t, gen.Write(dir, gen.File{Name: "c_plg.go", Source: generated}), "reading")
	})

	t.Run("missing directory", func(t *testing.T) {
		t.Parallel()

		err := gen.Write(filepath.Join(t.TempDir(), "missing"), gen.File{Name: "c_plg.go", Source: generated})
		require.ErrorIs(t, err, os.ErrNotExist)
		require.ErrorContains(t, err, "writing")
	})
}
