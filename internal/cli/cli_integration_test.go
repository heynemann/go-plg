//go:build integration

package cli_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/cli"
	"github.com/heynemann/go-plg/internal/engine"
)

// TestWarmLogsEachCompile warms the default compilation cache (which the other tests share, so this is a cache
// hit except on a machine's first run), resolved as a container resolves it. cmd/plg's integration test proves
// what warming spares a fresh process.
func TestWarmLogsEachCompile(t *testing.T) {
	t.Parallel()

	def, err := engine.DefaultCacheDir()
	require.NoError(t, err)

	var stderr bytes.Buffer

	require.Equal(t, 0, cli.Main([]string{"warm", "-cache-dir", def}, t.TempDir(), &stderr), stderr.String())
	assert.Contains(t, stderr.String(), `level=INFO msg="plg: guest compiled" guest=go duration=`,
		"the Go guest plg ships is compiled, and the compile logged")
}
