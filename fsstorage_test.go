package plg_test

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// storagesFS holds the plugin directories of the FS storage tests, embedded.
//
//go:embed testdata/storages
var storagesFS embed.FS

// diskDir is a plugin directory tree read from disk: a one-file Go plugin, a multi-file Go plugin without a
// version, a Lua plugin and a README that is not a plugin.
const diskDir = "testdata/storages/disk"

// embeddedStorage returns a storage over the embedded copy of testdata/storages/<dir>.
func embeddedStorage(t *testing.T, name, dir string) *plg.FSStorage {
	t.Helper()

	sub, err := fs.Sub(storagesFS, "testdata/storages/"+dir)
	require.NoError(t, err)

	return plg.NewFSStorageFromFS(name, sub)
}

// manifestStorage returns a storage holding one plugin directory, p, with the given plugin.yaml and plugin.go.
func manifestStorage(manifest string) *plg.FSStorage {
	return plg.NewFSStorageFromFS("mapfs", fstest.MapFS{
		"p/plugin.yaml": {Data: []byte(manifest)},
		"p/plugin.go":   {Data: []byte("package p\n")},
	})
}

const isContentHash = `^sha256:[0-9a-f]{64}$`

// ruleImplements is the interface the plugins of the manifests these tests parse implement, and implementsLine
// declares it in a plugin.yaml.
const (
	ruleImplements = "example.com/contracts/rules.Rule@v1.4.0"
	implementsLine = "implements: " + ruleImplements + "\n"
)

func TestFSStorageListsPluginDirectories(t *testing.T) {
	t.Parallel()

	store := plg.NewFSStorage(diskDir)
	assert.Equal(t, diskDir, store.Name())

	descs, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, descs, 3, "README.md is not a plugin directory")

	// multifile has no version: Register hashes the files it loads (TestRegisterHashesVersionlessPlugins).
	want := []plg.Descriptor{
		{
			Key: "multifile", Implements: implementsOf[testcontract.Fooer](), Language: plg.LanguageGo,
			Entrypoint: ".", MemoryLimit: 64 << 20,
		},
		{
			Key: "scripted", Implements: implementsOf[testcontract.Fooer](), Language: plg.LanguageLua,
			Entrypoint: "main.lua", Version: "0.1.0",
		},
		{
			Key: "single", Implements: implementsOf[testcontract.Fooer](), Language: plg.LanguageGo,
			Entrypoint: "plugin.go", Version: "1.2.3",
		},
	}
	assert.Equal(t, want, descs, "sorted by key")

	embedded, err := embeddedStorage(t, "embed", "disk").List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, want, embedded, "an embed.FS of the same tree lists the same plugins")

	files, err := store.Open(t.Context(), &descs[2])
	require.NoError(t, err)

	src, err := fs.ReadFile(files, "plugin.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), "package single", "Open serves the plugin directory")
}

func TestFSStorageIgnoresEntriesThatAreNotPlugins(t *testing.T) {
	t.Parallel()

	store := plg.NewFSStorageFromFS("mapfs", fstest.MapFS{
		"README":               {Data: []byte("not a plugin")},
		".git/config":          {Data: []byte("[core]")},
		".hidden/plugin.yaml":  {Data: []byte("{malformed")},
		"p/plugin.yaml":        {Data: []byte(implementsLine + "language: go\nentrypoint: .\nversion: \"1\"\n")},
		"p/.notes/plugin.yaml": {Data: []byte("{malformed")},
	})

	descs, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plg.Descriptor{
		{Key: "p", Implements: ruleImplements, Language: plg.LanguageGo, Entrypoint: ".", Version: "1"},
	}, descs)
}

func TestFSStorageParsesManifests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		manifest string
		want     plg.Descriptor
	}{
		{
			manifest: implementsLine + "language: go\nentrypoint: plugin.go\nversion: v1\n",
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: "plugin.go", Version: "v1"},
		},
		{
			manifest: "# a comment\n---\nversion: '2'   # quoted\nentrypoint: \"main.lua\"\nlanguage: lua\n" +
				"implements: '" + ruleImplements + "'\n...\n",
			want: plg.Descriptor{Language: plg.LanguageLua, Entrypoint: "main.lua", Version: "2"},
		},
		{
			manifest: `{"language": "typescript", "entrypoint": "src/index.ts", "version": "3", "memoryLimit": null, ` +
				`"implements": "` + ruleImplements + `"}`,
			want: plg.Descriptor{Language: plg.LanguageTypeScript, Entrypoint: "src/index.ts", Version: "3"},
		},
		{
			manifest: implementsLine + "language: go\nentrypoint: .\nversion: 4\nmemoryLimit: 65536\n",
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: ".", Version: "4", MemoryLimit: 64 << 10},
		},
		{
			manifest: implementsLine + "language: go\nentrypoint: .\nversion: 5\nmemoryLimit: 4294967296\n",
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: ".", Version: "5", MemoryLimit: 4 << 30},
		},
		{
			manifest: implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 0x4000000\n",
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: ".", MemoryLimit: 64 << 20},
		},
		{
			manifest: implementsLine + "language: go\nentrypoint: .\nversion: \"\\u00e9t\\u00e9\"\n",
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: ".", Version: "été"},
		},
		{
			// a string takes the scalar verbatim: no float rounding, no date parsing
			manifest: implementsLine + "language: go\nentrypoint: .\nversion: 1.10\n",
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: ".", Version: "1.10"},
		},
		{
			manifest: implementsLine + "language: go\nentrypoint: .\nsecrets: [stripe_key, db.password, A-1]\n",
			want: plg.Descriptor{
				Language: plg.LanguageGo, Entrypoint: ".", Secrets: []string{"stripe_key", "db.password", "A-1"},
			},
		},
		{
			manifest: implementsLine + "language: go\nentrypoint: .\nversion: 2026-11-06\n",
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: ".", Version: "2026-11-06"},
		},
		{
			manifest: "implements: !!str " + ruleImplements + "\nlanguage: !!str go\nentrypoint: .\n",
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: "."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.manifest, func(t *testing.T) {
			t.Parallel()

			tc.want.Implements = ruleImplements

			parsed, err := plg.ParseManifest([]byte(tc.manifest))
			require.NoError(t, err)
			assert.Equal(t, tc.want, parsed, "ParseManifest reads the manifest as FSStorage does, without a key")

			descs, err := manifestStorage(tc.manifest).List(t.Context())
			require.NoError(t, err)

			tc.want.Key = "p"
			assert.Equal(t, []plg.Descriptor{tc.want}, descs)
		})
	}
}

