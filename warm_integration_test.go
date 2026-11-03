//go:build integration

package plg_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/guestreg"
)

// TestWarm warms the default compilation cache, which the other tests use (so this is a cache hit except on a
// machine's first run); cmd/plg's integration test proves across processes that warming spares a cold compile.
func TestWarm(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	require.NoError(t, plg.Warm(t.Context(), plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil)))))

	for _, guest := range guestreg.All() {
		assert.Contains(t, logs.String(), `"msg":"plg: guest compiled","guest":"`+guest.Language+`"`,
			"every linked guest is compiled")
	}

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	err := plg.Warm(t.Context(), plg.WithCompilationCacheDir(filepath.Join(file, "cache")))
	require.ErrorIs(t, err, syscall.ENOTDIR, "an unusable directory fails rather than warming memory")
	assert.ErrorContains(t, err, file)
}
