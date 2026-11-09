//go:build integration

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testproxy"
)

// TestPackFromTheCommand builds the plg command and packs the testdata's author repository with it three times,
// each in a process of its own and from a copy of the repository at another path, with another module cache,
// guest cache and GOMAXPROCS, the last with -format json: every run writes the same bytes for each plugin (the
// packed report names no machine's directories nor CPU count), the JSON run prints validate's JSON report on
// stdout, and each artifact opens with plg.OpenArtifact, its provenance naming the pinned contract module and the
// plg the command was built from.
func TestPackFromTheCommand(t *testing.T) {
	t.Parallel()

	bin := buildCommand(t)

	testdata, err := filepath.Abs(filepath.Join("..", "..", "testdata", "validate"))
	require.NoError(t, err)

	proxy := testproxy.New(t, filepath.Join(testdata, "modules"))

	outs, repos := make([]string, 0, 3), make([]string, 0, 3)

	for i, format := range []string{"table", "table", "json"} {
		repo := filepath.Join(t.TempDir(), strings.Repeat("deeper/", i), "repo")
		require.NoError(t, os.CopyFS(repo, os.DirFS(filepath.Join(testdata, "repo"))))

		repos = append(repos, repo)

		environ := append(testproxy.Environ(proxy, t.TempDir()), fmt.Sprintf("GOMAXPROCS=%d", i+1))
		out := t.TempDir()
		outs = append(outs, out)

		stdout, stderr, code := runCommand(t, bin, environ, "pack", "-format", format, "-o", out,
			"-guest-cache", t.TempDir(), repo)
		require.Equal(t, 0, code, stdout+stderr)
		assert.Contains(t, stderr, "plg pack: wrote "+filepath.Join(out, "gold.plg")+" (sha256:")

		if format == "json" {
			var report validateReport

			require.NoError(t, sonic.ConfigStd.UnmarshalFromString(stdout, &report), stdout)
			assert.True(t, report.Valid)
			assert.Len(t, report.Plugins, 3)
		}
	}

	for _, key := range []string{"gold", "silver", "coupon"} {
		data, err := os.ReadFile(filepath.Join(outs[0], key+".plg")) //nolint:gosec // G304: the test's artifact
		require.NoError(t, err)

		for _, out := range outs[1:] {
			again, readErr := os.ReadFile(filepath.Join(out, key+".plg")) //nolint:gosec // G304: as above
			require.NoError(t, readErr)
			assert.Equal(t, data, again, "%s: the same input packs to the same bytes, wherever and however run", key)
		}

		art, err := plg.OpenArtifact(bytes.NewReader(data), int64(len(data)))
		require.NoError(t, err)

		provenance := art.Provenance()
		assert.Equal(t, plg.ContractModule{Path: "example.com/rewards", Version: "v1.2.0"}, provenance.Contract)
		assert.Equal(t, "(devel)", provenance.PlgVersion, "a command built from this checkout")

		for _, repo := range repos {
			assert.NotContains(t, string(provenance.Report), repo, "%s: the report names no machine's paths", key)
		}

		assert.Contains(t, string(provenance.Report), "pool=1..GOMAXPROCS")
	}
}
