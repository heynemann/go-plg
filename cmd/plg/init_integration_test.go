//go:build integration

package main

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/testproxy"
)

// TestInitFromTheCommand builds the plg command, creates a TypeScript plugin of the testdata's pricing.Rule with
// plg init and validates it unchanged with plg validate: its typings, type-check, load and example test cases pass.
func TestInitFromTheCommand(t *testing.T) {
	t.Parallel()

	bin := buildCommand(t)

	modules, err := filepath.Abs(filepath.Join("..", "..", "testdata", "validate", "modules"))
	require.NoError(t, err)

	environ := testproxy.Environ(testproxy.New(t, modules), t.TempDir())
	root := t.TempDir()

	plg := func(args ...string) string {
		t.Helper()

		//nolint:gosec // G204: runs the plg command the test built
		cmd := exec.CommandContext(t.Context(), bin, args...)
		cmd.Env, cmd.Dir = environ, root

		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))

		return string(out)
	}

	plg("init", "-implements", "example.com/shop/pricing.Rule@v1.0.0", "-lang", "ts", "rule")
	assert.FileExists(t, filepath.Join(root, "rule", "plg.d.ts"))
	assert.FileExists(t, filepath.Join(root, "rule", "plugin.ts"))

	validated := plg("validate", "rule")
	assert.Contains(t, validated, "the plugin type-checks against pricing.Rule (plg.d.ts)")
	assert.Contains(t, validated, "5 passed")
}
