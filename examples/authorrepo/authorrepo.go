// Package authorrepo is what the tests of author repositories share (the example one, examples/rules, and the
// rewards platform's teams under e2e/teams): the plg command built from this checkout, contract modules served by
// a file-based GOPROXY at the versions plugins pin, and a repository's CI script run in a copy of it, so the tests
// drive the real commands without network.
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

// contractModule is the module of the contract the example repository's plugins implement, at the version they pin.
const (
	contractModule  = "github.com/heynemann/go-plg/examples"
	contractVersion = "v1.0.0"
)

// Examples returns the directory of the examples module.
func Examples(tb testing.TB) string {
	tb.Helper()

	_, file, _, ok := runtime.Caller(0)
	require.True(tb, ok)

	return filepath.Join(filepath.Dir(file), "..")
}

// Command builds the plg command from this checkout into a directory of tb and returns that directory, to be put
// first on the PATH.
func Command(tb testing.TB) string {
	tb.Helper()

	bin := tb.TempDir()

	//nolint:gosec // G204: builds this checkout's plg command
	build := exec.CommandContext(tb.Context(), "go", "build", "-o", filepath.Join(bin, "plg"), "./cmd/plg")
	build.Dir = filepath.Join(Examples(tb), "..")
	out, err := build.CombinedOutput()
	require.NoError(tb, err, string(out))

	return bin
}

// ProxyEnviron returns the environment an author repository's commands run in: bin (Command's) first on the PATH,
// and the go command's settings resolving every module version under modules (each a directory <module
// path>@<version> holding the module's files, go.mod at its root) through a GOPROXY of tb, into a module cache of
// tb.
func ProxyEnviron(tb testing.TB, bin, modules string) []string {
	tb.Helper()

	environ := testproxy.Environ(testproxy.New(tb, modules), tb.TempDir())
	for i, kv := range environ {
		if path, ok := strings.CutPrefix(kv, "PATH="); ok {
			environ[i] = "PATH=" + bin + string(os.PathListSeparator) + path
		}
	}

	return environ
}

// Environ builds the plg command from this checkout and returns the environment the example repository's commands
// run in (ProxyEnviron's), resolving the contract module (contractModule at contractVersion: the examples' pricing
// package).
func Environ(tb testing.TB) []string {
	tb.Helper()

	modules := tb.TempDir()
	module := filepath.Join(modules, filepath.FromSlash(contractModule)+"@"+contractVersion)
	require.NoError(tb, os.MkdirAll(filepath.Join(module, "pricing"), dirMode))
	require.NoError(tb, os.WriteFile(filepath.Join(module, "go.mod"),
		[]byte("module "+contractModule+"\n\ngo 1.27\n"), fileMode))

	source := filepath.Join(Examples(tb), "pricing", "pricing.go")
	contract, err := os.ReadFile(source) //nolint:gosec // G304: the contract
	require.NoError(tb, err)
	//nolint:gosec // G703: a path of the test's own temporary directory
	require.NoError(tb, os.WriteFile(filepath.Join(module, "pricing", "pricing.go"), contract, fileMode))

	return ProxyEnviron(tb, Command(tb), modules)
}

// Copy copies the example author repository into dir (its own directory, such as <dir>/examples/rules), as
// CopyRepository does.
func Copy(tb testing.TB, dir string) {
	tb.Helper()

	CopyRepository(tb, filepath.Join(Examples(tb), "rules"), dir, "The pricing rules")
}

// CopyRepository copies the author repository in src into dir, leaving out what its CI script writes (dist and
// reports), and commits it with message to a git repository of its own, as an author's repository is.
func CopyRepository(tb testing.TB, src, dir, message string) {
	tb.Helper()

	require.NoError(tb, os.CopyFS(dir, os.DirFS(src)))
	require.NoError(tb, os.RemoveAll(filepath.Join(dir, "dist")))
	require.NoError(tb, os.RemoveAll(filepath.Join(dir, "reports")))
	git(tb, dir, "init", "-q")
	Commit(tb, dir, message)
}

// Commit commits every change of the author repository in dir (Copy's or CopyRepository's) with message, as its
// author would.
func Commit(tb testing.TB, dir, message string) {
	tb.Helper()

	git(tb, dir, "add", "-A")
	git(tb, dir, "-c", "user.name=Ana Lima", "-c", "user.email=ana@example.com", "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", message)
}

// git runs git with args in dir, without the user's or the system's configuration.
func git(tb testing.TB, dir string, args ...string) {
	tb.Helper()

	cmd := exec.CommandContext(tb.Context(), "git", args...) //nolint:gosec // G204: the fixed commands of this file
	cmd.Dir = dir

	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	require.NoError(tb, err, string(out))
}

// Pack runs the repository's CI script in a copy of it, as its CI would, and returns the directory of the
// artifacts it packed.
func Pack(tb testing.TB) string {
	tb.Helper()

	repo := tb.TempDir()
	Copy(tb, repo)

	return CI(tb, repo, Environ(tb))
}

// CI runs the CI script of the author repository in dir (Copy's or CopyRepository's) in environ (Environ's or
// ProxyEnviron's), as its CI would, and returns the directory of the artifacts it packed: run again after a Commit,
// it packs the repository anew.
func CI(tb testing.TB, dir string, environ []string) string {
	tb.Helper()

	ci := exec.CommandContext(tb.Context(), "./ci.sh")
	ci.Dir = dir
	ci.Env = environ
	out, err := ci.CombinedOutput()
	require.NoError(tb, err, string(out))

	return filepath.Join(dir, "dist")
}
