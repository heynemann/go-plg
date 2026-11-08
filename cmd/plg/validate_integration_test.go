//go:build integration

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/testproxy"
)

// TestValidateFromTheCommand builds the plg command and validates the testdata's plugins with it, the contract
// module served by a file-based GOPROXY: a good TypeScript plugin passes with exit code 0, and one whose Create
// fails exits 1, the failed step on stdout and the verdict on stderr; the testdata's author repository, whose
// plugins of the three languages import its shared directory, passes with a summary; a Lua plugin whose test
// cases all pass exits 0.
func TestValidateFromTheCommand(t *testing.T) {
	t.Parallel()

	bin := filepath.Join(t.TempDir(), "plg")

	//nolint:gosec // G204: builds this module's plg command
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".")

	build.Env = append(os.Environ(), "GOWORK=off")

	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	testdata, err := filepath.Abs(filepath.Join("..", "..", "testdata", "validate"))
	require.NoError(t, err)

	environ := testproxy.Environ(testproxy.New(t, filepath.Join(testdata, "modules")), t.TempDir())

	plgValidate := func(plugin string) (string, string, int) {
		t.Helper()

		//nolint:gosec // G204: runs the plg command the test built
		cmd := exec.CommandContext(t.Context(), bin, "validate", plugin)
		cmd.Env = environ

		var stdout, stderr bytes.Buffer

		cmd.Stdout, cmd.Stderr = &stdout, &stderr

		err := cmd.Run()
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return stdout.String(), stderr.String(), exit.ExitCode()
		}

		require.NoError(t, err)

		return stdout.String(), stderr.String(), 0
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
	assert.True(t, strings.HasSuffix(stdout, "\ntests      ok      10 passed\n"), stdout)
}