func TestParseManifestLeavesAMissingVersionToRegister(t *testing.T) {
	t.Parallel()

	manifest := implementsLine + "language: go\nentrypoint: .\n"

	parsed, err := plg.ParseManifest([]byte(manifest))
	require.NoError(t, err)
	assert.Equal(t, plg.Descriptor{Implements: ruleImplements, Language: plg.LanguageGo, Entrypoint: "."}, parsed)

	descs, err := manifestStorage(manifest).List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plg.Descriptor{
		{Key: "p", Implements: ruleImplements, Language: plg.LanguageGo, Entrypoint: "."},
	}, descs, "FSStorage leaves it to Register too, which hashes the files it loads")
}

// TestParseManifestReadsImplements parses the interface a plugin declares it implements, through ParseManifest and
// FSStorage alike: valid forms are kept verbatim, and a missing or malformed one fails, saying why.
func TestParseManifestReadsImplements(t *testing.T) {
	t.Parallel()

	for _, implements := range []string{
		ruleImplements,
		"example.com/contracts/v2/rewards.Cohort@v2.0.1",                      // a major version suffix
		"example.com/contracts.Rule@v0.3.0",                                   // the module's root package, a v0 module
		"example.com/contracts/rules.rule@v1.0.0",                             // an unexported interface
		"example.com/contracts/rules.Rule@v1.5.0-rc.1",                        // a prerelease
		"example.com/contracts/rules.Rule@v0.0.0-20261108120000-0123456789ab", // a pseudo-version
		"contracts.Rule@v1.0.0",                                               // a path without a dot in its first element
	} {
		t.Run(implements, func(t *testing.T) {
			t.Parallel()

			manifest := "implements: " + implements + "\nlanguage: go\nentrypoint: .\n"
			want := plg.Descriptor{Implements: implements, Language: plg.LanguageGo, Entrypoint: "."}

			parsed, err := plg.ParseManifest([]byte(manifest))
			require.NoError(t, err)
			assert.Equal(t, want, parsed)

			descs, err := manifestStorage(manifest).List(t.Context())
			require.NoError(t, err)

			want.Key = "p"
			assert.Equal(t, []plg.Descriptor{want}, descs)
		})
	}

	for name, tc := range map[string]struct{ implements, want string }{
		"missing":           {want: "implements is required: the interface the plugin implements and the version"},
		"null":              {implements: "~", want: "implements is required"},
		"empty":             {implements: "''", want: "implements is required"},
		"no version":        {implements: "example.com/c/rules.Rule", want: `it must end with "@" and the contract`},
		"empty version":     {implements: "example.com/c/rules.Rule@", want: `the version must be a canonical semantic`},
		"no v":              {implements: "example.com/c/rules.Rule@1.4.0", want: `canonical semantic version, as in v1.4.0, not "1.4.0"`},
		"major and minor":   {implements: "example.com/c/rules.Rule@v1.4", want: `not "v1.4"`},
		"major only":        {implements: "example.com/c/rules.Rule@v1", want: `not "v1"`},
		"build metadata":    {implements: "example.com/c/rules.Rule@v1.4.0+meta", want: `not "v1.4.0+meta"`},
		"a query":           {implements: "example.com/c/rules.Rule@latest", want: `not "latest"`},
		"two versions":      {implements: "example.com/c/rules.Rule@v1.0.0@v2.0.0", want: `not "v1.0.0@v2.0.0"`},
		"no interface":      {implements: "example.com/c/rules@v1.0.0", want: "it must name the interface after its package"},
		"no package":        {implements: "Rule@v1.0.0", want: "it must name the interface after its package"},
		"empty interface":   {implements: "example.com/c/rules.@v1.0.0", want: `the interface name must be a Go identifier, not ""`},
		"not an identifier": {implements: "example.com/c/rules.1Rule@v1.0.0", want: `Go identifier, not "1Rule"`},
		"leading slash":     {implements: "/c/rules.Rule@v1.0.0", want: `malformed import path "/c/rules"`},
		"a space":           {implements: "'example.com/my c/rules.Rule@v1.0.0'", want: `malformed import path`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			manifest := "language: go\nentrypoint: .\n"
			if tc.implements != "" {
				manifest += "implements: " + tc.implements + "\n"
			}

			_, err := plg.ParseManifest([]byte(manifest))
			require.ErrorIs(t, err, plg.ErrInvalidManifest)
			require.ErrorContains(t, err, tc.want)

			_, err = manifestStorage(manifest).List(t.Context())
			require.ErrorIs(t, err, plg.ErrInvalidManifest)
			require.ErrorContains(t, err, "plugin directory p")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestFSStorageRejectsMalformedManifests(t *testing.T) {
	t.Parallel()

	// Every manifest failing on a value has a valid implements, so it fails on that value.
	tests := map[string]string{
		"empty":                ``,
		"only a comment":       "# language: go\n",
		"not YAML":             "language: [go\n",
		"tab indentation":      "language: go\n\tentrypoint: .\n",
		"a sequence":           "- language: go\n  entrypoint: .\n",
		"a scalar":             "go",
		"null":                 `null`,
		"two documents":        implementsLine + "language: go\nentrypoint: .\n---\nlanguage: go\nentrypoint: .\n",
		"an empty second doc":  implementsLine + "language: go\nentrypoint: .\n---\n",
		"anchor":               implementsLine + "language: &l go\nentrypoint: .\n",
		"alias":                implementsLine + "x: &l go\nlanguage: *l\nentrypoint: .\n",
		"merge key":            implementsLine + "<<: {language: go, entrypoint: .}\n",
		"merge of an alias":    implementsLine + "x: &m {language: go}\n<<: *m\nentrypoint: .\n",
		"custom tag":           implementsLine + "language: !lang go\nentrypoint: .\n",
		"custom mapping tag":   "--- !plugin\n" + implementsLine + "language: go\nentrypoint: .\n",
		"binary tag":           implementsLine + "language: !!binary Z28=\nentrypoint: .\n",
		"duplicate key":        implementsLine + "language: go\nentrypoint: .\nlanguage: lua\n",
		"duplicate flow key":   "{implements: " + ruleImplements + ", language: go, entrypoint: ., entrypoint: x}",
		"non-scalar key":       implementsLine + "? [language]\n: go\nentrypoint: .\n",
		"unknown key":          implementsLine + "language: go\nentrypoint: .\nmemory_limit: 65536\n",
		"key case":             implementsLine + "Language: go\nentrypoint: .\n",
		"language type":        implementsLine + "language: [go]\nentrypoint: .\n",
		"no language":          implementsLine + "entrypoint: .\n",
		"null language":        implementsLine + "language: ~\nentrypoint: .\n",
		"unknown language":     implementsLine + "language: python\nentrypoint: main.py\n",
		"language case":        implementsLine + "language: Go\nentrypoint: .\n",
		"no entrypoint":        implementsLine + "language: go\n",
		"empty entrypoint":     implementsLine + "language: go\nentrypoint: ''\n",
		"escaping entrypoint":  implementsLine + "language: go\nentrypoint: ../other\n",
		"absolute entrypoint":  implementsLine + "language: go\nentrypoint: /plugin.go\n",
		"trailing slash":       implementsLine + "language: go\nentrypoint: pkg/\n",
		"version type":         implementsLine + "language: go\nentrypoint: .\nversion: {major: 1}\n",
		"control character":    implementsLine + "language: go\nentrypoint: .\nversion: \"a\x01b\"\n",
		"invalid UTF-8":        implementsLine + "language: go\nentrypoint: \"\xff\"\n",
		"zero memory limit":    implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 0\n",
		"limit of 1 byte":      implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 1\n",
		"limit under a page":   implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 65535\n",
		"negative memoryLimit": implementsLine + "language: go\nentrypoint: .\nmemoryLimit: -1\n",
		"fractional limit":     implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 65536.5\n",
		"float limit":          implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 1e6\n",
		"limit as a string":    implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 64MiB\n",
		"quoted limit":         implementsLine + "language: go\nentrypoint: .\nmemoryLimit: \"65536\"\n",
		"boolean limit":        implementsLine + "language: go\nentrypoint: .\nmemoryLimit: true\n",
		"limit over 4 GiB":     implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 4294967297\n",
		"limit over uint64":    implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 18446744073709551616\n",
		"limit of 2^64+65536":  implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 18446744073709617152\n",
		"oversize": implementsLine + "language: go\nentrypoint: .\n" + strings.Repeat(
			"#",
			plg.MaxManifestSize,
		),
		"no implements":         "language: go\nentrypoint: .\n",
		"implements type":       "implements: [" + ruleImplements + "]\nlanguage: go\nentrypoint: .\n",
		"implements mapping":    "implements: {rule: " + ruleImplements + "}\nlanguage: go\nentrypoint: .\n",
		"implements case":       "Implements: " + ruleImplements + "\nlanguage: go\nentrypoint: .\n",
		"secrets type":          implementsLine + "language: go\nentrypoint: .\nsecrets: stripe_key\n",
		"secret name type":      implementsLine + "language: go\nentrypoint: .\nsecrets: [{a: b}]\n",
		"empty secret name":     implementsLine + "language: go\nentrypoint: .\nsecrets: ['']\n",
		"secret starting digit": implementsLine + "language: go\nentrypoint: .\nsecrets: [1key]\n",
		"secret with a space":   implementsLine + "language: go\nentrypoint: .\nsecrets: ['a key']\n",
		"secret name too long": implementsLine + "language: go\nentrypoint: .\nsecrets: [" + strings.Repeat(
			"k",
			plg.MaxSecretName+1,
		) + "]\n",
		"secret declared twice": implementsLine + "language: go\nentrypoint: .\nsecrets: [a, b, a]\n",
		"too many secrets": implementsLine + "language: go\nentrypoint: .\nsecrets: [" + secretList(
			plg.MaxSecrets+1,
		) + "]\n",
	}

	for name, manifest := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := manifestStorage(manifest).List(t.Context())
			require.ErrorIs(t, err, plg.ErrInvalidManifest)
			require.ErrorContains(t, err, "plugin directory p")

			_, err = plg.ParseManifest([]byte(manifest))
			require.ErrorIs(t, err, plg.ErrInvalidManifest, "ParseManifest rejects what FSStorage rejects")
		})
	}
}

