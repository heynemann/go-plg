//go:build integration

package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/cli"
	"github.com/heynemann/go-plg/internal/testproxy"
)

// pricingRule is the testdata contract every init test implements: a Host, a method of several results, times,
// pointers, maps and slices in its types, and //plg:validation.
const pricingRule = "example.com/shop/pricing.Rule@v1.0.0"

// plgIn runs plg with args from dir in the environment environ.
func plgInEnv(environ []string, dir string, args ...string) validated {
	var stdout, stderr bytes.Buffer

	code := cli.Main(args, &cli.Env{
		Stdout: &stdout, Stderr: &stderr,
		Environ: func() []string { return environ }, Dir: dir,
	})

	return validated{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// readFile reads the file name.
func readFile(name string) ([]byte, error) {
	return os.ReadFile(name) //nolint:gosec,wrapcheck // G304: the test's own files
}

// TestInitWritesPluginsThatPassValidate creates a plugin of each language implementing pricing.Rule and validates
// it unchanged: the skeleton loads, type-checks (Go and TypeScript) and passes its example test cases, one per
// method and one for Validate, in the language's real guest. A Go plugin outside any repository is a module of its
// own requiring the pinned contract module.
func TestInitWritesPluginsThatPassValidate(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	for _, lang := range []string{"go", "lua", "ts"} {
		root := t.TempDir()

		created := plgInEnv(environ, root, "init", "-implements", pricingRule, "-lang", lang, "pricing")
		require.Equal(t, 0, created.code, lang+"\n"+created.stdout+created.stderr)
		assert.Contains(t, rows(created.stdout), "interface ok pricing.Rule: Coupon, Quote, Spin, Split; Host "+
			"pricing.Customers: Tier (a stub: every call fails)", lang)
		assert.Contains(t, rows(created.stdout), "owner warning fill in plugin.yaml's owner block", lang)

		manifest, err := readFile(filepath.Join(root, "pricing", "plugin.yaml"))
		require.NoError(t, err)
		assert.Contains(t, string(manifest), "implements: "+pricingRule+"\n", lang)
		assert.Contains(t, string(manifest), "owner:\n", lang)

		if lang == "go" {
			goMod, err := readFile(filepath.Join(root, "pricing", "go.mod"))
			require.NoError(t, err)
			assert.Contains(t, string(goMod), "module pricing\n")
			assert.Contains(t, string(goMod), "require example.com/shop v1.0.0\n")
		}

		valid := plgInEnv(environ, root, "validate", "pricing")
		require.Equal(t, 0, valid.code, lang+"\n"+valid.stdout+valid.stderr)

		for _, call := range []string{"Coupon", "Quote", "Spin", "Split"} {
			assert.Contains(t, rows(valid.stdout),
				"test ok tests/example.yaml: "+call+" returns the skeleton's zero values", lang)
		}

		assert.Contains(t, rows(valid.stdout), "test ok tests/example.yaml: Validate reports no issues", lang)
		assert.Contains(t, rows(valid.stdout), "tests ok 5 passed", lang)
	}
}

// TestInitAddsThePluginToItsRepository creates a Go plugin in an author repository with a go.mod: plg.yaml lists
// it after the plugins it listed (its comments kept), the repository's go.mod gains the contract module's
// requirement, and the repository validates. A plugin directory already in use is refused, as is a pin the
// repository's go.mod requires another version of, which leaves the repository as it was.
func TestInitAddsThePluginToItsRepository(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	root := t.TempDir()

	created := plgInEnv(environ, root, "init", "-implements", pricingRule, "-lang", "lua", "rules/first")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)

	const listed = "# the team's pricing rules\nplugins:\n  - rules/first # the first one\nrequireOwnership: false\n"

	require.NoError(t, os.WriteFile(filepath.Join(root, "plg.yaml"), []byte(listed), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/team\n\ngo 1.27\n"),
		0o600))

	created = plgInEnv(environ, root, "init", "-implements", pricingRule, "-lang", "go", "rules/second")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)
	assert.Contains(t, rows(created.stdout), "plg.yaml ok lists rules/second")
	assert.NoFileExists(t, filepath.Join(root, "rules", "second", "go.mod"), "the repository's module is the plugin's")

	plgYAML, err := readFile(filepath.Join(root, "plg.yaml"))
	require.NoError(t, err)
	//nolint:testifylint // the text, comments and layout included, not only the YAML
	assert.Equal(t, "# the team's pricing rules\nplugins:\n  - rules/first # the first one\n  - rules/second\n"+
		"requireOwnership: false\n", string(plgYAML))

	goMod, err := readFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	assert.Contains(t, string(goMod), "require example.com/shop v1.0.0\n")

	valid := plgInEnv(environ, root, "validate", ".")
	require.Equal(t, 0, valid.code, valid.stdout+valid.stderr)
	assert.Contains(t, rows(valid.stdout), "rules/first ok 1 warning")
	// The owner block to fill in, and Validate's plugin.Issue: validate type-checks no import of plg's plugin package.
	assert.Contains(t, rows(valid.stdout), "rules/second ok 2 warnings")

	refused := plgInEnv(environ, root, "init", "-implements", pricingRule, "-lang", "ts", "rules/second")
	assert.Equal(t, 1, refused.code)
	assert.Contains(t, refused.stdout, "is not empty")

	appendTo(t, root, "go.mod", "require example.com/rewards v1.3.0\n")

	goMod, err = readFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)

	refused = plgInEnv(environ, root, "init", "-implements", "example.com/rewards/cohort.Provider@v1.2.0", "-lang",
		"go", "rules/third")
	assert.Equal(t, 1, refused.code, refused.stdout+refused.stderr)
	assert.Contains(t, rows(refused.stdout), "go.mod FAIL "+filepath.Join(root, "go.mod")+" requires another "+
		"version of the contract module: example.com/rewards v1.3.0, not the pin v1.2.0 (init with @v1.3.0, or go get "+
		"example.com/rewards@v1.2.0)")
	assert.NoDirExists(t, filepath.Join(root, "rules", "third"))

	unchanged, err := readFile(filepath.Join(root, "plg.yaml"))
	require.NoError(t, err)
	assert.Equal(t, string(plgYAML), string(unchanged)) //nolint:testifylint // byte for byte

	unchanged, err = readFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, string(goMod), string(unchanged))
}

