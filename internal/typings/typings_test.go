package typings_test

import (
	"flag"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/typings"
)

//nolint:gochecknoglobals // the test binary's flag
var update = flag.Bool("update", false, "rewrite the golden files from the generator's output")

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// load type-checks the testdata package in dir under the import path pkgPath.
func load(t *testing.T, dir, pkgPath string) *types.Package {
	t.Helper()

	fset := token.NewFileSet()

	entries, err := os.ReadDir(filepath.Join("testdata", dir))
	require.NoError(t, err)

	files := make([]*ast.File, 0, len(entries))

	for _, entry := range entries {
		file, parseErr := parser.ParseFile(fset, filepath.Join("testdata", dir, entry.Name()), nil,
			parser.ParseComments)
		require.NoError(t, parseErr)

		files = append(files, file)
	}

	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", nil)}
	pkg, err := conf.Check(pkgPath, fset, files, nil)
	require.NoError(t, err)

	return pkg
}

// named returns the type pkg declares as name.
func named(t *testing.T, pkg *types.Package, name string) *types.Named {
	t.Helper()

	typ, ok := pkg.Scope().Lookup(name).Type().(*types.Named)
	require.True(t, ok, name)

	return typ
}

// everything is the testdata contract whose methods use every data type plg carries.
func everything(t *testing.T) *typings.Contract {
	t.Helper()

	pkg := load(t, "everything", "example.com/everything")

	return &typings.Contract{
		Interface: named(t, pkg, "Store"), Host: named(t, pkg, "Directory"), Validation: named(t, pkg, "Limits"),
		Implements: "example.com/everything.Store@v1.4.0",
	}
}

// TestGeneratesTheGoldenTypings generates the TypeScript and Lua typings of a contract using every data type and
// compares them with the golden files, which are the product: the integration test type-checks a TypeScript
// plugin against them and the Lua one parses the annotations. Run with -update to rewrite them.
func TestGeneratesTheGoldenTypings(t *testing.T) {
	t.Parallel()

	for _, language := range []string{typings.LanguageTypeScript, typings.LanguageLua} {
		typ, err := typings.Generate(language, everything(t), nil)
		require.NoError(t, err)

		goldens := filepath.Join("testdata", "golden")

		for _, file := range typ.Files {
			if *update {
				require.NoError(t, os.WriteFile(filepath.Join(goldens, file.Name), file.Data, 0o600))
			}

			want, err := fs.ReadFile(os.DirFS(goldens), file.Name)
			require.NoError(t, err)
			assert.Equal(t, string(want), string(file.Data), "%s: run go test ./internal/typings -update", file.Name)
		}
	}
}

// TestNamespacesAvoidTheGlobals generates the typings of a contract package named json, a global of the Lua guest:
// its namespace becomes json_, in TypeScript and Lua alike, and refers to its types by it.
func TestNamespacesAvoidTheGlobals(t *testing.T) {
	t.Parallel()

	pkg := load(t, "json", "example.com/json")
	contract := &typings.Contract{Interface: named(t, pkg, "Codec"), Implements: "example.com/json.Codec@v1.0.0"}

	ts, err := typings.Generate(typings.LanguageTypeScript, contract, nil)
	require.NoError(t, err)

	declarations := string(ts.Files[0].Data)
	assert.Contains(t, declarations, "declare namespace json_ {\n")
	assert.Contains(t, declarations, "    Encode(value: json_.Value): plg.Result<string>;\n")
	assert.NotContains(t, declarations, "declare const host", "a contract without a Host declares no host global")
	assert.NotContains(t, declarations, "Validate?", "a contract without a validation input declares no Validate")
	assert.NotContains(t, declarations, "\n// Host: ")

	lua, err := typings.Generate(typings.LanguageLua, contract, nil)
	require.NoError(t, err)

	annotations := string(lua.Files[0].Data)
	assert.Contains(
		t,
		annotations,
		"---@class json_.Codec\n---@field Encode fun(self: json_.Codec, value: json_.Value): "+
			"string\n",
	)
	assert.NotContains(t, annotations, "\nhost = {}")
}

