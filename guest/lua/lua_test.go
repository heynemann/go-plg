package lua_test

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/guest/lua"
	"github.com/heynemann/go-plg/internal/guestreg"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

//nolint:ireturn // wazero exposes compiled modules only as an interface
func compile(t *testing.T, features api.CoreFeatures) (wazero.CompiledModule, error) {
	t.Helper()

	guest, ok := guestreg.Lookup(lua.Language)
	require.True(t, ok, "importing guest/lua registers the Lua guest")

	wasm, err := guest.Wasm()
	require.NoError(t, err)

	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter().WithCoreFeatures(features))

	t.Cleanup(func() { assert.NoError(t, rt.Close(ctx)) })

	return rt.CompileModule(ctx, wasm) //nolint:wrapcheck // test helper
}

// TestTheArtifactSpeaksTheGuestABIAndImportsNothingElse pins the artifact's interface: a reactor exporting the
// plg guest ABI whose only imports are WASI preview 1 and the plg host functions, so a Lua plugin has no other
// way out of its sandbox.
func TestTheArtifactSpeaksTheGuestABIAndImportsNothingElse(t *testing.T) {
	t.Parallel()

	mod, err := compile(t, api.CoreFeaturesV2|experimental.CoreFeaturesExceptionHandling)
	require.NoError(t, err)

	exports := mod.ExportedFunctions()
	for _, name := range []string{"_initialize", "plg_alloc", "plg_init", "plg_call", "plg_close", "plg_start", "plg_resume"} {
		assert.Contains(t, exports, name)
	}

	assert.Contains(t, mod.ExportedMemories(), "memory")

	var plgImports []string

	for _, fn := range mod.ImportedFunctions() {
		module, name, _ := fn.Import()
		if module == "plg" {
			plgImports = append(plgImports, name)

			continue
		}

		assert.Equal(t, "wasi_snapshot_preview1", module, "import %s.%s", module, name)
	}

	slices.Sort(plgImports)
	assert.Equal(t, []string{"host_call", "http_roundtrip", "http_start", "secret", "take_result"}, plgImports)
}

// TestTheArtifactNeedsExceptionHandling pins why every plg runtime enables wazero's experimental exception
// handling: Lua's errors are compiled to it, and without the feature the guest does not even compile.
func TestTheArtifactNeedsExceptionHandling(t *testing.T) {
	t.Parallel()

	_, err := compile(t, api.CoreFeaturesV2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exception")
}