func TestFSStorageRequiresAManifestPerPluginDirectory(t *testing.T) {
	t.Parallel()

	tests := map[string]fstest.MapFS{
		"missing": {
			"a/plugin.yaml": {Data: []byte(implementsLine + "language: go\nentrypoint: .\nversion: 1\n")},
			"b/plugin.go":   {Data: []byte("package b\n")},
		},
		"a directory": {"b/plugin.yaml/x": {Data: []byte("{}")}},
	}

	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := plg.NewFSStorageFromFS("mapfs", files).List(t.Context())
			require.ErrorIs(t, err, plg.ErrInvalidManifest)
			require.ErrorContains(t, err, "plugin directory b")
		})
	}

	_, err := plg.NewFSStorageFromFS("mapfs", tests["missing"]).List(t.Context())
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestFSStorageNeverReadsPluginJSON checks the cutover to plugin.yaml: a directory holding only a (valid)
// plugin.json is not a plugin, and the error names plugin.yaml so the author knows what to write; beside a
// plugin.yaml, a plugin.json is just another file.
func TestFSStorageNeverReadsPluginJSON(t *testing.T) {
	t.Parallel()

	legacy := []byte(`{"language": "go", "entrypoint": "."}`)

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "p"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p", "plugin.json"), legacy, 0o600))

	for name, store := range map[string]*plg.FSStorage{
		"directory": plg.NewFSStorage(dir),
		"fs.FS":     plg.NewFSStorageFromFS("mapfs", fstest.MapFS{"p/plugin.json": {Data: legacy}}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			descs, err := store.List(t.Context())
			require.ErrorIs(t, err, plg.ErrInvalidManifest)
			require.NotErrorIs(t, err, fs.ErrNotExist, "the hint replaces the missing-file error")
			require.ErrorContains(t, err, "plugin directory p: no plugin.yaml, only a plugin.json")
			assert.Empty(t, descs)
		})
	}

	descs, err := plg.NewFSStorageFromFS("mapfs", fstest.MapFS{
		"p/plugin.json": {Data: []byte("{malformed")},
		"p/plugin.yaml": {Data: []byte(implementsLine + "language: lua\nentrypoint: main.lua\n")},
	}).List(t.Context())
	require.NoError(t, err, "a plugin.json beside a plugin.yaml is not read")
	assert.Equal(t, []plg.Descriptor{
		{Key: "p", Implements: ruleImplements, Language: plg.LanguageLua, Entrypoint: "main.lua"},
	}, descs)
}

