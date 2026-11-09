//go:build integration

package cli_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/cli"
	"github.com/heynemann/go-plg/internal/testproxy"
)

// validated is what one plg validate run printed and its exit code.
type validated struct {
	stdout, stderr string
	code           int
}

// validate runs plg validate with args in the environment environ, from the testdata's plugins directory.
func validate(environ []string, args ...string) validated {
	var stdout, stderr bytes.Buffer

	code := cli.Main(append([]string{"validate"}, args...), &cli.Env{
		Stdout: &stdout, Stderr: &stderr,
		Environ: func() []string { return environ }, Dir: filepath.Join("..", "..", "testdata", "validate", "plugins"),
	})

	return validated{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// modules is the testdata's contract modules: example.com/rewards v1.2.0, and v1.3.0 whose cohort.Provider adds
// Size.
func modules() string {
	return filepath.Join("..", "..", "testdata", "validate", "modules")
}

// TestValidateLoadsGoodPluginsOfEveryLanguage validates a plugin of each language implementing the testdata's
// contract, resolved through a file-based GOPROXY into a module cache of the test's own: each step passes, the
// contract module is the pinned one, a Go plugin's Host is named by its Create and its go.mod pin checked, and
// Create runs in the language's real guest. Validating again with GOPROXY=off works from the module cache, and
// -cache-dir holds the compiled guest's cache entries, which validating again with it reuses, compiling nothing.
func TestValidateLoadsGoodPluginsOfEveryLanguage(t *testing.T) {
	t.Parallel()

	cache := t.TempDir()
	environ := testproxy.Environ(testproxy.New(t, modules()), cache)
	module := "module example.com/rewards v1.2.0 (" + filepath.Join(cache, "example.com", "rewards@v1.2.0") + ")"

	tests := []struct {
		dir  string
		want []string
	}{
		{
			"gogood",
			[]string{
				"manifest   ok      go plugin gogood implementing example.com/rewards/cohort.Provider@v1.2.0\n",
				"contract   ok      " + module + "\n",
				"interface  ok      cohort.Provider: IsMember, List; Host cohort.Users: Tier (a stub: every call fails)\n",
				"go.mod     ok      " + filepath.Join(
					abs(t, "gogood"),
					"go.mod",
				) + " requires example.com/rewards v1.2.0\n",
				"sandbox    ok      no go statements, and imports only what the Go guest provides\n" +
					"types      ok      the plugin's package type-checks against cohort.Provider\n",
				"load       ok      Create ran in the go guest (version ",
			},
		},
		{"luagood", []string{ // the contract's only interface that passes as a Host is the stub Host
			"manifest   ok      lua plugin luagood implementing example.com/rewards/cohort.Provider@v1.2.0\n",
			"contract   ok      " + module + "\n",
			"interface  ok      cohort.Provider: IsMember, List; Host cohort.Users: Tier (a stub: every call fails)\n",
			"typings    ok      plg.d.lua up to date\nsyntax     ok      every .lua file compiles\n",
			"load       ok      Create ran in the lua guest (version ",
		}},
		{"tsgood", []string{
			"manifest   ok      typescript plugin tsgood implementing example.com/rewards/cohort.Provider@v1.2.0\n",
			"typings    ok      plg.d.ts, tsconfig.json up to date\n",
			"types      ok      the plugin type-checks against cohort.Provider (plg.d.ts)\n",
			"load       ok      Create ran in the typescript guest (version ",
		}},
		{"godotimport", []string{ // Create(users Users), the contract dot-imported
			"interface  ok      cohort.Provider: IsMember, List; Host cohort.Users: Tier (a stub: every call fails)\n",
			"load       ok      Create ran in the go guest (version ",
		}},
		{"goaliasimport", []string{ // Create(users rewards.Users); entrypoint "."; go.mod replaces v1.2.0 by itself
			"interface  ok      cohort.Provider: IsMember, List; Host cohort.Users: Tier (a stub: every call fails)\n",
			"go.mod     ok      " + filepath.Join(abs(t, "goaliasimport"), "go.mod") +
				" requires example.com/rewards v1.2.0\n",
			"load       ok      Create ran in the go guest (version ",
		}},
		{"gov2", []string{ // Create(users rewards.Users), importing example.com/rewards/v2 without a name
			"contract   ok      module example.com/rewards/v2 v2.0.0 (",
			"interface  ok      rewards.Provider: IsMember; Host rewards.Users: Tier (a stub: every call fails)\n",
			"types      ok      the plugin's package type-checks against rewards.Provider\n",
			"load       ok      Create ran in the go guest (version ",
		}},
		{"goskipped", []string{ // the files the guest skips declare another Host, import os and start goroutines
			"interface  ok      cohort.Provider: IsMember, List; Host cohort.Users: Tier (a stub: every call fails)\n",
			"sandbox    ok      no go statements, and imports only what the Go guest provides\n" +
				"types      ok      the plugin's package type-checks against cohort.Provider\n",
			"load       ok      Create ran in the go guest (version ",
		}},
	}

	for _, tc := range tests {
		got := validate(environ, tc.dir)
		require.Equal(t, 0, got.code, "%s\n%s%s", tc.dir, got.stdout, got.stderr)

		for _, want := range tc.want {
			assert.Contains(t, got.stdout, want, tc.dir)
		}

		assert.NotContains(t, got.stdout, "warning", "%s: its owner is complete", tc.dir)
		assert.Contains(t, got.stdout, "tests      ok      no test cases (tests/*.yaml)\n", tc.dir)
	}

	warned := []struct {
		dir, want string
	}{
		{
			"noowner",
			"           warning  plg.ownership.missing (owner): plugin.yaml's owner lacks team.name and one of " +
				"contact.oncall, contact.channel, contact.email\n",
		},
		{"goreplace", "go.mod     warning  " + filepath.Join(abs(t, "goreplace"), "go.mod") + " replaces " +
			"example.com/rewards by the directory " + filepath.Join(filepath.Dir(abs(t, "goreplace")), "..",
			"modules", "example.com", "rewards@v1.2.0") + ": the pin v1.2.0 is not checked against it\n"},
	}

	for _, tc := range warned {
		got := validate(environ, tc.dir)
		require.Equal(t, 0, got.code, "%s: a warning does not fail validate\n%s%s", tc.dir, got.stdout, got.stderr)
		assert.Contains(t, got.stdout, tc.want, tc.dir)
		assert.Contains(t, got.stdout, " guest (version ", "%s: the plugin loaded", tc.dir)
	}

	offline := validate(testproxy.Environ("off", cache), "gogood")
	require.Equal(t, 0, offline.code, "the module cache serves the contract offline\n%s%s", offline.stdout,
		offline.stderr)
	assert.Contains(t, offline.stdout, "contract   ok      "+module+"\n")

	cacheDir := t.TempDir()
	cached := validate(environ, "luagood", "-cache-dir", cacheDir)
	require.Equal(t, 0, cached.code, "%s%s", cached.stdout, cached.stderr)

	compiled := cacheFiles(t, cacheDir)
	require.NotEmpty(t, compiled, "-cache-dir is where the guest is compiled to")

	for name := range compiled {
		rel, err := filepath.Rel(cacheDir, name)
		require.NoError(t, err)
		assert.Regexp(t, `^wazero-[^/]+/[0-9a-f]{64}$`, filepath.ToSlash(rel), "a compiled module's cache entry")
	}

	cached = validate(environ, "luagood", "-cache-dir", cacheDir)
	require.Equal(t, 0, cached.code, "%s%s", cached.stdout, cached.stderr)
	assert.Equal(t, compiled, cacheFiles(t, cacheDir), "the second run compiles nothing new")
}

// TestValidateFailsEachBrokenPlugin validates broken plugins: each fails at its step with a message naming the
// problem, and plg validate exits 1 without running the later steps. A plugin pinning v1.3.0 is checked against
// v1.3.0, whose interface has a method more.
func TestValidateFailsEachBrokenPlugin(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"bad manifest", []string{"badmanifest"}, []string{
			"manifest  FAIL    plg: invalid plugin.yaml: implements is required",
		}},
		{"unknown contract module", []string{"unknowncontract"}, []string{
			"contract  FAIL    no module provides the contract package example.com/nosuch/cohort at v1.0.0: ",
			"/example.com/nosuch/cohort/@v/v1.0.0.info: no such file or directory; ",
			"/example.com/nosuch/@v/v1.0.0.info: no such file or directory; ",
		}},
		{"unfetchable version", []string{"unknownversion"}, []string{
			"contract  FAIL    no module provides the contract package example.com/rewards/cohort at v1.9.0: ",
			"/example.com/rewards/@v/v1.9.0.info: no such file or directory",
		}},
		{"missing interface", []string{"missinginterface"}, []string{
			"interface  FAIL    not an interface plg can adapt: package cohort declares no type Segmenter\n",
		}},
		{"failing Create", []string{"createfails"}, []string{
			"load       FAIL    plugin Create failed: no cohort configured\n",
		}},
		{"Host called by Create, the contract's only Host candidate", []string{"hostincreate"}, []string{
			"interface  ok      cohort.Provider: IsMember, List; Host cohort.Users: Tier (a stub: every call fails)\n",
			"load       FAIL    plugin Create failed: plg validate runs plugins without the service's Host: " +
				"cohort.Users.Tier was called\n",
		}},
		{"Host called by Create, the contract having several Host candidates", []string{"tsv2hosts"}, []string{
			"interface  ok      rewards.Provider: IsMember\n",
			"load       FAIL    plugin Create failed: ",
			"           hint    if Create takes a Host, pass -host <Ledger|Users>\n",
		}},
		{"Host chosen by -host", []string{"tsv2users", "-host", "Users"}, []string{
			"interface  ok      rewards.Provider: IsMember; Host rewards.Users: Tier (a stub: every call fails)\n",
			"load       FAIL    plugin Create failed: plg validate runs plugins without the service's Host: " +
				"rewards.Users.Tier was called\n",
		}},
		{"Host the plugin's typings were written for", []string{"tsv2users"}, []string{
			"interface  ok      rewards.Provider: IsMember; Host rewards.Users: Tier (a stub: every call fails)\n",
			"typings    ok      plg.d.ts, tsconfig.json up to date\n",
			"types      ok      the plugin type-checks against rewards.Provider (plg.d.ts)\n",
			"load       FAIL    plugin Create failed: plg validate runs plugins without the service's Host: " +
				"rewards.Users.Tier was called\n",
		}},
		{"method of another signature", []string{"wrongsignature"}, []string{
			"types      FAIL    the plugin does not type-check against the contract: plugin.go:14:9: cannot use " +
				"provider{} (value of struct type provider) as cohort.Provider value in return statement: provider " +
				"does not implement cohort.Provider (wrong type for method IsMember) have IsMember(context.Context, " +
				"string) (string, error) want IsMember(context.Context, string) (bool, error)\n",
		}},
		{"Create of another signature", []string{"gocreatesignature"}, []string{
			"types      ok      the plugin's package type-checks against cohort.Provider\n",
			"load       FAIL    plugin source failed to load: plugin Create has the wrong type: Create has type " +
				"func() interp.valueInterface; want func Create() (<interface>, error)",
		}},
		{"go.mod pinning another version", []string{"gomodmismatch"}, []string{
			"go.mod     FAIL    the plugin's go.mod does not require the pinned contract version: " +
				filepath.Join(abs(t, "gomodmismatch"), "go.mod") +
				" requires example.com/rewards v1.1.0, plugin.yaml pins v1.2.0: raise the requirement in " +
				filepath.Join(abs(t, "gomodmismatch"), "go.mod") + " to example.com/rewards v1.2.0 (go get " +
				"example.com/rewards@v1.2.0)\n",
		}},
		{"no plugin.yaml", []string{"nosuchdir"}, []string{
			"manifest  FAIL    lstat " + filepath.Join(abs(t, "."), "nosuchdir", "plugin.yaml") +
				": no such file or directory\n",
		}},
		{"interface breaking the contract", []string{"brokencontract"}, []string{
			"interface  ok      cohort.Broken: Do\n",
			"typings    FAIL    plg: invalid interface cohort.Broken: method Do: first parameter must be " +
				"context.Context\n",
		}},
		{"Host breaking the contract", []string{"luagood", "-host", "Broken"}, []string{
			"typings    FAIL    plg: invalid host: plg: invalid interface cohort.Broken: method Do: first parameter " +
				"must be context.Context\n",
		}},
		{
			"import beyond the contract and the standard library",
			[]string{"goimportsother"},
			[]string{
				"go.mod     ok      ",
				"sandbox    FAIL    plg: the plugin imports packages the Go guest cannot provide: plugin.go:5:2: " +
					"\"example.com/other\"\n",
				"           hint    the Go guest provides its allowlist (the standard library packages plg lists " +
					"and github.com/heynemann/go-plg/plugin), the contract package, the plugin's vendor/ packages and " +
					"the contract's //plg:guest-packages\n",
			},
		},
		{"pinned to a version adding a method", []string{"newerpin"}, []string{
			"contract   ok      module example.com/rewards v1.3.0 (",
			"interface  ok      cohort.Provider: IsMember, List, Size; Host cohort.Users: Tier (a stub: every call " +
				"fails)\n",
			"load       FAIL    plugin source failed to load: the table Create returned has no method Size\n",
		}},
		{"go statements", []string{"gogoroutine"}, []string{
			"go.mod     ok      ",
			"sandbox    FAIL    plg: go statements are not allowed in plugins: plugin.go:15:2, plugin.go:22:3\n",
		}},
		{"a module that does not compile, which only a call requires", []string{"luasyntax"}, []string{
			"typings    ok      plg.d.lua up to date\n",
			"syntax     FAIL    1 .lua file does not compile\n" +
				"                   lib/tiers.lua:8: '}' expected near ']'\n",
		}},
	}

	for _, tc := range tests {
		got := validate(environ, tc.args...)
		assert.Equal(t, 1, got.code, "%s\n%s%s", tc.name, got.stdout, got.stderr)
		assert.Contains(t, got.stderr, "is not a valid plugin\n", tc.name)

		for _, want := range tc.want {
			assert.Contains(t, got.stdout, want, "%s\n%s", tc.name, got.stdout)
		}

		rows := strings.Split(strings.TrimSuffix(got.stdout, "\n"), "\n")
		failed := slices.IndexFunc(rows, func(row string) bool { return strings.Contains(row, "  FAIL  ") })
		require.GreaterOrEqual(t, failed, 0, tc.name)

		for _, row := range rows[failed+1:] {
			assert.True(t, strings.HasPrefix(row, " "), "%s: no step runs after the failed one: %q", tc.name, row)
		}
	}
}

