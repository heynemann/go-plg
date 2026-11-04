package typescript_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/guest/typescript"
	"github.com/heynemann/go-plg/internal/guestreg"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestImportRegistersTheTypeScriptGuest(t *testing.T) {
	t.Parallel()

	guest, ok := guestreg.Lookup(typescript.Language)
	require.True(t, ok)

	wasm, err := guest.Wasm()
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(wasm, []byte("\x00asm\x01\x00\x00\x00")), "a wasm 1.0 module")

	for _, unwanted := range []string{"js_std_", "js_os_", "qjs:std", "qjs:os"} {
		assert.False(t, bytes.Contains(wasm, []byte(unwanted)), "QuickJS's %s modules are not linked", unwanted)
	}
}

func TestImportRegistersTheBundler(t *testing.T) {
	t.Parallel()

	guest, ok := guestreg.Lookup(typescript.Language)
	require.True(t, ok)
	require.NotNil(t, guest.Bundler)

	wasm, err := guest.Bundler()
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(wasm, []byte("\x00asm\x01\x00\x00\x00")), "a wasm 1.0 module")

	again, err := guest.Bundler()
	require.NoError(t, err)
	assert.Same(t, &wasm[0], &again[0], "inflated once")
}

// TestNothingLinksTheBundlerNatively checks the link graph: esbuild runs only inside the bundler module, so
// neither the guest package nor the core links it (nor the guest package wazero).
func TestNothingLinksTheBundlerNatively(t *testing.T) {
	t.Parallel()

	deps := func(pkg string) []string {
		//nolint:gosec // G204: go list on fixed package paths
		out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", pkg).Output()
		require.NoError(t, err)

		return strings.Fields(string(out))
	}

	for _, dep := range deps("github.com/heynemann/go-plg/guest/typescript") {
		assert.NotContains(t, dep, "esbuild")
		assert.NotContains(t, dep, "wazero")
	}

	for _, dep := range deps("github.com/heynemann/go-plg") {
		assert.NotContains(t, dep, "esbuild")
		assert.NotContains(t, dep, "tsbundle")
	}
}
