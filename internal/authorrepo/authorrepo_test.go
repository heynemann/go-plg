package authorrepo_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/authorrepo"
	"github.com/heynemann/go-plg/internal/directive"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// write creates the files (path relative to dir: content; a path ending in / is a directory) in dir.
func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			require.NoError(t, os.MkdirAll(full, 0o750))

			continue
		}

		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
}

// repoDir returns a repository with plg.yaml, plugins a, b/c and shared lib, plus the given files.
func repoDir(t *testing.T, plgYAML string, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	write(t, dir, map[string]string{"a/": "", "b/c/": "", "lib/": "", authorrepo.FileName: plgYAML})
	write(t, dir, files)

	return dir
}

func TestLoadReadsEverySetting(t *testing.T) {
	t.Parallel()

	checkout := t.TempDir()
	write(t, checkout, map[string]string{"go.mod": "module example.com/rewards\n"})

	dir := repoDir(t, `
plugins: [a, ./b/c/]
shared: [lib]
limits: {memory: 32MiB, timeout: 2s, pool: 2}
requireOwnership: true
replace:
  example.com/rewards: `+checkout+`
  example.com/other: ../other
`, map[string]string{"../other/go.mod": "module example.com/other\n"})

	repo, err := authorrepo.Load(dir)
	require.NoError(t, err)

	abs, err := filepath.Abs(dir)
	require.NoError(t, err)

	assert.Equal(t, &authorrepo.Repo{
		Dir:     abs,
		Plugins: []string{"a", "b/c"},
		Shared:  []string{"lib"},
		Replace: []authorrepo.Replace{
			{Path: "example.com/other", Dir: filepath.Join(filepath.Dir(abs), "other")},
			{Path: "example.com/rewards", Dir: checkout},
		},
		Limits:           directive.Limits{Memory: 32 << 20, Timeout: 2 * time.Second, PoolMin: 2, PoolMax: 2},
		RequireOwnership: true,
	}, repo)
	assert.Equal(t, filepath.Join(abs, "plg.yaml"), repo.File())

	minimal, err := authorrepo.Load(repoDir(t, "plugins: [a]\n", nil))
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, minimal.Plugins)
	assert.Empty(t, minimal.Shared)
	assert.Empty(t, minimal.Replace)
	assert.Zero(t, minimal.Limits)
	assert.False(t, minimal.RequireOwnership)
}

func TestLoadRejectsBrokenRules(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, plgYAML, want string }{
		{"no plugins", "shared: [lib]\n", "plugins must list at least one plugin directory"},
		{"unknown key", "plugins: [a]\nshare: [lib]\n", "field share not found"},
		{"duplicate key", "plugins: [a]\nplugins: [a]\n", `duplicate key "plugins"`},
		{"anchor", "plugins: &p [a]\n", "anchors are not allowed"},
		{"absolute plugin", "plugins: [/a]\n", `plugins: "/a" is not a directory inside the repository`},
		{"escaping plugin", "plugins: [../a]\n", `plugins: "../a" is not a directory inside the repository`},
		{"root as a plugin", "plugins: [.]\n", `plugins: "." is not a directory inside the repository`},
		{"empty shared", "plugins: [a]\nshared: ['']\n", `shared: "" is not a directory inside the repository`},
		{"plugin twice", "plugins: [a, a/]\n", "plugins: a is listed twice"},
		{"missing plugin", "plugins: [nope]\n", "plugins: stat "},
		{"file as a plugin", "plugins: [f]\n", "plugins: f is not a directory"},
		{"plugin inside shared", "plugins: [lib/p]\nshared: [lib]\n", "lib/p and lib: a plugin or shared " +
			"directory cannot be inside another"},
		{"shared inside a plugin", "plugins: [a]\nshared: [a/lib]\n", "a and a/lib: a plugin or shared " +
			"directory cannot be inside another"},
		{"shared twice nested", "plugins: [a]\nshared: [lib, lib/x]\n", "lib and lib/x"},
		{"same key", "plugins: [c, b/c]\n", `plugins: c and b/c have the same key (a plugin's key is its ` +
			`directory's name): "c"`},
		{"unknown limit", "plugins: [a]\nlimits: {cpu: 1}\n", `limits: invalid plg directive: unknown limit "cpu"`},
		{"bad memory", "plugins: [a]\nlimits: {memory: 1KiB}\n", "limits: invalid plg directive: memory must be " +
			"from 64KiB to 4GiB"},
		{"bad module path", "plugins: [a]\nreplace: {'not a path': x}\n", "replace: malformed module path"},
		{"replace without a directory", "plugins: [a]\nreplace: {example.com/x: ''}\n", "replace: " +
			"example.com/x needs a directory"},
		{"replace without go.mod", "plugins: [a]\nreplace: {example.com/x: lib}\n", "replace: example.com/x: " +
			"the directory must hold the module's go.mod: stat "},
		{"replace by another module", "plugins: [a]\nreplace: {example.com/x: other}\n", `declares another ` +
			`module: "example.com/y", not example.com/x`},
	}

	// The files each repository has beside plugins a and b/c and the directory lib.
	files := map[string]string{
		"f": "x", "lib/p/": "", "a/lib/": "", "lib/x/": "", "c/": "", "other/go.mod": "module example.com/y\n",
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := authorrepo.Load(repoDir(t, tc.plgYAML, files))
			require.ErrorIs(t, err, authorrepo.ErrInvalid)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLoadNeedsARegularFileOfBoundedSize(t *testing.T) {
	t.Parallel()

	_, err := authorrepo.Load(t.TempDir())
	require.ErrorIs(t, err, authorrepo.ErrInvalid)
	require.ErrorIs(t, err, os.ErrNotExist)

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, authorrepo.FileName), 0o750))
	assert.True(t, authorrepo.Exists(dir), "a plg.yaml of any kind is the repository's, which Load then rejects")

	_, err = authorrepo.Load(dir)
	require.ErrorContains(t, err, "plg.yaml is not a regular file")

	big := repoDir(t, "plugins: [a]\n#"+strings.Repeat("x", plg.MaxManifestSize)+"\n", nil)
	_, err = authorrepo.Load(big)
	require.ErrorContains(t, err, "plg.yaml is over 65536 bytes")
}

