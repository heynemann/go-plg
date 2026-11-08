//go:build integration

package typings_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/tscheck"
	"github.com/heynemann/go-plg/internal/typings"
)

// checker compiles the TypeScript compiler through the default compilation cache, which `make warm` fills.
func checker(t *testing.T) *tscheck.Checker {
	t.Helper()

	eng := engine.New(t.Context(), engine.Options{})
	rt := eng.NewRuntime(t.Context(), engine.RuntimeOptions{MemoryLimit: tscheck.MemoryLimit})

	t.Cleanup(func() {
		assert.NoError(t, rt.Close(context.Background()))
		assert.NoError(t, eng.Close(context.Background()))
	})

	c, err := tscheck.Compile(t.Context(), rt)
	require.NoError(t, err)

	return c
}

// golden returns the golden directory's files, with plugin.ts replaced by source unless it is empty.
func golden(t *testing.T, source string) fs.FS {
	t.Helper()

	dir := os.DirFS(filepath.Join("testdata", "golden"))
	if source == "" {
		return dir
	}

	entries, err := fs.ReadDir(dir, ".")
	require.NoError(t, err)

	var files memfs.Builder

	require.NoError(t, files.Add("plugin.ts", []byte(source)))

	for _, entry := range entries {
		if entry.Name() != "plugin.ts" {
			data, readErr := fs.ReadFile(dir, entry.Name())
			require.NoError(t, readErr)
			require.NoError(t, files.Add(entry.Name(), data))
		}
	}

	built, err := files.Build()
	require.NoError(t, err)

	return built
}

// TestGoldenTypingsTypeCheck type-checks plugins against the golden declarations with the real compiler: one
// implementing every method of the contract, reading every value plg hands over and returning Dates for times,
// type-checks; one reading a field tagged json:"-", handing a Date where plg hands a time over and whose Create
// returns no Store gets the compiler's diagnostics, at their positions.
func TestGoldenTypingsTypeCheck(t *testing.T) {
	t.Parallel()

	typ, err := typings.Generate(typings.LanguageTypeScript, everything(t))
	require.NoError(t, err)

	check := checker(t)

	diags, err := typ.CheckTypeScript(t.Context(), check, golden(t, ""), "plugin.ts")
	require.NoError(t, err)
	assert.Empty(t, diags, "the plugin of every method type-checks")

	bad := `const item = {} as everything.Item;
console.log(item.Secret);
const stamped: everything.Stamped = { Prev: null, History: null, At: new Date() };
export function Create() {
  return { stamped };
}
`

	diags, err = typ.CheckTypeScript(t.Context(), check, golden(t, bad), "plugin.ts")
	require.NoError(t, err)

	got := make([]string, len(diags))
	for i := range diags {
		got[i] = diags[i].String()
	}

	require.Len(t, got, 3, got)
	assert.Equal(t, "plugin.ts:2:18: Property 'Secret' does not exist on type 'Item'. (TS2339)", got[0])
	assert.Contains(t, got[1], "plugin.ts:3:")
	assert.Contains(t, got[1], "is missing the following properties from type 'Time': seconds, nanos")
	assert.Contains(t, got[2], "plugin.ts:4:17: Create: Argument of type '() => { stamped: Stamped; }' is not "+
		"assignable to parameter of type '(...args: never[]) => Promise<Store> | Store'.")
}

// TestCheckTypeScriptRefusesLuaTypings asks Lua typings to type-check TypeScript.
func TestCheckTypeScriptRefusesLuaTypings(t *testing.T) {
	t.Parallel()

	typ, err := typings.Generate(typings.LanguageLua, everything(t))
	require.NoError(t, err)

	_, err = typ.CheckTypeScript(t.Context(), nil, golden(t, ""), "plugin.ts")
	require.ErrorIs(t, err, typings.ErrLanguage)
}

// TestTypeScriptScaffoldTypeChecks type-checks the TypeScript skeleton of a contract with several results, a
// variadic method, unnamed parameters, times, pointers, maps, a Host and a validation input against its typings
// with the real compiler: no diagnostics.
func TestTypeScriptScaffoldTypeChecks(t *testing.T) {
	t.Parallel()

	contract := shapes(t)

	typ, err := typings.Generate(typings.LanguageTypeScript, contract)
	require.NoError(t, err)

	scaffold, err := typings.NewScaffold(typings.LanguageTypeScript, contract, "shape")
	require.NoError(t, err)

	var files memfs.Builder

	for _, file := range append(typ.Files, scaffold.Files...) {
		require.NoError(t, files.Add(file.Name, file.Data))
	}

	plugin, err := files.Build()
	require.NoError(t, err)

	diags, err := typ.CheckTypeScript(t.Context(), checker(t), plugin, scaffold.Entrypoint)
	require.NoError(t, err)
	assert.Empty(t, diags, string(scaffold.Files[0].Data))
}