// TestValidateExplainsWhatItCannotResolve validates without a go command on the PATH, and offline with a module
// cache that lacks the contract: the contract step says why.
func TestValidateExplainsWhatItCannotResolve(t *testing.T) {
	t.Parallel()

	noGo := append(testproxy.Environ("off", t.TempDir()), "PATH="+t.TempDir())
	got := validate(noGo, "luagood")
	assert.Equal(t, 1, got.code)
	assert.Contains(t, got.stdout, "contract  FAIL    the go command is needed to resolve contract modules, and "+
		"none is on the PATH (install Go from https://go.dev/dl)")

	got = validate(testproxy.Environ("off", t.TempDir()), "luagood")
	assert.Equal(t, 1, got.code)
	assert.Contains(t, got.stdout, "example.com/rewards@v1.2.0: module lookup disabled by GOPROXY=off")
}

func TestValidateUsage(t *testing.T) {
	t.Parallel()

	const (
		noOperand = "plg validate: name one plugin directory or author repository\n"
		nearLimit = "plg validate: -near-limit must be above 0 and at most 1, not "
	)

	for _, tc := range []struct {
		want string
		args []string
	}{
		{args: nil, want: noOperand},
		{args: []string{"a", "b"}, want: noOperand},
		{args: []string{"-nope", "a"}, want: "plg validate: flag provided but not defined: -nope\n"},
		{args: []string{"-format", "xml", "a"}, want: "plg validate: -format must be table or json, not \"xml\"\n"},
		{
			args: []string{"-color", "sometimes", "a"},
			want: "plg validate: -color must be auto, always or never, not \"sometimes\"\n",
		},
		{args: []string{"-near-limit", "0", "a"}, want: nearLimit + "0\n"},
		{args: []string{"-near-limit", "1.5", "a"}, want: nearLimit + "1.5\n"},
		{args: []string{"-near-limit", "NaN", "a"}, want: nearLimit + "NaN\n"},
	} {
		got := validate(nil, tc.args...)
		assert.Equal(t, 2, got.code, tc.args)
		assert.True(t, strings.HasPrefix(got.stderr, tc.want), "%v: %s", tc.args, got.stderr)
		assert.Empty(t, got.stdout, tc.args)
	}
}