// TestInitWritesEditorConfigurationForSharedDirectories gives Lua and TypeScript plugins in an author repository
// with a shared directory the editor configuration resolving it at its repository path (the Lua Language
// Server's workspace library, TypeScript's rootDirs): plg types for a plugin created before plg.yaml shared the
// directory, plg init for a new one; validate then finds them up to date. plg types never overwrites a
// .luarc.json it did not write, and writes it again once it is moved away.
func TestInitWritesEditorConfigurationForSharedDirectories(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	root := t.TempDir()

	created := plgInEnv(environ, root, "init", "-implements", pricingRule, "-lang", "lua", "rules/lua")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)

	luarc := filepath.Join(root, "rules", "lua", ".luarc.json")
	assert.NoFileExists(t, luarc, "no shared directories outside a repository")

	require.NoError(t, os.WriteFile(filepath.Join(root, "plg.yaml"), []byte("plugins: [rules/lua]\nshared: [lib]\n"),
		0o600))
	require.NoError(t, os.Mkdir(filepath.Join(root, "lib"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "lib", "tiers.lua"), []byte("return {}\n"), 0o600))

	stale := plgInEnv(environ, root, "validate", "rules/lua")
	assert.Equal(t, 1, stale.code)
	assert.Contains(t, stale.stdout, "stale: .luarc.json not what plg types writes")

	written := plgInEnv(environ, root, "types", "rules/lua")
	require.Equal(t, 0, written.code, written.stdout+written.stderr)
	assert.Contains(t, rows(written.stdout), "typings ok wrote .luarc.json; plg.d.lua already up to date")

	created = plgInEnv(environ, root, "init", "-implements", pricingRule, "-lang", "ts", "rules/ts")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)

	settings, err := readFile(luarc)
	require.NoError(t, err)
	assert.Contains(t, string(settings), `"workspace.library": ["../../lib"]`)

	tsconfig, err := readFile(filepath.Join(root, "rules", "ts", "tsconfig.json"))
	require.NoError(t, err)
	assert.Contains(t, string(tsconfig), `"rootDirs": [".", "../.."],`)

	valid := plgInEnv(environ, root, "validate", ".")
	require.Equal(t, 0, valid.code, valid.stdout+valid.stderr)

	const mine = "{\"workspace.library\": [\"elsewhere\"]}\n"

	require.NoError(t, os.WriteFile(luarc, []byte(mine), 0o600))

	refused := plgInEnv(environ, root, "types", "rules/lua")
	assert.Equal(t, 1, refused.code, refused.stdout+refused.stderr)
	assert.Contains(t, refused.stdout, "did not write is in the way")

	kept, err := readFile(luarc)
	require.NoError(t, err)
	assert.Equal(t, mine, string(kept)) //nolint:testifylint // byte for byte

	require.NoError(t, os.Remove(luarc))

	written = plgInEnv(environ, root, "types", "rules/lua")
	require.Equal(t, 0, written.code, written.stdout+written.stderr)
	assert.Contains(t, rows(written.stdout), "typings ok wrote .luarc.json; plg.d.lua already up to date")

	again, err := readFile(luarc)
	require.NoError(t, err)
	assert.Equal(t, string(settings), string(again))
}