// TestFSStorageBoundsManifestReads lists plugin directories whose plugin.yaml is a FIFO (opening it would wait
// for a writer forever), a link to /dev/zero (an endless read) or a regular file over MaxManifestSize: each fails
// at once with ErrInvalidManifest naming the directory, through an os.Root and through an os.DirFS alike.
func TestFSStorageBoundsManifestReads(t *testing.T) {
	t.Parallel()

	valid := implementsLine + "language: go\nentrypoint: .\n"
	oversized := valid + strings.Repeat(" ", plg.MaxManifestSize+1-len(valid))

	tests := map[string]struct {
		make func(t *testing.T, name string)
		want string
	}{
		"fifo": {
			make: func(t *testing.T, name string) { t.Helper(); require.NoError(t, syscall.Mkfifo(name, 0o600)) },
			want: "not a regular file",
		},
		"link to /dev/zero": {
			make: func(t *testing.T, name string) { t.Helper(); require.NoError(t, os.Symlink("/dev/zero", name)) },
		},
		"oversized": {
			make: func(t *testing.T, name string) {
				t.Helper()
				require.NoError(t, os.WriteFile(name, []byte(oversized), 0o600))
			},
			want: "over 65536 bytes",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(dir, "p"), 0o700))
			tc.make(t, filepath.Join(dir, "p", "plugin.yaml"))

			for _, store := range []*plg.FSStorage{plg.NewFSStorage(dir), plg.NewFSStorageFromFS("dirfs", os.DirFS(dir))} {
				_, err := store.List(t.Context())
				require.ErrorIs(t, err, plg.ErrInvalidManifest, store.Name())
				require.ErrorContains(t, err, "plugin directory p", store.Name())
				require.ErrorContains(t, err, tc.want, store.Name())
			}
		})
	}

	_, err := plg.ParseManifest([]byte(oversized))
	require.ErrorIs(t, err, plg.ErrInvalidManifest)
	require.ErrorContains(t, err, "over 65536 bytes", "ParseManifest shares the cap")

	_, err = plg.ParseManifest([]byte(oversized[:plg.MaxManifestSize]))
	require.NoError(t, err, "a manifest of exactly MaxManifestSize bytes is accepted")
}

// TestFSStorageListNeverWaitsOnASwappedManifest replaces a plugin.yaml List found to be a regular file by a FIFO
// right before List opens it: a read-only open of a FIFO waits for a writer, so it would block List forever. List
// fails at once with ErrInvalidManifest naming the directory.
func TestFSStorageListNeverWaitsOnASwappedManifest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	manifest := filepath.Join(dir, "p", "plugin.yaml")
	require.NoError(t, os.Mkdir(filepath.Dir(manifest), 0o700))
	require.NoError(t, os.WriteFile(manifest, []byte(implementsLine+"language: go\nentrypoint: .\n"), 0o600))

	swapped := false

	_, err := plg.ListSwapping(t.Context(), plg.NewFSStorage(dir), func(name string) {
		if name == "p/plugin.yaml" {
			swapped = true

			swapFIFO(t, manifest)
		}
	})
	require.True(t, swapped, "List opened plugin.yaml")
	require.ErrorIs(t, err, plg.ErrInvalidManifest)
	require.ErrorContains(t, err, "plugin directory p: plugin.yaml is not a regular file")
}

