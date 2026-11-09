package typings_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/heynemann/go-plg/internal/typings"
)

// shapes is the testdata contract the scaffolds implement.
func shapes(t *testing.T) *typings.Contract {
	t.Helper()

	pkg := load(t, "shapes", "example.com/shapes")

	return &typings.Contract{
		Interface: named(t, pkg, "Shape"), Host: named(t, pkg, "Canvas"), Validation: named(t, pkg, "Limits"),
		Implements: "example.com/shapes.Shape@v1.0.0",
	}
}

// clashes is the testdata contract whose names clash.
func clashes(t *testing.T) *typings.Contract {
	t.Helper()

	pkg := load(t, "clashes", "example.com/clashes")

	return &typings.Contract{Interface: named(t, pkg, "Lease"), Implements: "example.com/clashes.Lease@v1.0.0"}
}

// typeCheckGo type-checks the Go skeleton source against the contract's package, the packages its types come from
// and plg's plugin package.
func typeCheckGo(t *testing.T, contract *typings.Contract, source []byte) *types.Package {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "plugin.go", source, 0)
	require.NoError(t, err)

	sourceImporter := importer.ForCompiler(fset, "source", nil)
	contractPkg := contract.Interface.Obj().Pkg()

	conf := types.Config{Importer: importerFunc(func(path string) (*types.Package, error) {
		if path == contractPkg.Path() {
			return contractPkg, nil
		}

		for _, imported := range contractPkg.Imports() { // the very packages the contract's types come from
			if imported.Path() == path {
				return imported, nil
			}
		}

		return sourceImporter.Import(path) // plg's plugin package
	})}

	pkg, err := conf.Check("example.com/"+file.Name.Name, fset, []*ast.File{file}, nil)
	require.NoError(t, err, string(source))

	return pkg
}

// importerFunc adapts a function to types.Importer.
type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// TestGoScaffoldImplementsTheInterface writes the Go skeleton of a contract with a variadic method, unnamed
// parameters, one named like an import, a Host and a validation input, and type-checks it against the contract: it compiles, Create returns the interface taking the Host, and the skeleton's type implements the
// interface and has Validate(context.Context, Limits) ([]plugin.Issue, error).
func TestGoScaffoldImplementsTheInterface(t *testing.T) {
	t.Parallel()

	contract := shapes(t)

	scaffold, err := typings.NewScaffold(typings.LanguageGo, contract, "my-store")
	require.NoError(t, err)
	require.Equal(t, "plugin.go", scaffold.Entrypoint)
	require.Equal(
		t,
		[]string{"plugin.go", typings.ScaffoldTests},
		[]string{scaffold.Files[0].Name, scaffold.Files[1].Name},
	)

	pkg := typeCheckGo(t, contract, scaffold.Files[0].Data)
	assert.Equal(t, "mystore", pkg.Name())

	create, ok := pkg.Scope().Lookup("Create").Type().(*types.Signature)
	require.True(t, ok)
	assert.True(t, types.Identical(contract.Host, create.Params().At(0).Type()))
	assert.True(t, types.Identical(contract.Interface, create.Results().At(0).Type()))

	impl := types.NewPointer(pkg.Scope().Lookup("shape").Type())
	iface, ok := contract.Interface.Underlying().(*types.Interface)
	require.True(t, ok)
	assert.True(t, types.Implements(impl, iface))

	validate, _, _ := types.LookupFieldOrMethod(impl, true, pkg, "Validate")
	require.NotNil(t, validate)
	assert.Equal(t, "func(ctx context.Context, in example.com/shapes.Limits) "+
		"([]github.com/heynemann/go-plg/plugin.Issue, error)", validate.Type().String())
}