// TestRecursiveTypesReachTheirTimes hands back a type that refers to itself before its time.Time, by value and
// through a pointer, in both method orders: the plugin may return a Date in either, so both take plg.Out.
func TestRecursiveTypesReachTheirTimes(t *testing.T) {
	t.Parallel()

	pkg := load(t, "recursive", "example.com/recursive")

	for _, name := range []string{"ValueFirst", "PointerFirst"} {
		contract := &typings.Contract{Interface: named(t, pkg, name), Implements: "example.com/recursive." + name}

		typ, err := typings.Generate(typings.LanguageTypeScript, contract, nil)
		require.NoError(t, err)

		declarations := string(typ.Files[0].Data)
		assert.Contains(t, declarations, "): plg.Result<plg.Out<recursive.A>>;\n", name)
		assert.Contains(t, declarations, "): plg.Result<plg.Out<recursive.A | null>>;\n", name)
	}
}

// TestParameterNamesStayDistinct generates the typings of a contract with a parameter named end, a keyword, beside
// one named end_: end becomes end__, so the two stay distinct, in TypeScript and Lua alike.
func TestParameterNamesStayDistinct(t *testing.T) {
	t.Parallel()

	ts, err := typings.Generate(typings.LanguageTypeScript, clashes(t), nil)
	require.NoError(t, err)
	assert.Contains(t, string(ts.Files[0].Data), "    Get(end__: string, end_: number): plg.Result<void>;\n")

	lua, err := typings.Generate(typings.LanguageLua, clashes(t), nil)
	require.NoError(t, err)
	assert.Contains(t, string(lua.Files[0].Data),
		"---@field Get fun(self: clashes.Lease, end__: string, end_: integer)\n")
}

// TestGenerateRefusesWhatTypingsCannotDescribe generates typings for Go, for a Host that is no interface, an
// interface whose method takes no context, and one carrying a channel.
func TestGenerateRefusesWhatTypingsCannotDescribe(t *testing.T) {
	t.Parallel()

	contract := everything(t)

	_, err := typings.Generate("go", contract, nil)
	require.ErrorIs(t, err, typings.ErrLanguage)
	require.EqualError(t, err, "typings: no typings for this language: go (Go plugins import the contract package "+
		"itself)")

	pkg := types.NewPackage("example.com/odd", "odd")
	store, ok := contract.Interface.Underlying().(*types.Interface)
	require.True(t, ok)

	ctx := types.NewParam(token.NoPos, nil, "ctx", store.Method(0).Signature().Params().At(0).Type())
	errType := types.Universe.Lookup("error").Type()

	iface := func(name string, params ...*types.Var) *types.Named {
		sig := types.NewSignatureType(nil, nil, nil, types.NewTuple(params...),
			types.NewTuple(types.NewParam(token.NoPos, nil, "", errType)), false)
		method := types.NewFunc(token.NoPos, pkg, "Do", sig)

		return types.NewNamed(types.NewTypeName(token.NoPos, pkg, name, nil),
			types.NewInterfaceType([]*types.Func{method}, nil).Complete(), nil)
	}

	notInterface := types.NewNamed(types.NewTypeName(token.NoPos, pkg, "Plain", nil), types.Typ[types.Int], nil)
	noContext := iface("NoContext", types.NewParam(token.NoPos, nil, "s", types.Typ[types.String]))
	channel := iface("Channel", ctx,
		types.NewParam(token.NoPos, nil, "c", types.NewChan(types.SendRecv, types.Typ[types.Int])))

	for _, tc := range []struct {
		contract *typings.Contract
		want     string
	}{
		{&typings.Contract{Interface: contract.Interface, Host: notInterface}, "Plain is not an interface"},
		{&typings.Contract{Interface: noContext}, "a method of NoContext does not take a context first and " +
			"return an error last"},
		{&typings.Contract{Interface: channel}, "method Do: parameter 1: "},
	} {
		_, err := typings.Generate(typings.LanguageTypeScript, tc.contract, nil)
		require.ErrorIs(t, err, typings.ErrContract, tc.want)
		assert.Contains(t, err.Error(), tc.want)
	}
}