func TestFindLooksUpToTheRepositoryRoot(t *testing.T) {
	t.Parallel()

	dir := repoDir(t, "plugins: [b/c]\n", map[string]string{"b/c/d/": "", "b/c/plg.yaml": "not read"})

	repo, err := authorrepo.Find(filepath.Join(dir, "b", "c"))
	require.NoError(t, err)
	require.NotNil(t, repo, "found above the plugin's directory, never in it")

	rel, listed := repo.Lists(filepath.Join(dir, "b", "c"))
	assert.True(t, listed)
	assert.Equal(t, "b/c", rel)

	rel, listed = repo.Lists(filepath.Join(dir, "a"))
	assert.False(t, listed, "a is in the repository, not listed")
	assert.Equal(t, "a", rel)

	outer := repoDir(t, "plugins: [a]\n", map[string]string{"inner/.git": "gitdir: elsewhere", "inner/p/": ""})

	repo, err = authorrepo.Find(filepath.Join(outer, "inner", "p"))
	require.NoError(t, err)
	assert.Nil(t, repo, "the search stops at the directory holding .git: the plg.yaml above it is another's")

	broken := repoDir(t, "plugins: []\n", nil)
	_, err = authorrepo.Find(filepath.Join(broken, "a"))
	require.ErrorIs(t, err, authorrepo.ErrInvalid, "a plg.yaml found is read")
}

func TestReplacementPicksTheLongestModulePath(t *testing.T) {
	t.Parallel()

	repo := &authorrepo.Repo{Replace: []authorrepo.Replace{
		{Path: "example.com/r", Dir: "/r"}, {Path: "example.com/r/v2", Dir: "/v2"},
	}}

	for pkg, want := range map[string]string{
		"example.com/r": "/r", "example.com/r/cohort": "/r", "example.com/r/v2/cohort": "/v2", "example.com/rx": "",
	} {
		got, ok := repo.Replacement(pkg)
		assert.Equal(t, want != "", ok, pkg)
		assert.Equal(t, want, got.Dir, pkg)
	}
}

