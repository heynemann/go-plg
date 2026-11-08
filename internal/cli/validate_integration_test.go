//go:build integration

package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		Stdout: &stdout, Stderr: &stderr, Getenv: func(string) string { return "" },
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
// Create runs in the language's real guest. Validating again with GOPROXY=off works from the module cache.
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

	compiled, err := os.ReadDir(cacheDir)
	require.NoError(t, err)
	assert.NotEmpty(t, compiled, "-cache-dir is where the guest is compiled to")
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
			"manifest  FAIL    open " + filepath.Join(abs(t, "."), "nosuchdir", "plugin.yaml") +
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

// abs returns the absolute path of the testdata plugin dir.
func abs(t *testing.T, dir string) string {
	t.Helper()

	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "validate", "plugins", dir))
	require.NoError(t, err)

	_, err = os.Stat(path)
	require.NoError(t, err)

	return path
}