// TestTypingsOnDisk writes typings into a plugin directory and reads them back: what is stale, which Host they
// were written for, and when Write leaves a file alone or refuses to touch it.
func TestTypingsOnDisk(t *testing.T) {
	t.Parallel()

	typ, err := typings.Generate(typings.LanguageTypeScript, everything(t), nil)
	require.NoError(t, err)

	dir := t.TempDir()
	assert.Equal(t, []string{"plg.d.ts", "tsconfig.json"}, typ.Names())
	assert.Equal(t, []string{"plg.d.ts", "tsconfig.json"}, typ.Stale(os.DirFS(dir)), "missing files are stale")
	assert.Empty(t, typings.HostIn(typings.LanguageTypeScript, os.DirFS(dir)), "no typings, no Host")

	written, err := typ.Write(dir)
	require.NoError(t, err)
	assert.Equal(t, []typings.Written{{Name: "plg.d.ts", Changed: true}, {Name: "tsconfig.json", Changed: true}},
		written)
	assert.Empty(t, typ.Stale(os.DirFS(dir)))
	assert.Equal(t, "Directory", typings.HostIn(typings.LanguageTypeScript, os.DirFS(dir)))
	assert.Empty(t, typings.HostIn(typings.LanguageLua, os.DirFS(dir)), "no Lua typings there")
	assert.Empty(t, typings.HostIn("go", os.DirFS(dir)))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte("// "+
		"Code generated by plg types. DO NOT EDIT.\n{}\n"), 0o600))
	assert.Equal(t, []string{"tsconfig.json"}, typ.Stale(os.DirFS(dir)))

	written, err = typ.Write(dir)
	require.NoError(t, err)
	assert.Equal(t, []typings.Written{{Name: "plg.d.ts"}, {Name: "tsconfig.json", Changed: true}}, written,
		"a generated file is rewritten, an identical one left alone")
	assert.Empty(t, typ.Stale(os.DirFS(dir)))

	require.NoError(t, os.Remove(filepath.Join(dir, "plg.d.ts")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "plg.d.ts"), 0o700))

	_, err = typ.Write(dir)
	require.Error(t, err, "a directory in the way cannot be read")
	assert.NotErrorIs(t, err, typings.ErrNotGenerated)
}

// TestGeneratedHeaders tells generated files by their header among the first three lines, and reads the Host
// line of the header only.
func TestGeneratedHeaders(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		data, host string
		generated  bool
	}{
		{"// Code generated by plg types. DO NOT EDIT.\n// Implements: x\n// Host: Users\n//\n", "Users", true},
		{"---@meta\n-- Code generated by plg types. DO NOT EDIT.\n-- Host: Ledger\n", "Ledger", true},
		{"// Code generated by plg types. DO NOT EDIT.\n// Implements: x\n\n// Host: Users\n", "", true},
		{"// Code generated by plg types. DO NOT EDIT.\nconst x = 1; // Host: Users\n", "", true},
		{"// Host: Users\n// Code generated by plg types. DO NOT EDIT.\n", "Users", true},
		{"//\n//\n//\n// Code generated by plg types. DO NOT EDIT.\n// Host: Users\n", "", false},
		{"// Code generated by plg gen. DO NOT EDIT.\n// Host: Users\n", "", false},
		{"", "", false},
	} {
		assert.Equal(t, tc.generated, typings.Generated([]byte(tc.data)), tc.data)
		assert.Equal(t, tc.host, typings.HostOf([]byte(tc.data)), tc.data)
	}
}