func TestFSStorageListAndOpenErrors(t *testing.T) {
	t.Parallel()

	missing := plg.NewFSStorage(filepath.Join(t.TempDir(), "missing"))

	_, err := missing.List(t.Context())
	require.ErrorIs(t, err, fs.ErrNotExist)

	_, err = missing.Open(t.Context(), &plg.Descriptor{Key: "p"})
	require.ErrorIs(t, err, fs.ErrNotExist)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = manifestStorage(implementsLine + "language: go\nentrypoint: .\n").List(ctx)
	require.ErrorIs(t, err, context.Canceled)

	// Opening a plugin copies every file and directory below it; listing reads only the manifests.
	files := fstest.MapFS{
		"p/plugin.yaml": {Data: []byte(implementsLine + "language: go\nentrypoint: .\n")},
		"p/plugin.go":   {Data: []byte("package p\n")},
		"p/sub/x.go":    {Data: []byte("package sub\n")},
	}

	for name, broken := range map[string]fs.FS{
		"unopenable file":     unreadableFS{files: files, broken: "p/plugin.go"},
		"unlistable dir":      unreadableFS{files: files, broken: "p/sub"},
		"unreadable contents": failingReadFS{files: files, broken: "p/sub/x.go"},
		"unclosable file":     failingReadFS{files: files, broken: "p/sub/x.go", close: true},
		"unstatable entry":    unstatableFS{files: files, broken: "p/sub/x.go"},
		"unstatable dir":      brokenDirFS{files: files, broken: "p/sub", fail: "stat"},
		"unreadable dir":      brokenDirFS{files: files, broken: "p/sub", fail: "read"},
		"unclosable dir":      brokenDirFS{files: files, broken: "p/sub", fail: "close"},
		"unreadable whole":    brokenDirFS{files: files, broken: "p/sub", fail: "whole"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := plg.NewFSStorageFromFS("broken", broken)

			descs, err := store.List(t.Context())
			require.NoError(t, err)
			require.Len(t, descs, 1)

			_, err = store.Open(t.Context(), &descs[0])
			require.ErrorIs(t, err, errStorage)
			require.ErrorContains(t, err, "plugin directory p")
		})
	}
}

// failingReadFS is files, except that reading the contents of the file named broken fails, or closing it with
// close.
type failingReadFS struct {
	files  fstest.MapFS
	broken string
	close  bool
}

func (f failingReadFS) Open(name string) (fs.File, error) {
	file, err := f.files.Open(name)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	switch {
	case name != f.broken:
		return file, nil
	case f.close:
		return failingClose{file}, nil
	default:
		return failingRead{file}, nil
	}
}

type failingRead struct {
	fs.File
}

func (failingRead) Read([]byte) (int, error) { return 0, errStorage }

type failingClose struct {
	fs.File
}

func (failingClose) Close() error { return errStorage }

// unstatableFS is files, except that the directory entry of the file named broken cannot give its information,
// as when the file is removed while its directory is read.
type unstatableFS struct {
	files  fstest.MapFS
	broken string
}

func (f unstatableFS) Open(name string) (fs.File, error) {
	file, err := f.files.Open(name)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	if dir, ok := file.(fs.ReadDirFile); ok {
		return unstatableDir{ReadDirFile: dir, name: name, broken: f.broken}, nil
	}

	return file, nil
}

// unstatableDir is the directory name of an unstatableFS.
type unstatableDir struct {
	fs.ReadDirFile

	name   string
	broken string
}

func (d unstatableDir) ReadDir(n int) ([]fs.DirEntry, error) {
	entries, err := d.ReadDirFile.ReadDir(n)

	for i, entry := range entries {
		if path.Join(d.name, entry.Name()) == d.broken {
			entries[i] = unstatableEntry{entry}
		}
	}

	return entries, err //nolint:wrapcheck // the directory's own error, io.EOF at its end
}

type unstatableEntry struct {
	fs.DirEntry
}

func (unstatableEntry) Info() (fs.FileInfo, error) { return nil, errStorage }

// brokenDirFS is files, except that the directory named broken fails: its Stat (stat), a read of its entries
// (read) or its Close (close), or, with whole, a read of it as an fs.ReadDirFS, every directory it opens being no
// fs.ReadDirFile.
type brokenDirFS struct {
	files  fstest.MapFS
	broken string
	fail   string
}

func (f brokenDirFS) Open(name string) (fs.File, error) {
	file, err := f.files.Open(name)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	dir, ok := file.(fs.ReadDirFile)

	switch {
	case !ok:
		return file, nil
	case f.fail == "whole":
		return struct{ fs.File }{file}, nil
	case name == f.broken:
		return brokenDir{ReadDirFile: dir, fail: f.fail}, nil
	default:
		return dir, nil
	}
}

func (f brokenDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.broken {
		return nil, errStorage
	}

	return f.files.ReadDir(name) //nolint:wrapcheck // the tree's own error, as an fs.FS returns it
}

// brokenDir is the broken directory of a brokenDirFS.
type brokenDir struct {
	fs.ReadDirFile

	fail string
}

func (d brokenDir) Stat() (fs.FileInfo, error) {
	if d.fail == "stat" {
		return nil, errStorage
	}

	return d.ReadDirFile.Stat() //nolint:wrapcheck // the directory's own error, as an fs.File returns it
}

func (d brokenDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.fail == "read" {
		return nil, errStorage
	}

	return d.ReadDirFile.ReadDir(n) //nolint:wrapcheck // the directory's own error, io.EOF at its end
}

func (d brokenDir) Close() error {
	if d.fail == "close" {
		return errStorage
	}

	return d.ReadDirFile.Close() //nolint:wrapcheck // the directory's own error, as an fs.File returns it
}

func TestFSStorageOpen(t *testing.T) {
	t.Parallel()

	store := plg.NewFSStorage(diskDir)

	tests := []struct {
		want error
		key  string
	}{
		{key: ".", want: plg.ErrInvalidDescriptor},
		{key: "", want: plg.ErrInvalidDescriptor},
		{key: "..", want: plg.ErrInvalidDescriptor},
		{key: "single/plugin.go", want: plg.ErrInvalidDescriptor},
		{key: "missing", want: fs.ErrNotExist},
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()

			_, err := store.Open(t.Context(), &plg.Descriptor{Key: tc.key})
			require.ErrorIs(t, err, tc.want)
		})
	}

	t.Run("a file", func(t *testing.T) {
		t.Parallel()

		_, err := store.Open(t.Context(), &plg.Descriptor{Key: "README.md"})
		require.ErrorContains(t, err, "plugin directory README.md: a plugin key is a directory name")
	})

	t.Run("a FIFO", func(t *testing.T) {
		t.Parallel()

		// What List listed as a plugin directory may be a FIFO by the time Open opens it, which a read-only open
		// would wait on for a writer forever.
		dir := t.TempDir()
		require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "p"), 0o600))

		_, err := plg.NewFSStorage(dir).Open(t.Context(), &plg.Descriptor{Key: "p"})
		require.ErrorContains(t, err, "plugin directory p: a plugin key is a directory name")
	})

	t.Run("over an fs.FS", func(t *testing.T) {
		t.Parallel()

		tree := plg.NewFSStorageFromFS("tree", fstest.MapFS{"f": {}, "l": link("p"), "p/a.go": {}})

		_, err := tree.Open(t.Context(), &plg.Descriptor{Key: "missing"})
		require.ErrorIs(t, err, fs.ErrNotExist)

		for _, key := range []string{"f", "l"} {
			_, err = tree.Open(t.Context(), &plg.Descriptor{Key: key})
			require.ErrorContains(t, err, "plugin directory "+key+": a plugin key is a directory name")
		}
	})
}

