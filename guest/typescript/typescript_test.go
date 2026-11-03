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

// TestTheGuestLinksNoBundler checks the link graph: esbuild runs in the core, on the host, not in the guest
// package.
func TestTheGuestLinksNoBundler(t *testing.T) {
	t.Parallel()

	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}",
		"github.com/heynemann/go-plg/guest/typescript").Output()
	require.NoError(t, err)

	for dep := range strings.FieldsSeq(string(out)) {
		assert.NotContains(t, dep, "esbuild")
		assert.NotContains(t, dep, "wazero")
	}
}
