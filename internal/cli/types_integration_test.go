//go:build integration

package cli_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/cli"
	"github.com/heynemann/go-plg/internal/testproxy"
)

// plgTypes runs plg types with args in the environment environ, from the testdata's plugins directory.
func plgTypes(environ []string, args ...string) validated {
	var stdout, stderr bytes.Buffer

	code := cli.Main(append([]string{"types"}, args...), &cli.Env{
		Stdout: &stdout, Stderr: &stderr, Getenv: func(string) string { return "" },
		Environ: func() []string { return environ }, Dir: filepath.Join("..", "..", "testdata", "validate", "plugins"),
	})

	return validated{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// pluginCopy copies the testdata plugin into a directory of the test's own and returns it.
func pluginCopy(t *testing.T, plugin string) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), plugin)
	require.NoError(t, os.CopyFS(dir, os.DirFS(abs(t, plugin))))

	return dir
}

// TestValidateTypeChecksTypeScriptPlugins validates TypeScript plugins whose code does not type-check against
// their typings: each fails at the types step with the compiler's diagnostics at their file:line:column, and the
// plugin is not loaded. What Create returns is checked against the interface even when nothing in the plugin
// says it implements it.
func TestValidateTypeChecksTypeScriptPlugins(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	tests := []struct {
		plugin string
		want   []string
	}{
		{"tswrongresult", []string{ // IsMember returns a string; Create's result is not declared a cohort.Provider
			"types      FAIL    plugin.ts:13:17: Create: Argument of type '() => Tiers' is not assignable to " +
				"parameter of type '(...args: never[]) => Promise<Provider> | Provider'.",
			"The types returned by 'IsMember(...)' are incompatible between these types. Type 'Promise<string>' is " +
				"not assignable to type 'Result<boolean>'. Type 'Promise<string>' is not assignable to type " +
				"'Promise<boolean>'. Type 'string' is not assignable to type 'boolean'. (TS2345)\n",
		}},
		{"tsunknownhost", []string{ // host.Rank: the Host cohort.Users has Tier only
			"types      FAIL    plugin.ts:4:51: Property 'Rank' does not exist on type 'Users'. (TS2339)\n",
		}},
		{"tsimplements", []string{ // the class says it implements cohort.Provider: the error is at its List
			"types      FAIL    plugin.ts:7:3: Property 'List' in type 'Gold' is not assignable to the same " +
				"property in base type 'Provider'.",
			"Types of property 'next' are incompatible. Type 'number' is not assignable to type 'string'. (TS2416)\n",
			"types      FAIL    plugin.ts:12:17: Create: Argument of type '() => Gold' is not assignable",
		}},
	}

	for _, tc := range tests {
		got := validate(environ, tc.plugin)
		assert.Equal(t, 1, got.code, "%s\n%s%s", tc.plugin, got.stdout, got.stderr)
		assert.Contains(t, got.stdout, "typings    ok      plg.d.ts, tsconfig.json up to date\n", tc.plugin)

		for _, want := range tc.want {
			assert.Contains(t, got.stdout, want, tc.plugin)
		}

		assert.NotContains(t, got.stdout, "\nload ", "%s: a plugin that does not type-check is not loaded", tc.plugin)
	}

	own := validate(environ, "tsimplements").stdout
	assert.Less(t, strings.Index(own, "plugin.ts:7:3"), strings.Index(own, "plugin.ts:12:17"),
		"the plugin's own diagnostics come before those about what Create returns")
}