// versionOf returns the version Register gives the single, version-less plugin directory of files.
func versionOf(t *testing.T, files fs.FS) string {
	t.Helper()

	store := plg.NewFSStorageFromFS("v", files)

	descs, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, descs, 1)
	require.Empty(t, descs[0].Version)

	version, err := plg.LoadedVersion(t.Context(), store, &descs[0])
	require.NoError(t, err)
	require.Regexp(t, isContentHash, version)

	return version
}

func TestFSStorageVersionFallsBackToAStableContentHash(t *testing.T) {
	t.Parallel()

	const manifest = implementsLine + "language: go\nentrypoint: .\n"

	base := func() fstest.MapFS {
		return fstest.MapFS{
			"p/plugin.yaml": {Data: []byte(manifest)},
			"p/a.go":        {Data: []byte("package p // a\n")},
			"p/sub/b.txt":   {Data: []byte("b")},
		}
	}

	version := versionOf(t, base())
	assert.Equal(t, version, versionOf(t, base()), "the same files hash the same")

	renamedDir := fstest.MapFS{}
	for name, file := range base() {
		renamedDir["q"+strings.TrimPrefix(name, "p")] = file
	}

	assert.Equal(t, version, versionOf(t, renamedDir), "the key is not part of the content")

	disk := t.TempDir()
	for name, file := range base() {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(disk, name)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(disk, name), file.Data, 0o600))
	}

	require.NoError(t, os.Symlink("a.go", filepath.Join(disk, "p", "link.go")))
	assert.Equal(t, version, versionOf(t, os.DirFS(disk)),
		"a real directory hashes like the same files in memory; symlinks are skipped")

	// Each change keeps the directory a valid plugin; "path/data split" keeps the bytes of a path followed by
	// its contents ("sub/b.txt"+"b" becomes "sub/b.tx"+"tb").
	changes := map[string]func(fstest.MapFS){
		"edited":        func(m fstest.MapFS) { m["p/a.go"] = &fstest.MapFile{Data: []byte("package p // A\n")} },
		"renamed":       func(m fstest.MapFS) { m["p/c.go"] = m["p/a.go"]; delete(m, "p/a.go") },
		"moved":         func(m fstest.MapFS) { m["p/b.txt"] = m["p/sub/b.txt"]; delete(m, "p/sub/b.txt") },
		"added":         func(m fstest.MapFS) { m["p/c.go"] = &fstest.MapFile{Data: []byte("package p\n")} },
		"added empty":   func(m fstest.MapFS) { m["p/sub/empty"] = &fstest.MapFile{} },
		"removed":       func(m fstest.MapFS) { delete(m, "p/sub/b.txt") },
		"emptied":       func(m fstest.MapFS) { m["p/sub/b.txt"] = &fstest.MapFile{} },
		"manifest edit": func(m fstest.MapFS) { m["p/plugin.yaml"] = &fstest.MapFile{Data: []byte(manifest + "\n")} },
		"path/data split": func(m fstest.MapFS) {
			delete(m, "p/sub/b.txt")

			m["p/sub/b.tx"] = &fstest.MapFile{Data: []byte("tb")}
		},
	}

	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			files := base()
			change(files)

			assert.NotEqual(t, version, versionOf(t, files))
		})
	}
}

func TestDuplicateKeysAcrossFSStoragesFailRegister(t *testing.T) {
	t.Parallel()

	disk := plg.NewFSStorage("testdata/storages/embedded")
	embedded := embeddedStorage(t, "embedded", "embedded")

	err := plg.Register[pinger](t.Context(), newUnitContainer(t), "dup", plg.WithStorage(disk),
		plg.WithStorage(embedded))
	require.ErrorIs(t, err, plg.ErrDuplicateKey)
	require.ErrorContains(t, err, `"other"`)
	require.ErrorContains(t, err, "testdata/storages/embedded")
	require.ErrorContains(t, err, "and embedded")
}

func TestMalformedManifestFailsRegister(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "broken-plugin"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken-plugin", "plugin.yaml"), []byte(`{"language":`), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "no-manifest"), 0o750))

	ctr := newUnitContainer(t)

	err := plg.Register[pinger](t.Context(), ctr, "broken", plg.WithStorage(plg.NewFSStorage(dir)))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorIs(t, err, plg.ErrInvalidManifest)
	require.ErrorContains(t, err, "plugin directory broken-plugin")
	require.ErrorContains(t, err, dir, "the storage is named")

	require.NoError(t, os.RemoveAll(filepath.Join(dir, "broken-plugin")))

	err = plg.Register[pinger](t.Context(), ctr, "broken", plg.WithStorage(plg.NewFSStorage(dir)))
	require.ErrorIs(t, err, plg.ErrInvalidManifest)
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.ErrorContains(t, err, "plugin directory no-manifest")
}

// skipRecord is the part of a log record a skipped plugin is reported with; only skip records name a storage.
type skipRecord struct {
	Level      string `json:"level"`
	Message    string `json:"msg"`
	Key        string `json:"key"`
	Storage    string `json:"storage"`
	Language   string `json:"language"`
	Implements string `json:"implements"`
}