func TestComposeCopiesSharedDirectoriesIntoAPlugin(t *testing.T) {
	t.Parallel()

	dir := repoDir(t, "plugins: [a, b/c]\nshared: [lib, ui/kit]\n", map[string]string{
		"go.mod":         "module example.com/repo\n",
		"a/plugin.lua":   "p",
		"a/sub/x.lua":    "x",
		"lib/tiers.go":   "package lib\n",
		"lib/tiers.lua":  "tiers",
		"lib/deep/d.lua": "d",
		"ui/kit/k.ts":    "k",
		"b/c/plugin.go":  "package c\n",
	})
	require.NoError(t, os.Symlink("tiers.lua", filepath.Join(dir, "lib", "link.lua")))
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "lib", "fifo"), 0o600))

	repo, err := authorrepo.Load(dir)
	require.NoError(t, err)

	script := t.TempDir()
	placed, err := repo.Compose(script, "a", "")
	require.NoError(t, err)
	assert.Equal(t, []authorrepo.Placement{{Shared: "lib", At: "lib"}, {Shared: "ui/kit", At: "ui/kit"}}, placed)
	assertFiles(t, script, map[string]string{
		"plugin.lua": "p", "sub/x.lua": "x", "lib/tiers.go": "package lib\n", "lib/tiers.lua": "tiers",
		"lib/deep/d.lua": "d", "ui/kit/k.ts": "k",
	})

	_, err = os.Lstat(filepath.Join(script, "lib", "fifo"))
	require.ErrorIs(t, err, os.ErrNotExist, "irregular files are left out")

	link, err := os.Readlink(filepath.Join(script, "lib", "link.lua"))
	require.NoError(t, err)
	assert.Equal(t, "tiers.lua", link, "links are copied as links")

	golang := t.TempDir()
	placed, err = repo.Compose(golang, "b/c", "pkg")
	require.NoError(t, err)
	assert.Equal(t, []authorrepo.Placement{{Shared: "lib", At: "pkg/vendor/example.com/repo/lib"}}, placed,
		"Go plugins vendor the shared directories holding Go files, at their import paths")
	assertFiles(t, golang, map[string]string{
		"plugin.go": "package c\n", "pkg/vendor/example.com/repo/lib/tiers.go": "package lib\n",
		"pkg/vendor/example.com/repo/lib/tiers.lua": "tiers", "pkg/vendor/example.com/repo/lib/deep/d.lua": "d",
	})
}

// assertFiles checks that the regular files below dir are want's.
func assertFiles(t *testing.T, dir string, want map[string]string) {
	t.Helper()

	got := map[string]string{}

	require.NoError(t, filepath.WalkDir(dir, func(name string, entry os.DirEntry, err error) error {
		require.NoError(t, err)

		if entry.Type().IsRegular() {
			data, err := os.ReadFile(name) //nolint:gosec // G304: the test's own files
			require.NoError(t, err)

			rel, err := filepath.Rel(dir, name)
			require.NoError(t, err)

			got[filepath.ToSlash(rel)] = string(data)
		}

		return nil
	}))
	assert.Equal(t, want, got)
}

func TestComposeRefusesWhatItCannotCopy(t *testing.T) {
	t.Parallel()

	conflict := repoDir(t, "plugins: [a]\nshared: [lib]\n", map[string]string{"a/lib": "mine", "lib/x.lua": "x"})
	repo, err := authorrepo.Load(conflict)
	require.NoError(t, err)

	_, err = repo.Compose(t.TempDir(), "a", "")
	require.ErrorIs(t, err, authorrepo.ErrCompose)
	require.ErrorContains(t, err, "cannot copy a shared directory into the plugin: a: the plugin already has lib, "+
		"where lib is copied")

	noModule := repoDir(t, "plugins: [a]\nshared: [lib]\n", map[string]string{"lib/x.go": "package lib\n"})
	repo, err = authorrepo.Load(noModule)
	require.NoError(t, err)

	_, err = repo.Compose(t.TempDir(), "a", ".")
	require.ErrorIs(t, err, authorrepo.ErrCompose)
	require.ErrorContains(t, err, "lib holds Go files, which Go plugins import by their import path: ")

	placed, err := repo.Compose(t.TempDir(), "a", "")
	require.NoError(t, err, "Lua and TypeScript plugins need no module")
	assert.Len(t, placed, 1)

	used := t.TempDir()
	write(t, used, map[string]string{"lib/x.go": "mine"})

	_, err = repo.Compose(used, "a", "")
	require.ErrorIs(t, err, authorrepo.ErrCompose, "the destination must be empty")
	require.ErrorIs(t, err, os.ErrExist)

	notDir := filepath.Join(t.TempDir(), "file")
	write(t, filepath.Dir(notDir), map[string]string{"file": "x"})

	_, err = repo.Compose(notDir, "a", "")
	require.ErrorIs(t, err, authorrepo.ErrCompose, "the destination must be a directory")

	large := repoDir(t, "plugins: [a]\nshared: [lib]\n", nil)
	sparse, err := os.Create(filepath.Join(large, "lib", "big.bin")) //nolint:gosec // G304: the test's directory
	require.NoError(t, err)
	require.NoError(t, sparse.Truncate(plg.MaxPluginFileSize+1))
	require.NoError(t, sparse.Close())

	repo, err = authorrepo.Load(large)
	require.NoError(t, err)

	dst := t.TempDir()
	_, err = repo.Compose(dst, "a", "")
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
	require.ErrorContains(t, err, "big.bin")

	entries, err := os.ReadDir(dst)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is copied before the sizes are checked")
}
