//go:build integration

package buildguest_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/buildguest"
	"github.com/heynemann/go-plg/internal/testproxy"
)

// service returns a service module requiring this checkout (with plg's own go.sum, so no network is needed)
// and the greet test module through a version-specific replacement relative to its go.mod.
func service(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	files := os.DirFS(root)

	gomod, err := fs.ReadFile(files, "go.mod")
	require.NoError(t, err)

	gosum, err := fs.ReadFile(files, "go.sum")
	require.NoError(t, err)

	greet, err := fs.ReadFile(files, "testdata/buildguest/greet/greet.go")
	require.NoError(t, err)

	modfile := strings.Replace(string(gomod), "module github.com/heynemann/go-plg", "module example.com/svc", 1) +
		"\nrequire (\n\tgithub.com/heynemann/go-plg v0.0.0\n\texample.com/greet v0.0.0\n)\n\n" +
		"replace github.com/heynemann/go-plg => " + root + "\n\nreplace example.com/greet v0.0.0 => ./third/greet\n"

	return module(t, map[string]string{
		"go.mod":                modfile,
		"go.sum":                string(gosum),
		"third/greet/go.mod":    "module example.com/greet\n\ngo 1.27\n",
		"third/greet/greet.go":  string(greet),
		"third/greet/a-b/ab.go": "package ab\n\n// Dashed is in example.com/greet/a-b.\nfunc Dashed() string { return \"a-b\" }\n",
		"third/greet/a/b/b.go":  "package b\n\n// Nested is in example.com/greet/a/b.\nfunc Nested() string { return \"a/b\" }\n",
	})
}

func TestBuildFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want   error
		name   string
		msg    string
		output string
		pkgs   []string
	}{
		{
			name: "package of no required module", pkgs: []string{"example.com/elsewhere/pkg"},
			want: buildguest.ErrModule, msg: "no module it requires provides example.com/elsewhere/pkg",
		},
		{
			// The relative, version-specific replacement resolves from the temporary module: greet is found,
			// and only the missing package fails.
			name: "package yaegi cannot extract", pkgs: []string{"example.com/greet", "nosuch/pkg"},
			want: buildguest.ErrExtract, msg: "nosuch/pkg",
		},
		{
			// Both would extract to example_com-greet-a-b.go: the second would overwrite the first.
			name: "packages extracted to the same file",
			pkgs: []string{"example.com/greet/a-b", "example.com/greet/a/b"},
			want: buildguest.ErrPackages,
			msg:  "example.com/greet/a-b and example.com/greet/a/b to the same file, example_com-greet-a-b.go",
		},
		{
			name: "output the go command cannot write", pkgs: []string{"example.com/greet"},
			output: filepath.Join("go.mod", "guest.wasm"), want: buildguest.ErrGo, msg: "go build", // below a file
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := service(t)

			output := tc.output
			if output == "" {
				output = "guest.wasm"
			}

			err := buildguest.Build(t.Context(), &buildguest.Options{Dir: dir, Output: output, Packages: tc.pkgs})
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, tc.msg)
			assert.NoFileExists(t, filepath.Join(dir, output))
		})
	}
}

// TestKeyFollowsWhatTheGuestIsBuiltFrom: the key of a guest changes with its packages and their order and with
// the content of a module replaced by a directory that it links (greet), and with nothing else: a guest not
// linking greet keeps its key when greet changes.
func TestKeyFollowsWhatTheGuestIsBuiltFrom(t *testing.T) {
	t.Parallel()

	dir := service(t)
	key := func(pkgs ...string) string {
		t.Helper()

		k, err := buildguest.Key(t.Context(), &buildguest.Options{Dir: dir, Packages: pkgs})
		require.NoError(t, err)
		require.Regexp(t, `^[0-9a-f]{64}$`, k)

		return k
	}

	xml, both := key("encoding/xml"), key("encoding/xml", "example.com/greet")
	assert.Equal(t, xml, key("encoding/xml"), "the same inputs, the same key")
	assert.NotEqual(t, xml, both)
	assert.NotEqual(t, both, key("example.com/greet", "encoding/xml"), "the packages' order is the build's")

	greet := filepath.Join(dir, "third", "greet", "greet.go")
	src, err := os.ReadFile(greet) //nolint:gosec // G304: the test's own module
	require.NoError(t, err)
	//nolint:gosec // G703: greet.go of the test's own temporary module
	require.NoError(t, os.WriteFile(greet, append(src, "\n// Changed.\n"...), 0o600))

	assert.NotEqual(t, both, key("encoding/xml", "example.com/greet"), "greet's content changed")
	assert.Equal(t, xml, key("encoding/xml"), "this guest does not link greet")
}

