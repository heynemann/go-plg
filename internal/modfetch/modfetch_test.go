package modfetch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/modfetch"
	"github.com/heynemann/go-plg/internal/testproxy"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// modules is the testdata's contract modules.
func modules() string {
	return filepath.Join("..", "..", "testdata", "validate", "modules")
}

// TestFetchFindsTheModuleProvidingThePackage resolves packages through a file-based GOPROXY: a package inside a
// module resolves to that module (the longer candidate paths are not modules), its directory in the test's module
// cache; a module path that is a module but lacks the package is not taken; each failure names every candidate
// tried.
func TestFetchFindsTheModuleProvidingThePackage(t *testing.T) {
	t.Parallel()

	cache := t.TempDir()
	fetcher := modfetch.Fetcher{Environ: testproxy.Environ(testproxy.New(t, modules()), cache)}

	got, err := fetcher.Fetch(t.Context(), "example.com/rewards/cohort", "v1.3.0")
	require.NoError(t, err)
	assert.Equal(t, modfetch.Module{
		Path: "example.com/rewards", Version: "v1.3.0", Dir: filepath.Join(cache, "example.com", "rewards@v1.3.0"),
	}, got)

	_, err = fetcher.Fetch(t.Context(), "example.com/rewards/missing", "v1.2.0")
	require.ErrorIs(t, err, modfetch.ErrNotFound)
	require.ErrorContains(t, err, "no module provides the contract package example.com/rewards/missing at v1.2.0: "+
		"example.com/rewards/missing@v1.2.0: ")
	require.ErrorContains(t, err, "; example.com/rewards@v1.2.0: the module has no directory for the package "+
		"example.com/rewards/missing; example.com@v1.2.0: ")

	_, err = fetcher.Fetch(t.Context(), "example.com/rewards/cohort", "not-a-version")
	require.ErrorIs(t, err, modfetch.ErrNotFound)
	require.ErrorContains(t, err, "example.com/rewards@not-a-version: ")
}

// TestFetchNeedsTheGoCommand fetches with a PATH that has no go command, then one whose go is not executable.
func TestFetchNeedsTheGoCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fetcher := modfetch.Fetcher{Environ: []string{"PATH=" + string(filepath.ListSeparator) + dir}}

	_, err := fetcher.Fetch(t.Context(), "example.com/rewards/cohort", "v1.2.0")
	require.ErrorIs(t, err, modfetch.ErrNoGo)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\n"), 0o600))

	_, err = fetcher.Fetch(t.Context(), "example.com/rewards/cohort", "v1.2.0")
	require.ErrorIs(t, err, modfetch.ErrNoGo, "a go that is not executable is not the go command")
}

// fakeGo writes an executable go command running script to a temporary directory and returns that directory. Its
// callers are not parallel: a parallel test forking while the script is open for writing would keep it open in
// the child until it execs, and running the script then fails with "text file busy".
func fakeGo(t *testing.T, script string) string {
	t.Helper()

	dir := t.TempDir()
	//nolint:gosec // G306: the fake go command must be executable
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\n"+script+"\n"), 0o700))

	return dir
}

// TestFetchReportsAGoCommandThatFails runs a go command that fails without printing JSON.
//
//nolint:paralleltest // see fakeGo
func TestFetchReportsAGoCommandThatFails(t *testing.T) {
	fetcher := modfetch.Fetcher{Environ: []string{"PATH=" + fakeGo(t, "echo broken >&2\nexit 3")}}

	_, err := fetcher.Fetch(t.Context(), "example.com/rewards", "v1.2.0")
	require.ErrorIs(t, err, modfetch.ErrNotFound)
	require.ErrorContains(t, err, "example.com/rewards@v1.2.0: go mod download failed: exit status 3: broken")
}

// TestFetchReportsUnusableGoOutput runs go commands whose JSON resolves nothing: one that fails without the go
// command's error field, and one that succeeds without a module directory.
//
//nolint:paralleltest // see fakeGo
func TestFetchReportsUnusableGoOutput(t *testing.T) {
	for name, script := range map[string]string{
		"go mod download failed: exit status 1: denied": `echo '{"Path":"example.com/rewards"}'; echo denied >&2; exit 1`,
		"go mod download gave no module directory":      `echo '{"Path":"example.com/rewards","Version":"v1.2.0"}'`,
	} {
		fetcher := modfetch.Fetcher{Environ: []string{"PATH=" + fakeGo(t, script)}}

		_, err := fetcher.Fetch(t.Context(), "example.com/rewards", "v1.2.0")
		require.ErrorIs(t, err, modfetch.ErrNotFound)
		require.ErrorContains(t, err, "example.com/rewards@v1.2.0: "+name)
	}
}

