//go:build integration

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/testproxy"
)

// TestValidateFromTheCommand builds the plg command and validates the testdata's plugins with it, the contract
// module served by a file-based GOPROXY: a good TypeScript plugin passes with exit code 0, and one whose Create
// fails exits 1, the failed step on stdout and the verdict on stderr; the testdata's author repository, whose
// plugins of the three languages import its shared directory, passes with a summary; a Lua plugin whose test
// cases all pass exits 0. A copy of the good plugin without its typings fails until plg types writes them.
func TestValidateFromTheCommand(t *testing.T) {
	t.Parallel()

	bin := buildCommand(t)

	testdata, err := filepath.Abs(filepath.Join("..", "..", "testdata", "validate"))
	require.NoError(t, err)

	environ := testproxy.Environ(testproxy.New(t, filepath.Join(testdata, "modules")), t.TempDir())

	plg := func(command, plugin string) (string, string, int) {
		t.Helper()

		return runCommand(t, bin, environ, command, plugin)
	}

	plgValidate := func(plugin string) (string, string, int) {
		t.Helper()

		return plg("validate", plugin)
	}

	stdout, stderr, code := plgValidate(filepath.Join(testdata, "plugins", "tsgood"))
	require.Equal(t, 0, code, stdout+stderr)
	assert.Contains(t, stdout, "contract   ok      module example.com/rewards v1.2.0 (")
	assert.Contains(t, stdout, "load       ok      Create ran in the typescript guest (version ")

	stdout, stderr, code = plgValidate(filepath.Join(testdata, "plugins", "createfails"))
	assert.Equal(t, 1, code, stdout+stderr)
	assert.Contains(t, stdout, "load       FAIL    plugin Create failed: no cohort configured\n")
	assert.Contains(t, stderr, "plg validate: "+filepath.Join(testdata, "plugins", "createfails")+
		" is not a valid plugin\n")

	stdout, stderr, code = plgValidate(filepath.Join(testdata, "repo"))
	require.Equal(t, 0, code, stdout+stderr)
	assert.Contains(t, stdout, "\nrules/silver\nSTEP ")
	assert.Contains(t, stdout, "shared     ok      copied lib at vendor/example.com/rewardsrepo/lib\n")
	assert.Contains(t, stdout, "\n\nPLUGIN        RESULT  DETAIL\ngold          ok      \nrules/silver  ok      \n"+
		"coupon        ok      \n")

	pricing := filepath.Join(t.TempDir(), "luapricing")
	require.NoError(t, os.CopyFS(pricing, os.DirFS(filepath.Join(testdata, "plugins", "luapricing"))))
	require.NoError(t, os.CopyFS(pricing, os.DirFS(filepath.Join(testdata, "cases"))))
	require.NoError(t, os.Remove(filepath.Join(pricing, "tests", "fail.yaml")))

	stdout, stderr, code = plgValidate(pricing)
	require.Equal(t, 0, code, stdout+stderr)
	assert.Contains(t, stdout, "\ntests      ok      10 passed\nresources  ok      tests/pass.yaml: ", stdout)

	untyped := filepath.Join(t.TempDir(), "untyped")
	require.NoError(t, os.CopyFS(untyped, os.DirFS(filepath.Join(testdata, "plugins", "tsgood"))))
	require.NoError(t, os.Remove(filepath.Join(untyped, "plg.d.ts")))

	stdout, stderr, code = plg("validate", untyped)
	assert.Equal(t, 1, code, stdout+stderr)
	assert.Contains(t, stdout, "typings    FAIL    typings: stale: plg.d.ts not what plg types writes for "+
		"example.com/rewards/cohort.Provider@v1.2.0: run plg types -host Users "+untyped+"\n")

	stdout, stderr, code = plg("types", untyped)
	require.Equal(t, 0, code, stdout+stderr)
	assert.Contains(t, stdout, "typings    ok      wrote plg.d.ts; tsconfig.json already up to date\n")

	stdout, stderr, code = plg("validate", untyped)
	require.Equal(t, 0, code, stdout+stderr)
	assert.Contains(t, stdout, "types      ok      the plugin type-checks against cohort.Provider (plg.d.ts)\n")
}