// TestCachedFailsWithoutACacheDirectory: a cache that cannot be created fails before anything is built.
func TestCachedFailsWithoutACacheDirectory(t *testing.T) {
	t.Parallel()

	dir := service(t)
	cache := filepath.Join(dir, "go.mod", "guests") // below a file

	_, err := buildguest.Cached(t.Context(), &buildguest.Options{Dir: dir, Packages: []string{"encoding/xml"}},
		cache)
	require.ErrorContains(t, err, "creating the guest cache")
}

// TestKeyNamesModuleVersions: a package of a module version is keyed by that version (its replacement's when the
// module is replaced by another version), which is immutable, not by the content of the module cache's copy.
func TestKeyNamesModuleVersions(t *testing.T) {
	t.Parallel()

	modules := t.TempDir()
	lib := filepath.Join(modules, "example.com", "lib@v1.1.0")
	require.NoError(t, os.MkdirAll(lib, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(lib, "go.mod"), []byte("module example.com/lib\n\ngo 1.27\n"), 0o600))
	require.NoError(
		t,
		os.WriteFile(filepath.Join(lib, "lib.go"), []byte("package lib\n\n// V is lib's.\nconst V = 1\n"),
			0o600),
	)

	// plg's own dependencies come from the go command's module cache.
	cache := t.TempDir()
	environ := testproxy.Environ(testproxy.New(t, modules)+","+testproxy.ModuleCache(t), cache)
	dir := service(t)
	gomod := filepath.Join(dir, "go.mod")

	original, err := os.ReadFile(gomod) //nolint:gosec // G304: the test's own module
	require.NoError(t, err)

	//nolint:gosec // G703: the test's own module
	require.NoError(t, os.WriteFile(gomod, append(original, "\nrequire example.com/lib v1.0.0\n\n"+
		"replace example.com/lib v1.0.0 => example.com/lib v1.1.0\n"...), 0o600))

	opts := &buildguest.Options{Dir: dir, Packages: []string{"example.com/lib"}, Environ: environ}

	key, err := buildguest.Key(t.Context(), opts)
	require.NoError(t, err)

	cached := filepath.Join(cache, "example.com", "lib@v1.1.0", "lib.go")
	require.FileExists(t, cached, "the module was fetched at its replacement's version")
	require.NoError(t, os.Chmod(cached, 0o600)) // the module cache's files are read-only
	require.NoError(t, os.WriteFile(cached, []byte("package lib\n\n// V is changed.\nconst V = 2\n"), 0o600))

	again, err := buildguest.Key(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, key, again)

	_, err = buildguest.Cached(t.Context(), &buildguest.Options{
		Dir: dir, Packages: []string{"example.com/lib/nosuch"}, Environ: environ,
	}, t.TempDir())
	require.ErrorIs(t, err, buildguest.ErrGo, "a package the module does not have")
	require.ErrorContains(t, err, "go list -deps")
}

// TestCachedLeavesNothingOnAFailedBuild: a guest whose build fails (an internal package, which the guest's
// package of symbols may not import) leaves no file in the cache, so no later run finds a broken guest.
func TestCachedLeavesNothingOnAFailedBuild(t *testing.T) {
	t.Parallel()

	dir := service(t)
	internal := filepath.Join(dir, "third", "greet", "internal", "x")
	require.NoError(t, os.MkdirAll(internal, 0o750))
	require.NoError(
		t,
		os.WriteFile(filepath.Join(internal, "x.go"), []byte("package x\n\n// F is internal.\nfunc F() {}\n"),
			0o600),
	)

	cache := t.TempDir()

	_, err := buildguest.Cached(t.Context(), &buildguest.Options{
		Dir: dir, Packages: []string{"example.com/greet/internal/x"},
	}, cache)
	require.ErrorIs(t, err, buildguest.ErrGo)
	require.ErrorContains(t, err, "use of internal package example.com/greet/internal/x not allowed")

	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
