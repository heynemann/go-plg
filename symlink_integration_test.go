//go:build integration

package plg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// hostSecret is the content of host files outside every plugin directory: no plugin may read it.
const hostSecret = "host secret 7f3a" //nolint:gosec // G101: a marker for leaked contents, not a credential

// secretFile writes data to the host file name in a directory of its own, outside every plugin directory, and
// returns its path.
func secretFile(t *testing.T, name, data string) string {
	t.Helper()

	file := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(file, []byte(data), 0o600))

	return file
}

// pluginRoot returns a directory of plugins holding the plugin directory "p": files (slash-separated name ->
// contents, a "../" name is next to "p") and symbolic links (name -> target).
func pluginRoot(t *testing.T, files, links map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for name, data := range files {
		file := filepath.Join(root, "p", filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o750))
		require.NoError(t, os.WriteFile(file, []byte(data), 0o600))
	}

	for name, target := range links {
		link := filepath.Join(root, "p", filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o750))
		require.NoError(t, os.Symlink(target, link))
	}

	return root
}

// tsFooerSource is a TypeScript Fooer plugin whose module starts with header and whose Foo returns foo.
func tsFooerSource(header, foo string) string {
	return header + "\nexport function Create() { return {\n" + fooerMembers("Foo: () => "+foo) + "\n}; }\n"
}

func TestSymlinksOutOfAPluginFailItsLoad(t *testing.T) {
	t.Parallel()

	secret := secretFile(t, "secret.txt", hostSecret)
	secretLua := secretFile(t, "lib.lua", `return "`+hostSecret+`"`)

	const luaPlugin = `function Create()
  return {
    Foo = function() return require("lib") end,
    Count = function() return 0 end,
    Fail = function() end,
    Boil = function() return { false, 0 } end,
    Scalars = function(_, ...) return { ... } end,
    Spin = function(_, n) return n end,
  }
end
`

	tsManifest := "language: typescript\nentrypoint: plugin.ts\nversion: \"1\"\n"
	luaManifest := "language: lua\nentrypoint: main.lua\nversion: \"1\"\n"
	tsImport := tsFooerSource(`import secret from "./x.txt";`, "secret")

	tests := []struct {
		files, links map[string]string
		name         string
		path         string // the offending path the error names
	}{
		{
			name:  "TypeScript import of a host file",
			files: map[string]string{"plugin.yaml": tsManifest, "plugin.ts": tsImport},
			links: map[string]string{"x.txt": secret},
			path:  "x.txt",
		},
		{
			// The storage's top-level files are not plugins: secret.txt is next to the plugin, outside it.
			name: "TypeScript import of a file next to the plugin by a relative link",
			files: map[string]string{
				"plugin.yaml":   "language: typescript\nentrypoint: lib/plugin.ts\nversion: \"1\"\n",
				"lib/plugin.ts": tsImport, "../secret.txt": hostSecret,
			},
			links: map[string]string{"lib/x.txt": "../../secret.txt"},
			path:  "lib/x.txt",
		},
		{
			name:  "TypeScript import of the host's environment",
			files: map[string]string{"plugin.yaml": tsManifest, "plugin.ts": tsImport},
			links: map[string]string{"x.txt": "/proc/self/environ"},
			path:  "x.txt",
		},
		{
			name:  "Go entrypoint linked to the root directory",
			files: map[string]string{"plugin.yaml": "language: go\nentrypoint: src\nversion: \"1\"\n"},
			links: map[string]string{"src": "/"},
			path:  "src",
		},
		{
			name:  "Lua require of a host module",
			files: map[string]string{"plugin.yaml": luaManifest, "main.lua": luaPlugin},
			links: map[string]string{"lib.lua": secretLua},
			path:  "lib.lua",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			checkSymlinkEscape(t, pluginRoot(t, tc.files, tc.links), tc.path)
		})
	}
}

// checkSymlinkEscape checks that registering the plugin directory "p" of root fails its load with an error
// naming the symbolic link at path, and that no host file reached the plugin.
func checkSymlinkEscape(t *testing.T, root, path string) {
	t.Helper()

	ctr := newContainer(t)

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer", plg.WithStorage(plg.NewFSStorage(root)))
	if err == nil {
		// Not printing what Foo returned: it may be the host's environment.
		got, callErr := getFooer(t, ctr, "fooer/p").Foo(t.Context(), "")
		t.Fatalf("the plugin loaded through the link; Foo returned %d bytes (the host secret: %t), %v", len(got),
			strings.Contains(got, hostSecret), callErr)
	}

	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, path+": symbolic link out of the plugin's files")
	assert.NotContains(t, err.Error(), hostSecret)

	_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, "fooer/p")
	require.ErrorIs(t, err, plg.ErrNotFound)
}