// TestInitResolvesRelativePaths creates plugins from a subdirectory of an author repository with a go.mod and a
// shared directory, named relative to a working directory given as ".", as `plg init` run in a shell is: plg types
// writes the editor configuration of the shared directory for a plugin created before plg.yaml shared it, the Go
// plugin's module is the repository's (found above the working directory), the new Lua plugin gets the editor
// configuration too, and the repository then validates, its typings up to date.
//
//nolint:paralleltest // t.Chdir: the working directory is the process's
func TestInitResolvesRelativePaths(t *testing.T) {
	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	root := t.TempDir()

	created := plgInEnv(environ, root, "init", "-implements", pricingRule, "-lang", "lua", "rules/first")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)

	for name, content := range map[string]string{
		"plg.yaml":      "plugins: [rules/first]\nshared: [lib]\nrequireOwnership: false\n",
		"go.mod":        "module example.com/team\n\ngo 1.27\n",
		"lib/tiers.lua": "return {}\n",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(content), 0o600))
	}

	t.Chdir(filepath.Join(root, "rules"))

	written := plgInEnv(environ, ".", "types", "first")
	require.Equal(t, 0, written.code, written.stdout+written.stderr)
	assert.Contains(t, rows(written.stdout), "typings ok wrote .luarc.json; plg.d.lua already up to date")

	created = plgInEnv(environ, ".", "init", "-implements", pricingRule, "-lang", "go", "vip")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)
	assert.Contains(t, rows(created.stdout), "plg.yaml ok lists rules/vip")
	assert.NoFileExists(t, filepath.Join(root, "rules", "vip", "go.mod"), "the repository's module is the plugin's")

	goMod, err := readFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	assert.Contains(t, string(goMod), "require example.com/shop v1.0.0\n")

	created = plgInEnv(environ, ".", "init", "-implements", pricingRule, "-lang", "lua", "tier")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)

	settings, err := readFile(filepath.Join(root, "rules", "tier", ".luarc.json"))
	require.NoError(t, err)
	assert.Contains(t, string(settings), `"workspace.library": ["../../lib"]`)

	valid := plgInEnv(environ, "..", "validate", ".")
	require.Equal(t, 0, valid.code, valid.stdout+valid.stderr)
	assert.Contains(t, rows(valid.stdout), "rules/tier ok 1 warning")
}