// TestValidateRefusesPluginsOverTheLimits validates copies of a Lua and a Go plugin, one with a sparse source over
// plg.MaxPluginFileSize (data.lua, data.go, which the syntax and sandbox steps would read), one with more than
// plg.MaxPluginEntries entries: each fails at the files step with plg.ErrPluginTooLarge naming the entry, as its
// load would, before any later step, and without reading the large file.
//
//nolint:paralleltest // runtime.MemStats.TotalAlloc over one run measures only it while no other test runs
func TestValidateRefusesPluginsOverTheLimits(t *testing.T) {
	for _, tc := range []struct{ plugin, source string }{{"luagood", "data.lua"}, {"gogood", "data.go"}} {
		t.Run(tc.plugin+" with a sparse file", func(t *testing.T) {
			dir := pluginCopy(t, tc.plugin)
			big := filepath.Join(dir, tc.source)
			require.NoError(t, os.WriteFile(big, nil, 0o600))
			require.NoError(t, os.Truncate(big, 4*plg.MaxPluginFileSize))

			var before, after runtime.MemStats

			runtime.GC()
			runtime.ReadMemStats(&before)

			got := validate(nil, dir)

			runtime.ReadMemStats(&after)

			assert.Equal(t, 1, got.code, got.stdout+got.stderr)
			assert.Contains(t, got.stdout, fmt.Sprintf("files     FAIL    plg: plugin directory %s: copying the "+
				"plugin's files: %v: %s is over %d bytes\n", tc.plugin, plg.ErrPluginTooLarge, tc.source,
				plg.MaxPluginFileSize))
			assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(plg.MaxPluginFileSize/4), "nothing is read")
		})

		t.Run(tc.plugin+" with too many entries", func(t *testing.T) {
			dir := pluginCopy(t, tc.plugin)
			many := filepath.Join(dir, "many")
			require.NoError(t, os.Mkdir(many, 0o750))

			for i := range plg.MaxPluginEntries {
				require.NoError(t, os.WriteFile(filepath.Join(many, fmt.Sprintf("f%05d", i)), nil, 0o600))
			}

			got := validate(nil, dir)
			assert.Equal(t, 1, got.code, got.stdout+got.stderr)
			assert.Contains(t, got.stdout, fmt.Sprintf("files     FAIL    plg: plugin directory %s: copying the "+
				"plugin's files: %v: many/f", tc.plugin, plg.ErrPluginTooLarge))
			assert.Contains(t, got.stdout, fmt.Sprintf("over %d entries", plg.MaxPluginEntries))
			assert.NotContains(t, got.stdout, "contract ", "no later step runs")
		})
	}
}

