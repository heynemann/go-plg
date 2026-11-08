// Package testproxy serves module versions from a directory as a file-based GOPROXY, so tests resolve modules
// through the real go command without network.
package testproxy

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/mod/module"
	"golang.org/x/mod/zip"
)

// Modes of the proxy's directories and files.
const (
	dirMode  = 0o750
	fileMode = 0o600
)

// New lays out, in a temporary directory of tb, a GOPROXY serving every module version under dir (each a
// directory <module path>@<version> holding the module's files, go.mod at its root) and returns its file:// URL.
func New(tb testing.TB, dir string) string {
	tb.Helper()

	proxy := tb.TempDir()
	versions := map[string][]string{} // escaped module path -> its versions

	err := filepath.WalkDir(dir, func(name string, entry os.DirEntry, err error) error {
		if err != nil || !entry.IsDir() || !strings.Contains(entry.Name(), "@") {
			return err
		}

		rel, err := filepath.Rel(dir, name)
		require.NoError(tb, err)

		path, version, _ := strings.Cut(filepath.ToSlash(rel), "@")
		escaped, err := module.EscapePath(path)
		require.NoError(tb, err)

		versionDir := filepath.Join(proxy, filepath.FromSlash(escaped), "@v")
		require.NoError(tb, os.MkdirAll(versionDir, dirMode))

		goMod, err := os.ReadFile(filepath.Join(name, "go.mod")) //nolint:gosec // G304: the test's testdata
		require.NoError(tb, err)

		var archive bytes.Buffer

		require.NoError(tb, zip.CreateFromDir(&archive, module.Version{Path: path, Version: version}, name))

		for ext, data := range map[string][]byte{
			".mod":  goMod,
			".info": []byte(`{"Version":"` + version + `","Time":"2026-01-01T00:00:00Z"}`),
			".zip":  archive.Bytes(),
		} {
			require.NoError(tb, os.WriteFile(filepath.Join(versionDir, version+ext), data, fileMode))
		}

		versions[escaped] = append(versions[escaped], version)

		return filepath.SkipDir
	})
	require.NoError(tb, err)

	for escaped, list := range versions {
		slices.Sort(list)
		require.NoError(tb, os.WriteFile(filepath.Join(proxy, filepath.FromSlash(escaped), "@v", "list"),
			[]byte(strings.Join(list, "\n")+"\n"), fileMode))
	}

	return "file://" + filepath.ToSlash(proxy)
}

// Environ returns the process's environment without its Go settings, then the go command's settings to resolve
// modules through proxy (a GOPROXY, or "off") into the module cache cache, with no checksum database and the local
// toolchain. The cache's files are writable (-modcacherw), so a temporary directory holding it can be removed.
func Environ(proxy, cache string) []string {
	environ := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GO") })

	return append(environ, "GOPROXY="+proxy, "GOMODCACHE="+cache, "GOFLAGS=-modcacherw", "GOSUMDB=off",
		"GOTOOLCHAIN=local", "GONOSUMDB=", "GOPRIVATE=")
}

// ModuleCache returns the download directory of the go command's module cache as a GOPROXY (file://…/cache/download,
// which serves every module version the cache holds): after [New]'s proxy, a fallback that resolves this module's
// own dependencies (which a custom Go guest's build needs) without network.
func ModuleCache(tb testing.TB) string {
	tb.Helper()

	out, err := exec.CommandContext(tb.Context(), "go", "env", "GOMODCACHE").Output()
	require.NoError(tb, err)

	return "file://" + filepath.ToSlash(filepath.Join(strings.TrimSpace(string(out)), "cache", "download"))
}