// TestInitUsage checks plg init's arguments.
func TestInitUsage(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"init", "-lang", "go"},
		{"init", "-implements", pricingRule},
		{"init", "-implements", pricingRule, "-lang", "cobol"},
		{"init", "-implements", "example.com/shop/pricing.Rule", "-lang", "go"},
		{"init", "-implements", pricingRule, "-lang", "go", "a", "b"},
		{"init", "-bogus"},
	} {
		ran := plgInEnv(nil, t.TempDir(), args...)
		assert.Equal(t, 2, ran.code, strings.Join(args, " "))
		assert.Contains(t, ran.stderr, "plg init: ", strings.Join(args, " "))
	}
}

// TestInitRefusals fails plg init before it writes anything, each step naming why: a version the proxy does not
// have, an interface the contract does not declare, a path that is a file, an invalid plg.yaml above the plugin,
// a repository go.mod that is no go.mod; and after writing the plugin, a repository whose plugin of the same key
// keeps plg.yaml from listing it, which leaves the directory as it was (empty). A contract with several possible
// Hosts gets a plugin without one and a hint naming them; -host picks one.
func TestInitRefusals(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	root := t.TempDir()

	initIn := func(dir, implements, lang, plugin string) validated {
		t.Helper()

		return plgInEnv(environ, dir, "init", "-implements", implements, "-lang", lang, plugin)
	}

	refused := initIn(root, "example.com/rewards/cohort.Provider@v1.9.0", "lua", "nine")
	assert.Equal(t, 1, refused.code)
	assert.Contains(t, strings.Join(rows(refused.stdout), "\n"), "contract FAIL")
	assert.Contains(t, refused.stderr, "plg init: no plugin created in "+filepath.Join(root, "nine"))
	assert.NoDirExists(t, filepath.Join(root, "nine"))

	refused = initIn(root, "example.com/rewards/cohort.Nope@v1.2.0", "lua", "nope")
	assert.Equal(t, 1, refused.code)
	assert.Contains(t, strings.Join(rows(refused.stdout), "\n"), "interface FAIL")

	for _, lang := range []string{"go", "lua"} { // no plugin can implement it: its method takes no context
		refused = initIn(root, "example.com/rewards/cohort.Broken@v1.2.0", lang, "broken")
		assert.Equal(t, 1, refused.code, lang)
		assert.Contains(t, refused.stdout, "Broken", lang)
		assert.NoDirExists(t, filepath.Join(root, "broken"), lang)
	}

	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), nil, 0o600))

	refused = initIn(root, pricingRule, "lua", "file")
	assert.Equal(t, 1, refused.code)
	assert.Contains(t, strings.Join(rows(refused.stdout), "\n"), "directory FAIL")

	several := initIn(root, "example.com/rewards/v2.Provider@v2.0.0", "lua", "several")
	require.Equal(t, 0, several.code, several.stdout+several.stderr)
	assert.Contains(t, rows(several.stdout), "hint for a Create taking a Host, pass -host <Ledger|Users>")
	assert.NotContains(t, readString(t, filepath.Join(root, "several", "plg.d.lua")), "host = {}")

	chosen := plgInEnv(environ, root, "init", "-implements", "example.com/rewards/v2.Provider@v2.0.0", "-lang", "ts",
		"-host", "Users", "chosen")
	require.Equal(t, 0, chosen.code, chosen.stdout+chosen.stderr)
	assert.Contains(t, readString(t, filepath.Join(root, "chosen", "plg.d.ts")), "declare const host: rewards.Users;")

	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "a", "rule"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "b", "rule"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "plg.yaml"), []byte("plugins: [a/rule]\n"), 0o600))

	refused = initIn(repo, pricingRule, "lua", "b/rule")
	assert.Equal(t, 1, refused.code)
	assert.Contains(t, refused.stdout, "have the same key")
	assert.DirExists(t, filepath.Join(repo, "b", "rule"))

	entries, err := os.ReadDir(filepath.Join(repo, "b", "rule"))
	require.NoError(t, err)
	assert.Empty(t, entries, "the directory init found empty is left empty")
	assert.Equal(t, "plugins: [a/rule]\n", readString(t, filepath.Join(repo, "plg.yaml")))

	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("this is no go.mod\n"), 0o600))

	refused = initIn(repo, pricingRule, "go", "c")
	assert.Equal(t, 1, refused.code)
	assert.Contains(t, strings.Join(rows(refused.stdout), "\n"), "go.mod FAIL")
	assert.NoDirExists(t, filepath.Join(repo, "c"))

	require.NoError(t, os.WriteFile(filepath.Join(repo, "plg.yaml"), []byte("plugins: []\n"), 0o600))

	refused = initIn(repo, pricingRule, "lua", "d")
	assert.Equal(t, 1, refused.code)
	assert.Contains(t, strings.Join(rows(refused.stdout), "\n"), "plg.yaml FAIL")
}