// TestValidateReportFromTheCommand runs the plg command's report formats. A Go plugin of a contract with
// //plg:guest-packages, validated without a user cache directory for its custom guest (no HOME, no
// XDG_CACHE_HOME, no -guest-cache), fails at the guest step: in the coloured table (-color always), in the JSON
// report on stdout (-format json) and in the JUnit file (-junit), and the command exits 1. A plugin whose only
// problem is a warning (its owner is missing) exits 0, the warning counted in the JSON report.
func TestValidateReportFromTheCommand(t *testing.T) {
	t.Parallel()

	bin := buildCommand(t)

	testdata, err := filepath.Abs(filepath.Join("..", "..", "testdata", "validate"))
	require.NoError(t, err)

	environ := slices.DeleteFunc(testproxy.Environ(testproxy.New(t, filepath.Join(testdata, "modules")),
		t.TempDir()), func(kv string) bool {
		return strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CACHE_HOME=")
	})
	environ = append(environ, "GOCACHE="+t.TempDir())
	goxml := filepath.Join(testdata, "plugins", "goxml")

	stdout, stderr, code := runCommand(t, bin, environ, "validate", "-color", "always", goxml)
	assert.Equal(t, 1, code, stdout+stderr)
	assert.Contains(t, stdout, "\nguest      \x1b[31mFAIL\x1b[0m    finding the guest cache: ")
	assert.Contains(t, stderr, "plg validate: "+goxml+" is not a valid plugin\n")

	junit := filepath.Join(t.TempDir(), "plg.xml")
	stdout, stderr, code = runCommand(t, bin, environ, "validate", "-format", "json", "-junit", junit, goxml)
	assert.Equal(t, 1, code, stdout+stderr)

	var report validateReport

	require.NoError(t, sonic.ConfigDefault.UnmarshalFromString(stdout, &report), stdout)
	require.Len(t, report.Plugins, 1)
	assert.False(t, report.Valid)
	assert.Equal(t, "guest", report.Plugins[0].FailedStep)

	last := report.Plugins[0].Steps[len(report.Plugins[0].Steps)-1]
	assert.Equal(t, "guest", last.Step)
	assert.Equal(t, "failed", last.Result)
	assert.Contains(t, last.Detail, "finding the guest cache: ")

	data, err := os.ReadFile(junit) //nolint:gosec // G304: the test's file
	require.NoError(t, err)
	assert.Regexp(t, `<testsuites name="plg validate" tests="[0-9]+" failures="1">`, string(data))
	assert.Regexp(t, `<testcase name="guest" classname="`+regexp.QuoteMeta(goxml)+`">\s*`+
		`<failure message="finding the guest cache: `, string(data))

	stdout, stderr, code = runCommand(t, bin, environ, "validate", "-format", "json",
		filepath.Join(testdata, "plugins", "noowner"))
	require.Equal(t, 0, code, "a warning does not fail validate\n%s%s", stdout, stderr)
	assert.Contains(t, stdout, `"warnings": 1,`)
	assert.Contains(t, stdout, `"valid": true`)
}

// buildCommand builds this module's plg command into a temporary directory and returns its path.
func buildCommand(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "plg")

	//nolint:gosec // G204: builds this module's plg command
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".")

	build.Env = append(os.Environ(), "GOWORK=off")

	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	return bin
}

// validateReport is the part of plg validate's JSON report the binary's test reads.
type validateReport struct {
	Plugins []validatedPlugin `json:"plugins"`
	Valid   bool              `json:"valid"`
}

// validatedPlugin is a plugin of a [validateReport].
type validatedPlugin struct {
	FailedStep string          `json:"failedStep"`
	Steps      []validatedStep `json:"steps"`
	Valid      bool            `json:"valid"`
}

// validatedStep is a step of a [validatedPlugin].
type validatedStep struct {
	Step   string `json:"step"`
	Result string `json:"result"`
	Detail string `json:"detail"`
}

// runCommand runs the plg command bin with args in environ and returns its stdout, stderr and exit code.
//
//nolint:revive,gocritic // stdout then stderr, as documented (nonamedreturns forbids naming them)
func runCommand(t *testing.T, bin string, environ []string, args ...string) (string, string, int) {
	t.Helper()

	//nolint:gosec // G204: runs the plg command the test built
	cmd := exec.CommandContext(t.Context(), bin, args...)
	cmd.Env = environ

	var out, errOut bytes.Buffer

	cmd.Stdout, cmd.Stderr = &out, &errOut

	err := cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return out.String(), errOut.String(), exit.ExitCode()
	}

	require.NoError(t, err)

	return out.String(), errOut.String(), 0
}