// TestValidateFailsOnStaleTypings validates copies of good Lua and TypeScript plugins whose typings are missing,
// edited, or written for another version of the contract: the typings step fails naming the stale files and the
// plg types command line to run, which then makes the plugin pass.
func TestValidateFailsOnStaleTypings(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	tests := []struct {
		spoil               func(t *testing.T, dir string)
		name, plugin, stale string
	}{
		{func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(dir, "plg.d.lua")))
		}, "missing", "luagood", "plg.d.lua"},
		{func(t *testing.T, dir string) {
			t.Helper()
			appendTo(t, dir, "tsconfig.json", "\n")
		}, "edited", "tsgood", "tsconfig.json"},
		{func(t *testing.T, dir string) {
			t.Helper()
			appendTo(t, dir, "plg.d.ts", "declare const extra: number;\n")
			appendTo(t, dir, "tsconfig.json", " ")
		}, "both edited", "tsgood", "plg.d.ts, tsconfig.json"},
		{func(t *testing.T, dir string) {
			t.Helper()
			replaceIn(t, dir, "plugin.yaml", "@v1.2.0", "@v1.3.0")
		}, "another contract version", "tsgood", "plg.d.ts"},
	}

	for _, tc := range tests {
		dir := pluginCopy(t, tc.plugin)
		tc.spoil(t, dir)

		got := validate(environ, dir)
		assert.Equal(t, 1, got.code, "%s\n%s%s", tc.name, got.stdout, got.stderr)
		assert.Contains(t, got.stdout, "typings    FAIL    typings: stale: "+tc.stale+" not what plg types writes "+
			"for example.com/rewards/cohort.Provider@v1.", tc.name)
		assert.Contains(t, got.stdout, ": run plg types -host Users "+dir+"\n", tc.name)

		written := plgTypes(environ, "-host", "Users", dir)
		require.Equal(t, 0, written.code, "%s\n%s%s", tc.name, written.stdout, written.stderr)
		assert.Contains(t, written.stdout, "typings    ok      wrote "+tc.stale, tc.name)

		got = validate(environ, dir)
		assert.NotContains(t, got.stdout, "typings    FAIL", "%s\n%s", tc.name, got.stdout)
		assert.Contains(t, got.stdout, "typings    ok      ", tc.name)
	}
}

// TestTypesWritesTypings writes typings with plg types: into a plugin without them, its Host taken from -host, then
// again (nothing changes), and from the Host recorded in the existing typings when -host is absent. A file plg
// types did not write is never overwritten, and a Go plugin gets none.
func TestTypesWritesTypings(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	dir := pluginCopy(t, "tsv2users")
	for _, name := range []string{"plg.d.ts", "tsconfig.json"} {
		require.NoError(t, os.Remove(filepath.Join(dir, name)))
	}

	got := plgTypes(environ, dir, "-host", "Users")
	require.Equal(t, 0, got.code, got.stdout+got.stderr)
	assert.Contains(t, got.stdout, "interface  ok      rewards.Provider: IsMember; Host rewards.Users: Tier")
	assert.Contains(t, got.stdout, "typings    ok      wrote plg.d.ts, tsconfig.json\n")

	for _, name := range []string{"plg.d.ts", "tsconfig.json"} {
		want, err := fs.ReadFile(os.DirFS(abs(t, "tsv2users")), name)
		require.NoError(t, err)

		written, err := fs.ReadFile(os.DirFS(dir), name)
		require.NoError(t, err)
		assert.Equal(t, string(want), string(written), "%s is the committed testdata's", name)
	}

	again := plgTypes(environ, dir)
	require.Equal(t, 0, again.code, again.stdout+again.stderr)
	assert.Contains(t, again.stdout, "Host rewards.Users: Tier",
		"without -host, the Host is the one the typings were written for")
	assert.Contains(t, again.stdout, "typings    ok      plg.d.ts, tsconfig.json already up to date\n")

	own := pluginCopy(t, "tsgood")
	require.NoError(t, os.Remove(filepath.Join(own, "plg.d.ts")))
	require.NoError(t, os.Remove(filepath.Join(own, "tsconfig.json")))
	appendTo(t, own, "tsconfig.json", "{}\n")

	refused := plgTypes(environ, own)
	assert.Equal(t, 1, refused.code, refused.stdout+refused.stderr)
	assert.Contains(t, refused.stdout, "typings    FAIL    typings: a file plg types did not write is in the way: "+
		filepath.Join(own, "tsconfig.json")+": move it away, then run plg types again\n")
	assert.Contains(t, refused.stderr, "plg types: no typings written for "+own+"\n")
	assert.NoFileExists(t, filepath.Join(own, "plg.d.ts"), "nothing is written when a file is in the way")

	kept, err := fs.ReadFile(os.DirFS(own), "tsconfig.json")
	require.NoError(t, err)
	assert.Equal(t, "{}\n", string(kept))

	goPlugin := plgTypes(environ, "gogood")
	assert.Equal(t, 1, goPlugin.code)
	assert.Contains(t, goPlugin.stdout, "typings    FAIL    typings: no typings for this language: go (Go plugins "+
		"import the contract package itself)\n")

	failed := plgTypes(environ, "unknowncontract")
	assert.Equal(t, 1, failed.code)
	assert.Contains(t, failed.stdout, "contract  FAIL    no module provides the contract package")
}