// TestFetchNeedsATemporaryDirectory fetches with TMPDIR naming no directory: the go command's empty working
// directory cannot be made.
func TestFetchNeedsATemporaryDirectory(t *testing.T) {
	dir := fakeGo(t, "")
	t.Setenv("TMPDIR", filepath.Join(dir, "gone"))

	fetcher := modfetch.Fetcher{Environ: []string{"PATH=" + dir}}

	_, err := fetcher.Fetch(t.Context(), "example.com/rewards", "v1.2.0")
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorContains(t, err, "modfetch: ")
}

// TestRequiredReadsTheContractRequirement reads what go.mod files require of the contract module: the nearest
// go.mod above the plugin's directory, its replace directives by version and by directory, and the failures.
func TestRequiredReadsTheContractRequirement(t *testing.T) {
	t.Parallel()

	write := func(dir, goMod string) string {
		t.Helper()

		require.NoError(t, os.MkdirAll(filepath.Join(dir, "plugin"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600))

		return filepath.Join(dir, "plugin")
	}

	const head = "module example.com/p\n\ngo 1.27\n\nrequire example.com/rewards v1.2.0\n"

	tests := []struct {
		name, goMod string
		want        modfetch.Requirement
	}{
		{"required", head, modfetch.Requirement{Version: "v1.2.0"}},
		{
			"replaced by a version", head + "replace example.com/rewards => example.com/fork v1.2.5\n",
			modfetch.Requirement{Version: "v1.2.5"},
		},
		{
			"replaced for another version", head + "replace example.com/rewards v1.1.0 => example.com/fork v1.2.5\n",
			modfetch.Requirement{Version: "v1.2.0"},
		},
		{
			"replaced by a relative directory", head + "replace example.com/rewards => ../rewards\n",
			modfetch.Requirement{Dir: "../rewards"},
		},
		{
			"replaced by an absolute directory", head + "replace example.com/rewards => /src/rewards\n",
			modfetch.Requirement{Dir: "/src/rewards"},
		},
	}

	for _, tc := range tests {
		dir := t.TempDir()

		got, err := modfetch.Required(write(dir, tc.goMod), "example.com/rewards")
		require.NoError(t, err, tc.name)

		tc.want.GoMod = filepath.Join(dir, "go.mod")
		if tc.want.Dir != "" && !filepath.IsAbs(tc.want.Dir) {
			tc.want.Dir = filepath.Join(dir, tc.want.Dir)
		}

		assert.Equal(t, tc.want, got, tc.name)
	}

	_, err := modfetch.Required(write(t.TempDir(), "module example.com/p\n"), "example.com/rewards")
	require.ErrorIs(t, err, modfetch.ErrGoMod)
	require.ErrorContains(t, err, "does not require example.com/rewards")

	_, err = modfetch.Required(write(t.TempDir(), "not a go.mod\n"), "example.com/rewards")
	require.ErrorContains(t, err, "unknown directive: not")

	_, err = modfetch.Required(t.TempDir(), "example.com/rewards")
	require.ErrorIs(t, err, modfetch.ErrGoMod, "no go.mod above a temporary directory")
	require.ErrorContains(t, err, "no go.mod in ")

	unreadable := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(unreadable, "go.mod"), 0o750))

	_, err = modfetch.Required(unreadable, "example.com/rewards")
	require.ErrorContains(t, err, "is a directory", "a go.mod that is a directory cannot be read")
}

// TestImportPathJoinsTheModulePath returns a directory's import path from the nearest go.mod at or above it, and
// fails without a go.mod or a module path.
func TestImportPathJoinsTheModulePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "lib", "deep"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/repo\n"), 0o600))

	for sub, want := range map[string]string{
		".": "example.com/repo", "lib": "example.com/repo/lib", filepath.Join("lib", "deep"): "example.com/repo/lib/deep",
	} {
		got, err := modfetch.ImportPath(filepath.Join(dir, sub))
		require.NoError(t, err, sub)
		assert.Equal(t, want, got, sub)
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("go 1.27\n"), 0o600))

	_, err := modfetch.ImportPath(filepath.Join(dir, "lib"))
	require.ErrorIs(t, err, modfetch.ErrGoMod)
	require.ErrorContains(t, err, filepath.Join(dir, "go.mod")+" declares no module path")

	_, err = modfetch.ImportPath(t.TempDir())
	require.ErrorIs(t, err, modfetch.ErrGoMod)
	require.ErrorContains(t, err, "no go.mod in ")
}
