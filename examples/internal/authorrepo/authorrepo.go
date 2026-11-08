// Package authorrepo is what the tests of the example author repository (examples/rules) share: the plg command
// built from this checkout, the pricing contract served by a file-based GOPROXY as the version its plugins pin,
// and the repository's CI script run in a copy of it, so the tests drive the real commands without network.
package authorrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/testproxy"
)

// Modes of the files the tests write.
const (
	dirMode  = 0o750
	fileMode = 0o600
)

// contractModule is the module of the contract the repository's plugins implement, at the version they pin.
const (
	contractModule  = "github.com/heynemann/go-plg/examples"
	contractVersion = "v1.0.0"
)

// Examples returns the directory of the examples module.
func Examples(tb testing.TB) string {
	tb.Helper()

	_, file, _, ok := runtime.Caller(0)
	require.True(tb, ok)

	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Environ builds the plg command from this checkout into a directory of tb put first on the PATH, and returns the
// environment the repository's commands run in: that PATH, and the go command's settings resolving the contract
// module (contractModule at contractVersion: the examples' pricing package) through a GOPROXY of tb, into a
// module cache of tb.
func Environ(tb testing.TB) []string {
	tb.Helper()

	examples := Examples(tb)
	bin := tb.TempDir()

	//nolint:gosec // G204: builds this checkout's plg command
	build := exec.CommandContext(tb.Context(), "go", "build", "-o", filepath.Join(bin, "plg"), "./cmd/plg")
	build.Dir = filepath.Join(examples, "..")
	out, err := build.CombinedOutput()
	require.NoError(tb, err, string(out))

	modules := tb.TempDir()
	module := filepath.Join(modules, filepath.FromSlash(contractModule)+"@"+contractVersion)
	require.NoError(tb, os.MkdirAll(filepath.Join(module, "pricing"), dirMode))
	require.NoError(tb, os.WriteFile(filepath.Join(module, "go.mod"),
		[]byte("module "+contractModule+"\n\ngo 1.27\n"), fileMode))

	contract, err := os.ReadFile(filepath.Join(examples, "pricing", "pricing.go")) //nolint:gosec // G304: the contract
	require.NoError(tb, err)
	//nolint:gosec // G703: a path of the test's own temporary directory
	require.NoError(tb, os.WriteFile(filepath.Join(module, "pricing", "pricing.go"), contract, fileMode))

	environ := testproxy.Environ(testproxy.New(tb, modules), tb.TempDir())
	for i, kv := range environ {
		if path, ok := strings.CutPrefix(kv, "PATH="); ok {
			environ[i] = "PATH=" + bin + string(os.PathListSeparator) + path
		}
	}

	return environ
}

// Copy copies the example author repository into dir (its own directory, such as <dir>/examples/rules), leaving
// out what its CI script writes, and commits it to a git repository of its own, as an author's repository is.
func Copy(tb testing.TB, dir string) {
	tb.Helper()

	require.NoError(tb, os.CopyFS(dir, os.DirFS(filepath.Join(Examples(tb), "rules"))))
	require.NoError(tb, os.RemoveAll(filepath.Join(dir, "dist")))
	require.NoError(tb, os.RemoveAll(filepath.Join(dir, "reports")))

	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{
			"-c", "user.name=Ana Lima", "-c", "user.email=ana@example.com", "-c", "commit.gpgsign=false",
			"commit", "-q", "-m", "The pricing rules",
		},
	} {
		git := exec.CommandContext(tb.Context(), "git", args...) //nolint:gosec // G204: the fixed commands above
		git.Dir = dir

		git.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := git.CombinedOutput()
		require.NoError(tb, err, string(out))
	}
}

// Pack runs the repository's CI script in a copy of it, as its CI would, and returns the directory of the
// artifacts it packed.
func Pack(tb testing.TB) string {
	tb.Helper()

	repo := tb.TempDir()
	Copy(tb, repo)

	ci := exec.CommandContext(tb.Context(), "./ci.sh")
	ci.Dir = repo
	ci.Env = Environ(tb)
	out, err := ci.CombinedOutput()
	require.NoError(tb, err, string(out))

	return filepath.Join(repo, "dist")
}