// TestGoScaffoldNamesNoParameterLikeAPackage writes the Go skeleton of a contract whose method is the first to use
// time and has a parameter named time: the parameter is unnamed, so the method's time.Time{} is the package's
// and the skeleton compiles and implements the interface.
func TestGoScaffoldNamesNoParameterLikeAPackage(t *testing.T) {
	t.Parallel()

	contract := clashes(t)

	scaffold, err := typings.NewScaffold(typings.LanguageGo, contract, "lease")
	require.NoError(t, err)

	source := string(scaffold.Files[0].Data)
	assert.Contains(t, source, "Expiry(ctx context.Context, _ time.Duration) (time.Time, error) {\n"+
		"\treturn time.Time{}, nil\n}", source)

	pkg := typeCheckGo(t, contract, scaffold.Files[0].Data)

	iface, ok := contract.Interface.Underlying().(*types.Interface)
	require.True(t, ok)
	assert.True(t, types.Implements(types.NewPointer(pkg.Scope().Lookup("lease").Type()), iface))
}

// exampleFile is what a scaffold's test case file holds.
type exampleFile struct {
	Cases []exampleCase `yaml:"cases"`
}

type exampleCase struct {
	Input  map[string]any `yaml:"input"`
	Expect map[string]any `yaml:"expect"`
	Name   string         `yaml:"name"`
	Call   string         `yaml:"call"`
}

// TestScaffoldTestsExpectTheSkeletonsZeroValues reads the example test cases of each language's scaffold: a case
// per method, in the interface's order, calling it with every parameter (unnamed ones as p<i>) and expecting its
// results' zero values (a list of several), a time being Go's zero time in Go and the Unix epoch in Lua and TypeScript, whose skeletons
// return seconds and nanos of zero; and Validate expecting no issues.
func TestScaffoldTestsExpectTheSkeletonsZeroValues(t *testing.T) {
	t.Parallel()

	for language, zeroTime := range map[string]string{
		typings.LanguageGo: "0001-01-01T00:00:00Z", typings.LanguageLua: "1970-01-01T00:00:00Z",
		typings.LanguageTypeScript: "1970-01-01T00:00:00Z",
	} {
		scaffold, err := typings.NewScaffold(language, shapes(t), "shape")
		require.NoError(t, err)

		var file exampleFile

		tests := string(scaffold.Files[1].Data)
		require.NoError(t, yaml.Unmarshal([]byte(tests), &file), tests)

		calls := make([]string, len(file.Cases))
		for i, tc := range file.Cases {
			calls[i] = tc.Call
		}

		assert.Equal(t, []string{"Area", "Corners", "Find", "Keywords", "Label", "Validate"}, calls, language)

		unnamed := file.Cases[4]
		assert.Equal(t, map[string]any{"p1": "", "p2": 0}, unnamed.Input, language)
		assert.Equal(t, map[string]any{"result": ""}, unnamed.Expect, language)
		assert.Equal(t, map[string]any{"result": []any{nil, false}}, file.Cases[2].Expect, language)
		assert.Equal(t, map[string]any{"result": []any{nil, 0}}, file.Cases[1].Expect, language)
		assert.Nil(t, file.Cases[3].Expect, "Keywords returns only its error: nothing to expect")
		assert.Equal(t, map[string]any{"issues": []any{}}, file.Cases[5].Expect, language)
		assert.Contains(t, tests, "result: {at: '"+zeroTime+"', unit: null, raw: null, nested: {note: \"\", X: 0, "+
			"Y: 0}, value: 0.0}", language)
	}

	lua, err := typings.NewScaffold(typings.LanguageLua, shapes(t), "shape")
	require.NoError(t, err)

	source := string(lua.Files[0].Data)
	assert.Contains(t, source, "function Shape:Corners(rest)\n  return { nil, 0 }\nend\n")
	assert.Contains(t, source, "function Shape:Keywords(function_, end_, time)\nend\n")
	assert.Contains(t, source, "return { at = { seconds = 0, nanos = 0 }, unit = nil, raw = nil, nested = { note = "+
		"\"\", X = 0, Y = 0 }, value = 0.0 }\n")
	assert.Contains(t, source, "function Shape:Validate(input)\n  return nil\nend\n")
	assert.True(t, strings.HasSuffix(source, "function Create()\n  return setmetatable({}, Shape)\nend\n"))
}

// TestScaffoldRefusesOtherLanguages asks for the skeleton of a language plg does not run.
func TestScaffoldRefusesOtherLanguages(t *testing.T) {
	t.Parallel()

	_, err := typings.NewScaffold("cobol", shapes(t), "shape")
	require.ErrorIs(t, err, typings.ErrLanguage)
}