// linkedPlugin copies the testdata plugin into a directory of the test's own, holding alias.lua, a link to
// another of its files, and returns a symbolic link to it, named linked.
func linkedPlugin(t *testing.T, plugin string) string {
	t.Helper()

	dir := pluginCopy(t, plugin)
	require.NoError(t, os.Symlink("plugin.yaml", filepath.Join(dir, "alias.lua")))

	link := filepath.Join(t.TempDir(), "linked")
	require.NoError(t, os.Symlink(dir, link))

	return link
}

// TestValidateFollowsALinkToThePluginDirectory validates a plugin through a symbolic link to its directory: it
// passes, as named on the command line, under the link's name, the link inside it left out of its files as ever (it
// would not compile as Lua); a link inside it leading out of it still fails the files step.
func TestValidateFollowsALinkToThePluginDirectory(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	got := validate(environ, linkedPlugin(t, "luagood"))
	require.Equal(t, 0, got.code, got.stdout+got.stderr)
	assert.Contains(t, got.stdout, "manifest   ok      lua plugin linked implementing ")

	link := linkedPlugin(t, "luagood")
	outside := filepath.Join(t.TempDir(), "secret.lua")
	require.NoError(t, os.WriteFile(outside, []byte("return 1\n"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(link, "leak.lua")))

	got = validate(environ, link)
	assert.Equal(t, 1, got.code, got.stdout+got.stderr)
	assert.Contains(t, got.stdout, "files     FAIL    plg: plugin directory luagood: copying the plugin's files: ")
	assert.Contains(t, got.stdout, "leak.lua: symbolic link out of the plugin's files")
}

// TestValidateFollowsALinkedWorkingDirectory validates "." from a working directory reached through a symbolic
// link, whose path (as os.Getwd returns $PWD) is the link's: the plugin passes.
//
//nolint:paralleltest // t.Chdir: the working directory is the process's
func TestValidateFollowsALinkedWorkingDirectory(t *testing.T) {
	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	link := linkedPlugin(t, "luagood")
	t.Chdir(link)

	var stdout, stderr bytes.Buffer

	code := cli.Main([]string{"validate", "."}, &cli.Env{
		Stdout: &stdout, Stderr: &stderr, Environ: func() []string { return environ }, Dir: link,
	})
	require.Equal(t, 0, code, stdout.String()+stderr.String())
	assert.Contains(t, stdout.String(), "manifest   ok      lua plugin linked implementing ")
}

// TestValidateReadsABoundedManifest validates copies of a plugin whose plugin.yaml is over plg.MaxManifestSize, a
// FIFO (opening it would wait for a writer forever), a link to /dev/zero (an endless read) or a link to another of
// its files (which its snapshot leaves out, so plg pack could only fail): each fails the manifest step at once,
// before any later step, for plg validate and plg pack alike.
func TestValidateReadsABoundedManifest(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		make func(t *testing.T, manifest string)
		want string
	}{
		{"oversized", func(t *testing.T, manifest string) {
			t.Helper()
			require.NoError(t, os.WriteFile(manifest, []byte("language: lua\n#"+strings.Repeat("x",
				plg.MaxManifestSize)), 0o600))
		}, fmt.Sprintf("plugin.yaml is over %d bytes", plg.MaxManifestSize)},
		{"a FIFO", func(t *testing.T, manifest string) {
			t.Helper()
			require.NoError(t, os.Remove(manifest))
			require.NoError(t, syscall.Mkfifo(manifest, 0o600))
		}, "plugin.yaml is not a regular file"},
		{"a link to /dev/zero", func(t *testing.T, manifest string) {
			t.Helper()
			require.NoError(t, os.Remove(manifest))
			require.NoError(t, os.Symlink("/dev/zero", manifest))
		}, "plugin.yaml is not a regular file"},
		{"a link inside the plugin", func(t *testing.T, manifest string) {
			t.Helper()
			require.NoError(t, os.Rename(manifest, filepath.Join(filepath.Dir(manifest), "manifest.yaml")))
			require.NoError(t, os.Symlink("manifest.yaml", manifest))
		}, "plugin.yaml is not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := pluginCopy(t, "luagood")
			tc.make(t, filepath.Join(dir, "plugin.yaml"))

			for command, got := range map[string]validated{
				"validate": validate(nil, dir), "pack": pack(nil, "-o", t.TempDir(), dir),
			} {
				assert.Equal(t, 1, got.code, got.stdout+got.stderr)
				assert.Contains(t, got.stdout, "manifest  FAIL    "+filepath.Join(dir, tc.want)+"\n", command)
				assert.NotContains(t, got.stdout, "contract ", "%s: no later step runs", command)
			}
		})
	}
}

// abs returns the absolute path of the testdata plugin dir.
func abs(t *testing.T, dir string) string {
	t.Helper()

	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "validate", "plugins", dir))
	require.NoError(t, err)

	_, err = os.Stat(path)
	require.NoError(t, err)

	return path
}
