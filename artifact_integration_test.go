//go:build integration

package plg_test

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/artifact"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// artifactStorage keeps one artifact per plugin, as a platform's storage keeps its blobs: List reads each
// artifact's plugin.yaml, Open returns the artifact itself.
type artifactStorage struct {
	blobs map[string][]byte // by key
}

func (artifactStorage) Name() string { return "artifacts" }

func (s artifactStorage) List(context.Context) ([]plg.Descriptor, error) {
	descs := make([]plg.Descriptor, 0, len(s.blobs))

	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for key := range s.blobs {
			if !yield(key) {
				return
			}
		}
	}) {
		art, err := openArtifact(s.blobs[key])
		if err != nil {
			return nil, err
		}

		manifest, err := art.ReadFile("plugin.yaml")
		if err != nil {
			return nil, err //nolint:wrapcheck // a test storage
		}

		desc, err := plg.ParseManifest(manifest)
		if err != nil {
			return nil, err //nolint:wrapcheck // a test storage
		}

		desc.Key = key
		descs = append(descs, desc)
	}

	return descs, nil
}

func (s artifactStorage) Open(_ context.Context, desc *plg.Descriptor) (fs.FS, error) {
	return openArtifact(s.blobs[desc.Key])
}

// luaFooerArtifact is a Lua Fooer whose Foo answers "<name> from the artifact".
const luaFooerArtifact = `function Create()
  return {
    Foo = function(_, name) return name .. " from the artifact" end,
    Count = function() return 0 end,
    Fail = function() end,
    Boil = function() return { false, 0 } end,
    Scalars = function(_, ...) return { ... } end,
    Spin = function(_, n) return n end,
  }
end
`

// TestPluginsLoadedFromArtifactsShowTheirProvenance registers a storage keeping artifacts (one recording a dirty
// tree and a report saying the plugin is not valid: nothing about the provenance is enforced at load) beside one
// keeping the same files without an artifact. Both load and serve; the artifact's version is its content digest
// (its plugin.yaml names none); Info and the admin handler show the artifact's provenance, its report as JSON,
// and none for the other plugin.
func TestPluginsLoadedFromArtifactsShowTheirProvenance(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"plugin.yaml": manifestOf[testcontract.Fooer]("language: lua\nentrypoint: main.lua\n"),
		"main.lua":    luaFooerArtifact,
	}
	tree := artifactTree(t, files)
	record := &adminjson.Provenance{
		Contract: adminjson.Module{Path: "github.com/heynemann/go-plg", Version: "v1.0.0"},
		Digest:   artifact.Digest(tree), Commit: "89abcdef", Plg: "v0.9.0", Dirty: true,
		Report: sonic.NoCopyRawMessage(`{"plugins":[{"plugin":"silver","valid":false}],"version":1,"valid":false}`),
	}

	var blob bytes.Buffer

	require.NoError(t, artifact.Write(&blob, tree, record))

	plain := plg.NewMemoryStorage("plain")
	desc, err := plg.ParseManifest([]byte(files["plugin.yaml"]))
	require.NoError(t, err)

	desc.Key = "bronze"
	require.NoError(t, plain.Put(&desc, tree))

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "rules", plg.WithPool(1, 1),
		plg.WithStorage(artifactStorage{blobs: map[string][]byte{"silver": blob.Bytes()}}), plg.WithStorage(plain)))

	got, err := getFooer(t, ctr, "rules/silver").Foo(t.Context(), "gold")
	require.NoError(t, err)
	assert.Equal(t, "gold from the artifact", got)

	silver := inspect(t, ctr, "rules/silver")
	require.NotNil(t, silver.Provenance)
	assert.Equal(t, plg.Provenance{
		Contract: plg.ContractModule{Path: "github.com/heynemann/go-plg", Version: "v1.0.0"},
		Digest:   artifact.Digest(tree), Commit: "89abcdef", PlgVersion: "v0.9.0", Dirty: true,
		Report: []byte(record.Report),
	}, *silver.Provenance)
	assert.Equal(t, silver.Provenance.Digest, silver.Version, "a versionless plugin's version is its digest")
	assert.Equal(t, plg.StatusServing, silver.Status)

	bronze := inspect(t, ctr, "rules/bronze")
	assert.Nil(t, bronze.Provenance, "loaded from plain files")
	assert.Equal(t, silver.Version, bronze.Version, "the same files, the same digest")

	srv := httptest.NewServer(plg.AdminHandler(ctr))
	t.Cleanup(srv.Close)

	answer := requestAdmin(t, srv, http.MethodGet, "/plugins/rules/silver")
	defer func() { require.NoError(t, answer.Body.Close()) }()

	require.Equal(t, http.StatusOK, answer.StatusCode)

	state := decodeAdmin(t, answer.Body)
	assert.Equal(t, stateJSON(&silver), state)
	assert.Equal(t, map[string]any{
		"contract": map[string]any{"path": "github.com/heynemann/go-plg", "version": "v1.0.0"},
		"digest":   silver.Version, "commit": "89abcdef", "plgVersion": "v0.9.0", "dirty": true,
		"report": map[string]any{
			"plugins": []any{map[string]any{"plugin": "silver", "valid": false}}, "version": float64(1), "valid": false,
		},
	}, state["provenance"])

	plainAnswer := requestAdmin(t, srv, http.MethodGet, "/plugins/rules/bronze")
	defer func() { require.NoError(t, plainAnswer.Body.Close()) }()

	plainState := decodeAdmin(t, plainAnswer.Body)
	assert.Contains(t, plainState, "provenance")
	assert.Nil(t, plainState["provenance"])
}
