//go:build integration

package typings_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	_ "github.com/heynemann/go-plg/guest/lua" // the Lua guest the typings run in
	"github.com/heynemann/go-plg/internal/testcontract"
)

// TestGoldenLuaTypingsRunInTheLuaGuest runs the golden Lua typings, those of the contract using every data type, in
// the real Lua guest: a plugin whose source starts with them loads, and its Create finds the empty tables they
// assign in place of the guest's http, secrets and json, and the host global. The language server reads Lua, so
// annotations the guest runs are annotations it reads.
func TestGoldenLuaTypingsRunInTheLuaGuest(t *testing.T) {
	t.Parallel()

	annotations, err := os.ReadFile(filepath.Join("testdata", "golden", "plg.d.lua"))
	require.NoError(t, err)

	source := string(annotations) + `
function Create()
  assert(next(http) == nil and next(secrets) == nil and next(json) == nil and next(host) == nil,
    "plg.d.lua did not run")
  return {
    Adopt = function(_, c) return c end,
    Mirror = function(_, p) return p end,
  }
end
`

	store := plg.NewMemoryStorage("typings")
	require.NoError(t, store.Put(&plg.Descriptor{
		Key: "golden", Implements: "github.com/heynemann/go-plg/internal/testcontract.Family@v1.0.0",
		Language: plg.LanguageLua, Entrypoint: "plugin.lua",
	}, fstest.MapFS{"plugin.lua": {Data: []byte(source)}}))

	ctr := plg.New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	require.NoError(t, plg.Register[testcontract.Family](t.Context(), ctr, "typings", plg.WithStorage(store)))

	family, err := plg.Get[testcontract.Family](t.Context(), ctr, "typings/golden")
	require.NoError(t, err)

	parent := testcontract.Parent{Children: []testcontract.Child{{}}}
	got, err := family.Mirror(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, parent, got)
}