// Register copies every storage's files by the same rules as the built-in storages.
func TestSymlinksOutOfACustomStorageFailTheLoad(t *testing.T) {
	t.Parallel()

	store := listStorage{
		name: "custom",
		files: fstest.MapFS{
			"plugin.ts": {Data: []byte(tsFooerSource(`import secret from "./x.txt";`, "secret"))},
			"x.txt":     link("/proc/self/environ"),
		},
		descs: []plg.Descriptor{{Key: "p", Language: plg.LanguageTypeScript, Entrypoint: "plugin.ts"}},
	}

	err := plg.Register[testcontract.Fooer](t.Context(), newContainer(t), "custom", plg.WithStorage(store))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, "opening: copying the plugin's files: x.txt: symbolic link out of the plugin's "+
		"files (to /proc/self/environ)")
}

// Symbolic links that stay inside a plugin's directory are skipped, as if they were not there: plugins cannot
// import or require them, and they are not part of the content hash.
func TestSymlinksInsideAPluginAreSkipped(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"plugin.yaml": "language: typescript\nentrypoint: plugin.ts\n",
		"plugin.ts":   tsFooerSource(`import data from "./data.txt";`, "data"),
		"data.txt":    "plugin data",
	}
	links := map[string]string{"alias.txt": "data.txt", "sub/up.txt": "../data.txt", "loop": "."}

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "linked",
		plg.WithStorage(plg.NewFSStorage(pluginRoot(t, files, links)))))

	got, err := getFooer(t, ctr, "linked/p").Foo(t.Context(), "")
	require.NoError(t, err)
	assert.Equal(t, "plugin data", got)

	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "plain",
		plg.WithStorage(plg.NewFSStorage(pluginRoot(t, files, nil)))))
	assert.Equal(t, inspect(t, ctr, "plain/p").Version, inspect(t, ctr, "linked/p").Version, "the links are not hashed")

	files["plugin.ts"] = tsFooerSource(`import data from "./alias.txt";`, "data")
	err = plg.Register[testcontract.Fooer](t.Context(), ctr, "alias",
		plg.WithStorage(plg.NewFSStorage(pluginRoot(t, files, links))))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, `"./alias.txt": no such file in the plugin`)
}

// changingFS serves plugin.ts with first's contents the first time it is opened and second's afterwards.
type changingFS struct {
	opens         *atomic.Int32
	first, second string
}

func (f changingFS) Open(name string) (fs.File, error) {
	data := f.second
	if name == "plugin.ts" && f.opens.Add(1) == 1 {
		data = f.first
	}

	return fstest.MapFS{"plugin.ts": {Data: []byte(data)}}.Open(name) //nolint:wrapcheck // the test's own fs
}

// A plugin's files are read once: its content hash is of the files it runs, even when the storage's files
// change while it loads.
func TestPluginsHashTheFilesTheyLoad(t *testing.T) {
	t.Parallel()

	first, second := tsFooerSource("", `"first"`), tsFooerSource("", `"second"`)
	ctr := newContainer(t)

	store := listStorage{
		name:  "changing",
		files: changingFS{opens: &atomic.Int32{}, first: first, second: second},
		descs: []plg.Descriptor{{Key: "p", Language: plg.LanguageTypeScript, Entrypoint: "plugin.ts"}},
	}
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "changing", plg.WithStorage(store)))

	got, err := getFooer(t, ctr, "changing/p").Foo(t.Context(), "")
	require.NoError(t, err)

	want := map[string]string{"first": first, "second": second}[got]
	require.NotEmpty(t, want, "Foo returned %q", got)

	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "fixed",
		plg.WithStorage(tsSource(t, fstest.MapFS{"plugin.ts": {Data: []byte(want)}}))))
	assert.Equal(t, inspect(t, ctr, "fixed/p").Version, inspect(t, ctr, "changing/p").Version,
		"the version is the hash of the source the plugin runs")
}
