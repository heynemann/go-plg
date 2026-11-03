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

// TestTestContractIsUpToDate regenerates internal/testcontract, whose generated file the plg tests run on: it
// must be exactly what is committed.
func TestTestContractIsUpToDate(t *testing.T) {
	t.Parallel()

	dir := filepath.Join("..", "testcontract")

	file, err := gen.Generate(dir, []string{"Fooer"})
	require.NoError(t, err)
	assert.Equal(t, "fooer_plg.go", file.Name)

	committed, err := fs.ReadFile(os.DirFS(dir), file.Name)
	require.NoError(t, err)
	assert.Equal(t, string(committed), string(file.Source), "run go generate ./internal/testcontract")
}

// plgStub is the part of package plg's API generated code uses. Type-checking generated code against it keeps
// these tests in process; the real plg is compiled against generated code in internal/testcontract and in the
// cmd/plg integration test.
const plgStub = `package plg

import (
	"io/fs"
	"reflect"
)

type Contract struct {
	Interface reflect.Type
	Adapter   reflect.Type
	Source    fs.FS
}

func MustRegisterContract(Contract) {}
`

type stubImporter struct {
	std types.Importer
	plg *types.Package
}

func (i stubImporter) Import(path string) (*types.Package, error) {
	if path == "github.com/heynemann/go-plg" {
		return i.plg, nil
	}

	return i.std.Import(path) //nolint:wrapcheck // test double
}

// typeCheck type-checks the Go package in dir as the compiler would: generated file included, test files
// excluded, build constraints applied.
func typeCheck(t *testing.T, dir string) *types.Package {
	t.Helper()

	fset := token.NewFileSet()
	std := importer.ForCompiler(fset, "gc", nil)

	stub, err := parser.ParseFile(fset, "plg.go", plgStub, 0)
	require.NoError(t, err)

	plg, err := (&types.Config{Importer: std}).Check("github.com/heynemann/go-plg", fset, []*ast.File{stub}, nil)
	require.NoError(t, err)

	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)

	var files []*ast.File

	for _, name := range names {
		if file := compiledFile(t, fset, name); file != nil {
			files = append(files, file)
		}
	}

	pkg, err := (&types.Config{Importer: stubImporter{std: std, plg: plg}}).Check("contract", fset, files, nil)
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

	file, err := gen.Generate(dir, ifaces)
	require.NoError(t, err)
	require.NoError(t, gen.Write(dir, file))

	again, err := gen.Generate(dir, ifaces)
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
		// adapters maps each interface to its adapter type.
		adapters map[string]string
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
			unwanted: []string{"return a.WPing(p0)"},
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
		},
		{
			name: "names the package already declares",
			files: map[string]string{
				"c.go": `package c

import ctx "context"

var context, reflect, plg, embed = 1, 2, 3, 4

type plgFooer struct{}

type plgFooerSource int

type Fooer interface {
	Foo(c ctx.Context) error
}
`,
			},
			ifaces:   []string{"Fooer"},
			adapters: map[string]string{"Fooer": "plgFooer2"},
			want: []string{
				`context2 "context"`,
				`embed2 "embed"`,
				`reflect2 "reflect"`,
				`plg2 "github.com/heynemann/go-plg"`,
				"var plgFooerSource2 embed2.FS",
				"plg2.MustRegisterContract(plg2.Contract{",
				"Interface: reflect2.TypeFor[Fooer](),",
				"Adapter:   reflect2.TypeFor[plgFooer2](),",
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
		file, err := gen.Generate(dir, tc.ifaces)
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			file, err := gen.Generate(writeFiles(t, tc.files), tc.ifaces)
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

	_, err := gen.Generate(filepath.Join(t.TempDir(), "missing"), []string{"Fooer"})
	require.ErrorIs(t, err, gen.ErrPackage)
	require.ErrorIs(t, err, os.ErrNotExist)

	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}

	dir := writeFiles(t, map[string]string{"c.go": "package c\n"})
	require.NoError(t, os.Chmod(filepath.Join(dir, "c.go"), 0))

	_, err = gen.Generate(dir, []string{"Fooer"})
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