// TestLuaTypingsAreLua runs a Lua plugin whose source starts with its typings, plg.d.lua: the annotations file is
// valid Lua, which the Lua guest loads and runs, so the language server, which reads Lua, reads it too.
func TestLuaTypingsAreLua(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	dir := pluginCopy(t, "luagood")

	annotations, err := fs.ReadFile(os.DirFS(dir), "plg.d.lua")
	require.NoError(t, err)

	source, err := fs.ReadFile(os.DirFS(dir), "plugin.lua")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, "plugin.lua")))
	appendTo(t, dir, "plugin.lua", string(annotations)+string(source))

	got := validate(environ, dir)
	require.Equal(t, 0, got.code, got.stdout+got.stderr)
	assert.Contains(t, got.stdout, "load       ok      Create ran in the lua guest (version ")
}

// TestTypingsDeclareTheValidationInput: the typings of a plugin of a contract with //plg:validation Limits declare
// Validate taking pricing.Limits in both languages, and a TypeScript plugin whose Validate takes another input
// fails the type-check at its Validate.
func TestTypingsDeclareTheValidationInput(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	for plugin, want := range map[string]string{
		"tspricing":  "Validate?(input: pricing.Limits): plg.Result<plg.Issue[] | null | undefined>;",
		"luapricing": "---@field Validate? fun(self: pricing.Rule, input: pricing.Limits):",
	} {
		dir := pluginCopy(t, plugin)
		name := map[string]string{"tspricing": "plg.d.ts", "luapricing": "plg.d.lua"}[plugin]
		require.NoError(t, os.Remove(filepath.Join(dir, name)))

		got := plgTypes(environ, dir)
		require.Equal(t, 0, got.code, got.stdout+got.stderr)

		written, err := fs.ReadFile(os.DirFS(dir), name)
		require.NoError(t, err)
		assert.Contains(t, string(written), want, plugin)
		assert.Contains(t, string(written), "maxPercent", plugin)
	}

	dir := pluginCopy(t, "tspricing")
	replaceIn(t, dir, "plugin.ts", "Validate(limits: pricing.Limits)", "Validate(limits: { maxPercent: string })")

	got := validate(environ, dir)
	assert.Equal(t, 1, got.code, got.stdout+got.stderr)
	assert.Contains(t, got.stdout, "types      FAIL    plugin.ts:")
	assert.Contains(t, got.stdout, "Property 'Validate' in type 'Rule' is not assignable to the same property in "+
		"base type 'Rule'.")
}

func TestTypesUsage(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{nil, {"a", "b"}, {"-nope", "a"}} {
		got := plgTypes(nil, args...)
		assert.Equal(t, 2, got.code, args)
		assert.Contains(t, got.stderr, "plg types: ", args)
	}
}

// appendTo appends text to the file name of the directory dir, creating it if needed.
func appendTo(t *testing.T, dir, name, text string) {
	t.Helper()

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)

	f, err := root.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)

	_, err = f.WriteString(text)
	require.NoError(t, errors.Join(err, f.Close(), root.Close()))
}

// replaceIn replaces the first old in the file name of the directory dir by replacement.
func replaceIn(t *testing.T, dir, name, old, replacement string) {
	t.Helper()

	data, err := fs.ReadFile(os.DirFS(dir), name)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, name)))
	appendTo(t, dir, name, strings.Replace(string(data), old, replacement, 1))
}