// TestInitWithTheContractRequiredAlready creates a Go plugin in a repository whose go.mod already requires the
// pinned contract module: the go.mod is left as it is.
func TestInitWithTheContractRequiredAlready(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	repo := t.TempDir()
	goMod := "module example.com/team\n\ngo 1.27\n\nrequire example.com/shop v1.0.0\n"

	require.NoError(t, os.MkdirAll(filepath.Join(repo, "first"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "plg.yaml"), []byte("plugins: [first]\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte(goMod), 0o600))

	created := plgInEnv(environ, repo, "init", "-implements", pricingRule, "-lang", "go", "second")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)
	assert.NotContains(t, created.stdout, "go.mod ")
	assert.Contains(
		t,
		rows(created.stdout),
		"next hint plg validate second, or plg dev second to validate on each save",
	)
	assert.Equal(t, goMod, readString(t, filepath.Join(repo, "go.mod")))
}

// TestInitUndoesWhatItWrote fails plg init once it started writing: in a directory it cannot write, and when the
// repository's go.mod cannot be written after the plugin was written and listed, which removes the plugin and
// restores plg.yaml. A Go plugin whose directory's name is no import path gets the module path plugin.
func TestInitUndoesWhatItWrote(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	standalone := t.TempDir()
	created := plgInEnv(environ, standalone, "init", "-implements", pricingRule, "-lang", "go", "my rule")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)
	assert.Contains(t, readString(t, filepath.Join(standalone, "my rule", "go.mod")), "module plugin\n")

	if os.Geteuid() == 0 {
		t.Skip("root writes read-only files and directories")
	}

	locked := t.TempDir()
	require.NoError(t, os.Chmod(locked, 0o500))                      //nolint:gosec // G302: read-only on purpose
	t.Cleanup(func() { assert.NoError(t, os.Chmod(locked, 0o700)) }) //nolint:gosec // G302: as t.TempDir made it

	refused := plgInEnv(environ, locked, "init", "-implements", pricingRule, "-lang", "lua", "rule")
	assert.Equal(t, 1, refused.code)
	assert.Contains(t, strings.Join(rows(refused.stdout), "\n"), "files FAIL")
	assert.NoDirExists(t, filepath.Join(locked, "rule"))

	const listed = "plugins: [first]\n"

	repo := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(repo, "first"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "plg.yaml"), []byte(listed), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/team\n"), 0o400))

	refused = plgInEnv(environ, repo, "init", "-implements", pricingRule, "-lang", "go", "second")
	assert.Equal(t, 1, refused.code)
	assert.Contains(t, strings.Join(rows(refused.stdout), "\n"), "go.mod FAIL")
	assert.NoDirExists(t, filepath.Join(repo, "second"))
	assert.Equal(t, listed, readString(t, filepath.Join(repo, "plg.yaml")))
}

// readString returns the content of the file name.
func readString(t *testing.T, name string) string {
	t.Helper()

	data, err := readFile(name)
	require.NoError(t, err)

	return string(data)
}