// decodeRecords returns the skip records among the JSON log lines in logs.
func decodeRecords(t *testing.T, logs *bytes.Buffer) []skipRecord {
	t.Helper()

	var records []skipRecord

	lines := bufio.NewScanner(logs)
	for lines.Scan() {
		var rec skipRecord

		require.NoError(t, sonic.Unmarshal(lines.Bytes(), &rec))

		if rec.Storage != "" {
			records = append(records, rec)
		}
	}

	require.NoError(t, lines.Err())

	return records
}

// TestLanguageAllowlist registers storages holding plugins in several languages, and plugins implementing another
// interface, with each language option: the plugins in a language left out and those implementing another
// interface, whatever their language, are skipped and logged, each with its reason.
func TestLanguageAllowlist(t *testing.T) {
	t.Parallel()

	pings, cohort := implementsOf[pinger](), "example.com/contracts/rewards.Cohort@v1.0.0"
	polyglot := listStorage{name: "poly", descs: []plg.Descriptor{
		{Key: "g", Implements: pings, Language: plg.LanguageGo, Entrypoint: "plugin.go"},
		{Key: "l", Implements: pings, Language: plg.LanguageLua, Entrypoint: "main.lua"},
		{Key: "t", Implements: pings, Language: plg.LanguageTypeScript, Entrypoint: "index.ts"},
		{Key: "x", Implements: pings, Language: "cobol", Entrypoint: "main.cbl"},
	}}
	goThenLua := listStorage{name: "poly", descs: polyglot.descs[:2]}
	luaOnly := listStorage{name: "scripts", descs: polyglot.descs[1:2]}
	scripts := listStorage{name: "scripts", descs: polyglot.descs[1:3]}
	goOnly := listStorage{name: "go", descs: polyglot.descs[:1]}
	cobol := listStorage{name: "odd", descs: polyglot.descs[3:]}
	luaDup := listStorage{name: "lua", descs: []plg.Descriptor{
		{Key: "g", Implements: pings, Language: plg.LanguageLua, Entrypoint: "a"},
	}}
	others := listStorage{name: "others", descs: []plg.Descriptor{
		{Key: "c", Implements: cohort, Language: plg.LanguageGo, Entrypoint: "plugin.go"},
		{Key: "y", Implements: cohort, Language: "cobol", Entrypoint: "main.cbl"},
	}}

	skipped := func(key, storage, lang string) skipRecord {
		return skipRecord{
			Level: "INFO", Message: "plg: plugin skipped: its language is not enabled", Key: "langs/" + key,
			Storage: storage, Language: lang, Implements: pings,
		}
	}
	elsewhere := func(key, lang string) skipRecord {
		return skipRecord{
			Level: "INFO", Message: "plg: plugin skipped: it implements another interface", Key: "langs/" + key,
			Storage: "others", Language: lang, Implements: cohort,
		}
	}

	tests := []struct {
		want     error
		name     string
		failsOn  string // an enabled plugin Register tried and failed to load, as the error names it
		storages []plg.Storage
		opts     []plg.RegisterOption
		skipped  []skipRecord
	}{
		{
			// The Lua plugin, enabled, fails: its guest is not linked into this test binary, or (with the
			// integration tests' blank import) it is but the plugin has no files.
			name: "lua only", storages: []plg.Storage{goThenLua}, opts: []plg.RegisterOption{plg.WithLuaPlugins()},
			failsOn: "l (storage poly)",
			skipped: []skipRecord{skipped("g", "poly", "go")},
		},
		{
			name: "golang only", storages: []plg.Storage{scripts, cobol},
			opts: []plg.RegisterOption{plg.WithGolangPlugins()},
			skipped: []skipRecord{
				skipped("l", "scripts", "lua"), skipped("t", "scripts", "typescript"), skipped("x", "odd", "cobol"),
			},
		},
		{
			// Likewise the TypeScript plugin: its guest is not linked, or it is but the plugin has no files.
			name: "typescript only", storages: []plg.Storage{goOnly, scripts},
			opts:    []plg.RegisterOption{plg.WithTypescriptPlugins()},
			failsOn: "t (storage scripts)",
			skipped: []skipRecord{skipped("g", "go", "go"), skipped("l", "scripts", "lua")},
		},
		{
			name: "every language by default", storages: []plg.Storage{luaOnly},
			failsOn: "l (storage scripts)",
		},
		{
			// Another interface's plugins are skipped first, in a language enabled or not.
			name: "another interface", storages: []plg.Storage{others, cobol},
			opts:    []plg.RegisterOption{plg.WithGolangPlugins()},
			skipped: []skipRecord{elsewhere("c", "go"), elsewhere("y", "cobol"), skipped("x", "odd", "cobol")},
		},
		{
			name: "duplicates count whatever the language", storages: []plg.Storage{goOnly, luaDup},
			opts: []plg.RegisterOption{plg.WithGolangPlugins()},
			want: plg.ErrDuplicateKey,
		},
		{
			name: "duplicates count whatever the interface", storages: []plg.Storage{goOnly, listStorage{
				name: "again", descs: []plg.Descriptor{
					{Key: "g", Implements: cohort, Language: plg.LanguageGo, Entrypoint: "plugin.go"},
				},
			}},
			opts: []plg.RegisterOption{plg.WithGolangPlugins()},
			want: plg.ErrDuplicateKey,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer

			ctr := plg.New(plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))

			t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

			opts := tc.opts
			for _, store := range tc.storages {
				opts = append(opts, plg.WithStorage(store))
			}

			err := plg.Register[pinger](t.Context(), ctr, "langs", opts...)

			switch {
			case tc.want != nil:
				require.ErrorIs(t, err, tc.want)
			case tc.failsOn != "":
				require.ErrorContains(t, err, tc.failsOn)
			default:
				require.NoError(t, err)

				for key := range plg.All[pinger](t.Context(), ctr) {
					assert.Fail(t, "a skipped plugin is not published", key)
				}
			}

			assert.Equal(t, tc.skipped, decodeRecords(t, &logs))
		})
	}
}

