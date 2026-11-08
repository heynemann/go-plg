//go:build integration

package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/directive"
	"github.com/heynemann/go-plg/internal/testproxy"
)

// assertOK asserts that out has the table row of a step that passed, with a detail starting with detail.
func assertOK(t *testing.T, out, step, detail string, msgAndArgs ...any) {
	t.Helper()

	pattern := `(?m)^` + regexp.QuoteMeta(step) + ` +ok +` + regexp.QuoteMeta(detail)
	assert.Regexp(t, pattern, out, msgAndArgs...)
}

// TestValidateRunsPluginsUnderTheContractsLimits validates Lua plugins of the testdata's example.com/directives
// contract: TightRule declares //plg:limits memory=16MiB timeout=300ms pool=1..2, Rule declares none. Validate
// prints the limits it runs each plugin under, and they are enforced: a Create keeping a 20 MiB string passes
// under Rule's 64 MiB default and fails under TightRule's 16 MiB, and a Create counting for seconds is stopped at
// TightRule's 300ms.
func TestValidateRunsPluginsUnderTheContractsLimits(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	tight := "memory=16MiB timeout=300ms pool=1..2 (//plg:limits)"
	defaults := fmt.Sprintf("memory=64MiB pool=1..%d, no timeout (plg's defaults)",
		min(runtime.GOMAXPROCS(0), directive.MaxPool))

	tests := []struct {
		dir, limits, load, failure string
	}{
		{dir: "luatight", limits: tight, load: "Create ran in the lua guest"},
		{dir: "luabigdefault", limits: defaults, load: "Create ran in the lua guest"},
		{dir: "luabig", limits: tight, failure: "not enough memory"},
		{dir: "luaslow", limits: tight, failure: "deadline"},
	}

	for _, tc := range tests {
		got := validate(environ, tc.dir)
		assertOK(t, got.stdout, "limits", tc.limits, tc.dir)

		if tc.failure == "" {
			require.Equal(t, 0, got.code, "%s\n%s%s", tc.dir, got.stdout, got.stderr)
			assertOK(t, got.stdout, "load", tc.load, tc.dir)

			continue
		}

		require.Equal(t, 1, got.code, "%s\n%s%s", tc.dir, got.stdout, got.stderr)
		assert.Regexp(t, `(?m)^load +FAIL +.*`+regexp.QuoteMeta(tc.failure), got.stdout, tc.dir)
	}
}

// TestValidateBuildsAndCachesTheContractsGoGuest validates Go plugins importing encoding/xml, outside the default
// Go guest's allowlist: one implements XMLParser, whose //plg:guest-packages encoding/xml makes validate build
// that custom guest from the plugin's module and load the plugin in it; the other implements Parser, the same
// interface without the directive, and fails to load in the default guest. Validating again finds the guest in
// the guest cache: the same file, not rebuilt.
func TestValidateBuildsAndCachesTheContractsGoGuest(t *testing.T) {
	t.Parallel()

	// plg's own dependencies, which the guest's build needs, come from the go command's module cache.
	environ := testproxy.Environ(testproxy.New(t, modules())+","+testproxy.ModuleCache(t), t.TempDir())
	guests := t.TempDir()

	plain := validate(environ, "goxmlplain", "-guest-cache", guests)
	require.Equal(t, 1, plain.code, "%s%s", plain.stdout, plain.stderr)
	assert.NotContains(t, plain.stdout, "guest ", "no directive, no custom guest")
	assert.Regexp(t, `(?m)^load +FAIL +.*encoding/xml`, plain.stdout)

	first := validate(environ, "goxml", "-guest-cache", guests)
	require.Equal(t, 0, first.code, "%s%s", first.stdout, first.stderr)

	entries, err := os.ReadDir(guests)
	require.NoError(t, err)
	require.Len(t, entries, 1, "one guest, no temporary file left")
	require.Regexp(t, `^[0-9a-f]{64}\.wasm$`, entries[0].Name())

	guest := filepath.Join(guests, entries[0].Name())
	assertOK(t, first.stdout, "guest", "a custom Go guest with encoding/xml, built into the guest cache ("+
		guest+")\n")
	assertOK(t, first.stdout, "load", "Create ran in the go guest")

	built, err := os.Stat(guest)
	require.NoError(t, err)

	again := validate(environ, "goxml", "-guest-cache", guests)
	require.Equal(t, 0, again.code, "%s%s", again.stdout, again.stderr)
	assertOK(t, again.stdout, "guest", "a custom Go guest with encoding/xml, found in the guest cache ("+
		guest+")\n")
	assertOK(t, again.stdout, "load", "Create ran in the go guest")

	reused, err := os.Stat(guest)
	require.NoError(t, err)
	assert.True(t, os.SameFile(built, reused), "the cached file itself, not a rebuilt one renamed over it")
	assert.Equal(t, built.ModTime(), reused.ModTime())

	entries, err = os.ReadDir(guests)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// TestValidateRepoLimitsOverrideTheContracts: an author repository's plg.yaml raises TightRule's 16 MiB memory
// limit to 64 MiB and its 300ms timeout to 1h, keeping its pool, and the plugin keeping 20 MiB, which fails under
// TightRule alone (TestValidateRunsPluginsUnderTheContractsLimits), passes. The hour is past go test's -timeout,
// so it never fires first: writing 20 MiB under the race detector took over 10s on a loaded CI runner.
func TestValidateRepoLimitsOverrideTheContracts(t *testing.T) {
	t.Parallel()

	big, err := os.ReadFile(filepath.Join("..", "..", "testdata", "validate", "plugins", "luabig", "plugin.lua"))
	require.NoError(t, err)

	manifest, err := os.ReadFile(filepath.Join("..", "..", "testdata", "validate", "plugins", "luabig", "plugin.yaml"))
	require.NoError(t, err)

	repo := copyRepo(t, "plugins: [tightbig]\nlimits: {memory: 64MiB, timeout: 1h}\n", map[string]string{
		"tightbig/plugin.lua": string(big), "tightbig/plugin.yaml": string(manifest),
	})

	got := validate(testproxy.Environ(testproxy.New(t, modules()), t.TempDir()), repo)
	require.Equal(t, 0, got.code, "%s%s", got.stdout, got.stderr)

	steps := section(t, got.stdout, "tightbig")
	assertOK(
		t,
		steps,
		"limits",
		"memory=64MiB timeout=1h0m0s pool=1..2 (plg.yaml memory=64MiB timeout=1h0m0s over //plg:limits "+
			"memory=16MiB timeout=300ms pool=1..2)\n",
	)
	assertOK(t, steps, "load", "Create ran in the lua guest")
}