// FuzzFSStorageManifest feeds arbitrary plugin.yaml contents to an FS storage. A manifest is rejected with
// ErrInvalidManifest naming its directory, or yields a descriptor that satisfies every documented constraint
// and survives re-encoding unchanged.
func FuzzFSStorageManifest(f *testing.F) {
	for _, seed := range []string{
		implementsLine + "language: go\nentrypoint: plugin.go\nversion: 1.0.0\nmemoryLimit: 67108864\n",
		implementsLine + "language: lua\nentrypoint: main.lua\n",
		"{implements: a.b/c.D@v1.0.0, language: typescript, entrypoint: src/index.ts, memoryLimit: 0x100000000}",
		implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 4294967297\n",
		implementsLine + "language: go\nentrypoint: .\nmemoryLimit: 65536.0\n",
		implementsLine + "language: go\nentrypoint: ../x\n",
		implementsLine + "language: go\nentrypoint: .\nextra: true\n",
		implementsLine + "Language: go\nentrypoint: .\n",
		implementsLine + "language: go\nentrypoint: .\nversion: \"\\ud800\"\n",
		implementsLine + "language: go\nentrypoint: .\nlanguage: lua\n",
		implementsLine + "language: go\nentrypoint: \"\xff\"\n",
		implementsLine + "language: &a go\nentrypoint: *a\n",
		implementsLine + "<<: {language: go}\nentrypoint: .\n",
		implementsLine + "language: !x go\nentrypoint: .\n",
		implementsLine + "language: go\nentrypoint: .\n---\n",
		"implements: a.b/c.D@1.0\nlanguage: go\nentrypoint: .\n",
		"implements: a.b/c@v1.0.0\nlanguage: go\nentrypoint: .\n",
		"language: go\nentrypoint: .\n",
		"a: &a [*a]\n",
		"version: 2026-01-01\nlanguage: go\nentrypoint: '.'\nimplements: x.y/z.W@v0.0.0-20260101000000-abcdefabcdef\n",
		implementsLine + "language: lua\nentrypoint: main.lua\nsecrets: [stripe_key, db.password]\n",
		implementsLine + "language: lua\nentrypoint: main.lua\nsecrets: []\n",
		implementsLine + "language: lua\nentrypoint: main.lua\nsecrets: [a, a]\n",
		implementsLine + "language: lua\nentrypoint: main.lua\nsecrets: [9lives, '']\n",
		implementsLine + "language: lua\nentrypoint: main.lua\nsecrets: key\n",
		`null`,
		``,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, manifest []byte) {
		descs, err := plg.NewFSStorageFromFS("fuzz", fstest.MapFS{"p/plugin.yaml": {Data: manifest}}).List(t.Context())
		if err != nil {
			require.ErrorIs(t, err, plg.ErrInvalidManifest)
			require.ErrorContains(t, err, "plugin directory p")

			return
		}

		require.Len(t, descs, 1)

		desc := descs[0]
		assert.Equal(t, "p", desc.Key)
		assert.Contains(t, []plg.Language{plg.LanguageGo, plg.LanguageLua, plg.LanguageTypeScript}, desc.Language)
		assert.Regexp(t, `^[^@]+\.[^./@]+@v\d+\.\d+\.\d+([-][^+@]*)?$`, desc.Implements,
			"implements names an interface after its package, at a canonical semantic version")
		assert.True(t, desc.Entrypoint != "" && fs.ValidPath(desc.Entrypoint), "entrypoint %q", desc.Entrypoint)
		assert.True(t, desc.MemoryLimit == 0 || desc.MemoryLimit >= 64<<10 && desc.MemoryLimit <= 4<<30,
			"memoryLimit %d is zero or from one wasm page to 4 GiB, as Register requires", desc.MemoryLimit)
		assert.LessOrEqual(t, len(desc.Secrets), plg.MaxSecrets)

		for i, name := range desc.Secrets {
			assert.Regexp(t, `^[A-Za-z][A-Za-z0-9_.-]{0,63}$`, name, "a secret name")
			assert.NotContains(t, desc.Secrets[:i], name, "a secret is declared once")
		}

		// Re-encoding the descriptor's fields gives the same plugin (an empty version stays empty: Register
		// hashes the files it loads).
		fields := map[string]any{
			"implements": desc.Implements, "language": desc.Language, "entrypoint": desc.Entrypoint,
			"version": desc.Version,
		}
		if desc.MemoryLimit != 0 {
			fields["memoryLimit"] = desc.MemoryLimit
		}

		if desc.Secrets != nil {
			fields["secrets"] = desc.Secrets
		}

		reencoded, err := yaml.Marshal(fields)
		require.NoError(t, err)

		again, err := manifestStorage(string(reencoded)).List(t.Context())
		require.NoError(t, err, "%s", reencoded)
		assert.Equal(t, []plg.Descriptor{desc}, again, "%s", reencoded)
	})
}

// BenchmarkFSStorageList measures what Register pays per FS storage before loading: reading every plugin.yaml.
func BenchmarkFSStorageList(b *testing.B) {
	sub, err := fs.Sub(storagesFS, "testdata/storages/disk")
	require.NoError(b, err)

	for name, store := range map[string]*plg.FSStorage{
		"dir":   plg.NewFSStorage(diskDir),
		"embed": plg.NewFSStorageFromFS("embed", sub),
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				descs, err := store.List(b.Context())
				if err != nil || len(descs) != 3 {
					b.Fatal(descs, err)
				}
			}
		})
	}
}

// secretList returns n distinct secret names, comma-separated, for a plugin.yaml's flow sequence.
func secretList(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("s%d", i)
	}

	return strings.Join(names, ", ")
}
