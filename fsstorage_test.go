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
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
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

// manifestStorage returns a storage holding one plugin directory, p, with the given plugin.json and plugin.go.
func manifestStorage(manifest string) *plg.FSStorage {
	return plg.NewFSStorageFromFS("mapfs", fstest.MapFS{
		"p/plugin.json": {Data: []byte(manifest)},
		"p/plugin.go":   {Data: []byte("package p\n")},
	})
}

const isContentHash = `^sha256:[0-9a-f]{64}$`

func TestFSStorageListsPluginDirectories(t *testing.T) {
	t.Parallel()

	store := plg.NewFSStorage(diskDir)
	assert.Equal(t, diskDir, store.Name())

	descs, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, descs, 3, "README.md is not a plugin directory")

	// multifile has no version: Register hashes the files it loads (TestRegisterHashesVersionlessPlugins).
	want := []plg.Descriptor{
		{Key: "multifile", Language: plg.LanguageGo, Entrypoint: ".", MemoryLimit: 64 << 20},
		{Key: "scripted", Language: plg.LanguageLua, Entrypoint: "main.lua", Version: "0.1.0"},
		{Key: "single", Language: plg.LanguageGo, Entrypoint: "plugin.go", Version: "1.2.3"},
	}
	assert.Equal(t, want, descs, "sorted by key")

	embedded, err := embeddedStorage(t, "embed", "disk").List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, want, embedded, "an embed.FS of the same tree lists the same plugins")

	files, err := store.Open(t.Context(), descs[2])
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
		".hidden/plugin.json":  {Data: []byte("{malformed")},
		"p/plugin.json":        {Data: []byte(`{"language":"go","entrypoint":".","version":"1"}`)},
		"p/.notes/plugin.json": {Data: []byte("{malformed")},
	})

	descs, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plg.Descriptor{{Key: "p", Language: plg.LanguageGo, Entrypoint: ".", Version: "1"}}, descs)
}

func TestFSStorageParsesManifests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		manifest string
		want     plg.Descriptor
	}{
		{
			manifest: `{"language":"go","entrypoint":"plugin.go","version":"v1"}`,
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: "plugin.go", Version: "v1"},
		},
		{
			manifest: " \n{\"version\": \"2\", \"entrypoint\": \"main.lua\", \"language\": \"lua\"}\n\t",
			want:     plg.Descriptor{Language: plg.LanguageLua, Entrypoint: "main.lua", Version: "2"},
		},
		{
			manifest: `{"language":"typescript","entrypoint":"src/index.ts","version":"3","memoryLimit":null}`,
			want:     plg.Descriptor{Language: plg.LanguageTypeScript, Entrypoint: "src/index.ts", Version: "3"},
		},
		{
			manifest: `{"language":"go","entrypoint":".","version":"4","memoryLimit":1}`,
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: ".", Version: "4", MemoryLimit: 1},
		},
		{
			manifest: `{"language":"go","entrypoint":".","version":"5","memoryLimit":4294967296}`,
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: ".", Version: "5", MemoryLimit: 4 << 30},
		},
		{
			manifest: `{"language":"go","entrypoint":".","version":"\u00e9t\u00e9"}`,
			want:     plg.Descriptor{Language: plg.LanguageGo, Entrypoint: ".", Version: "été"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.manifest, func(t *testing.T) {
			t.Parallel()

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

	manifest := `{"language":"go","entrypoint":"."}`

	parsed, err := plg.ParseManifest([]byte(manifest))
	require.NoError(t, err)
	assert.Equal(t, plg.Descriptor{Language: plg.LanguageGo, Entrypoint: "."}, parsed)

	descs, err := manifestStorage(manifest).List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plg.Descriptor{{Key: "p", Language: plg.LanguageGo, Entrypoint: "."}}, descs,
		"FSStorage leaves it to Register too, which hashes the files it loads")
}

func TestFSStorageRejectsMalformedManifests(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":                ``,
		"not JSON":             `language = "go"`,
		"unterminated":         `{"language":"go","entrypoint":"."`,
		"array":                `[{"language":"go","entrypoint":"."}]`,
		"null":                 `null`,
		"trailing data":        `{"language":"go","entrypoint":"."} {}`,
		"unknown key":          `{"language":"go","entrypoint":".","memory_limit":1}`,
		"key case":             `{"Language":"go","entrypoint":"."}`,
		"language type":        `{"language":1,"entrypoint":"."}`,
		"no language":          `{"entrypoint":"."}`,
		"unknown language":     `{"language":"python","entrypoint":"main.py"}`,
		"language case":        `{"language":"Go","entrypoint":"."}`,
		"no entrypoint":        `{"language":"go"}`,
		"empty entrypoint":     `{"language":"go","entrypoint":""}`,
		"escaping entrypoint":  `{"language":"go","entrypoint":"../other"}`,
		"absolute entrypoint":  `{"language":"go","entrypoint":"/plugin.go"}`,
		"trailing slash":       `{"language":"go","entrypoint":"pkg/"}`,
		"version type":         `{"language":"go","entrypoint":".","version":1}`,
		"control character":    "{\"language\":\"go\",\"entrypoint\":\".\",\"version\":\"a\x01b\"}",
		"zero memory limit":    `{"language":"go","entrypoint":".","memoryLimit":0}`,
		"negative memoryLimit": `{"language":"go","entrypoint":".","memoryLimit":-1}`,
		"fractional limit":     `{"language":"go","entrypoint":".","memoryLimit":1.5}`,
		"limit as a string":    `{"language":"go","entrypoint":".","memoryLimit":"64MiB"}`,
		"limit over 4 GiB":     `{"language":"go","entrypoint":".","memoryLimit":4294967297}`,
		"limit over uint64":    `{"language":"go","entrypoint":".","memoryLimit":18446744073709551616}`,
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
			"a/plugin.json": {Data: []byte(`{"language":"go","entrypoint":".","version":"1"}`)},
			"b/plugin.go":   {Data: []byte("package b\n")},
		},
		"a directory": {"b/plugin.json/x": {Data: []byte("{}")}},
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

// TestFSStorageBoundsManifestReads lists plugin directories whose plugin.json is a FIFO (opening it would wait
// for a writer forever), a link to /dev/zero (an endless read) or a regular file over MaxManifestSize: each fails
// at once with ErrInvalidManifest naming the directory, through an os.Root and through an os.DirFS alike.
func TestFSStorageBoundsManifestReads(t *testing.T) {
	t.Parallel()

	valid := `{"language":"go","entrypoint":"."}`
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
			tc.make(t, filepath.Join(dir, "p", "plugin.json"))

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

func TestFSStorageListAndOpenErrors(t *testing.T) {
	t.Parallel()

	missing := plg.NewFSStorage(filepath.Join(t.TempDir(), "missing"))

	_, err := missing.List(t.Context())
	require.ErrorIs(t, err, fs.ErrNotExist)

	_, err = missing.Open(t.Context(), plg.Descriptor{Key: "p"})
	require.ErrorIs(t, err, fs.ErrNotExist)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = manifestStorage(`{"language":"go","entrypoint":"."}`).List(ctx)
	require.ErrorIs(t, err, context.Canceled)

	// Opening a plugin copies every file and directory below it; listing reads only the manifests.
	files := fstest.MapFS{
		"p/plugin.json": {Data: []byte(`{"language":"go","entrypoint":"."}`)},
		"p/plugin.go":   {Data: []byte("package p\n")},
		"p/sub/x.go":    {Data: []byte("package sub\n")},
	}

	for name, broken := range map[string]fs.FS{
		"unopenable file":     unreadableFS{files: files, broken: "p/plugin.go"},
		"unlistable dir":      unreadableFS{files: files, broken: "p/sub"},
		"unreadable contents": failingReadFS{files: files, broken: "p/sub/x.go"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := plg.NewFSStorageFromFS("broken", broken)

			descs, err := store.List(t.Context())
			require.NoError(t, err)
			require.Len(t, descs, 1)

			_, err = store.Open(t.Context(), descs[0])
			require.ErrorIs(t, err, errStorage)
			require.ErrorContains(t, err, "plugin directory p")
		})
	}
}

// failingReadFS is files, except that reading the contents of the file named broken fails.
type failingReadFS struct {
	files  fstest.MapFS
	broken string
}

func (f failingReadFS) Open(name string) (fs.File, error) {
	file, err := f.files.Open(name)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	if name == f.broken {
		return failingRead{file}, nil
	}

	return file, nil
}

type failingRead struct {
	fs.File
}

func (failingRead) Read([]byte) (int, error) { return 0, errStorage }

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

			_, err := store.Open(t.Context(), plg.Descriptor{Key: tc.key})
			require.ErrorIs(t, err, tc.want)
		})
	}

	t.Run("a file", func(t *testing.T) {
		t.Parallel()

		_, err := store.Open(t.Context(), plg.Descriptor{Key: "README.md"})
		require.ErrorContains(t, err, "plugin directory README.md")
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

	version, err := plg.LoadedVersion(t.Context(), store, descs[0])
	require.NoError(t, err)
	require.Regexp(t, isContentHash, version)

	return version
}

func TestFSStorageVersionFallsBackToAStableContentHash(t *testing.T) {
	t.Parallel()

	const manifest = `{"language":"go","entrypoint":"."}`

	base := func() fstest.MapFS {
		return fstest.MapFS{
			"p/plugin.json": {Data: []byte(manifest)},
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
		"manifest edit": func(m fstest.MapFS) { m["p/plugin.json"] = &fstest.MapFile{Data: []byte(manifest + "\n")} },
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken-plugin", "plugin.json"), []byte(`{"language":`), 0o600))
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
	Level    string `json:"level"`
	Key      string `json:"key"`
	Storage  string `json:"storage"`
	Language string `json:"language"`
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

func TestLanguageAllowlist(t *testing.T) {
	t.Parallel()

	polyglot := listStorage{name: "poly", descs: []plg.Descriptor{
		{Key: "g", Language: plg.LanguageGo, Entrypoint: "plugin.go"},
		{Key: "l", Language: plg.LanguageLua, Entrypoint: "main.lua"},
		{Key: "t", Language: plg.LanguageTypeScript, Entrypoint: "index.ts"},
		{Key: "x", Language: "cobol", Entrypoint: "main.cbl"},
	}}
	goThenLua := listStorage{name: "poly", descs: polyglot.descs[:2]}
	luaOnly := listStorage{name: "scripts", descs: polyglot.descs[1:2]}
	scripts := listStorage{name: "scripts", descs: polyglot.descs[1:3]}
	goOnly := listStorage{name: "go", descs: polyglot.descs[:1]}
	cobol := listStorage{name: "odd", descs: polyglot.descs[3:]}
	luaDup := listStorage{name: "lua", descs: []plg.Descriptor{{Key: "g", Language: plg.LanguageLua, Entrypoint: "a"}}}

	skipped := func(key, storage, lang string) skipRecord {
		return skipRecord{Level: "INFO", Key: "langs/" + key, Storage: storage, Language: lang}
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
			name: "duplicates count whatever the language", storages: []plg.Storage{goOnly, luaDup},
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

// FuzzFSStorageManifest feeds arbitrary plugin.json contents to an FS storage. A manifest is rejected with
// ErrInvalidManifest naming its directory, or yields a descriptor that satisfies every documented constraint
// and survives re-encoding unchanged.
func FuzzFSStorageManifest(f *testing.F) {
	for _, seed := range []string{
		`{"language":"go","entrypoint":"plugin.go","version":"1.0.0","memoryLimit":67108864}`,
		`{"language":"lua","entrypoint":"main.lua"}`,
		`{"language":"typescript","entrypoint":"src/index.ts","memoryLimit":4294967296}`,
		`{"language":"go","entrypoint":".","memoryLimit":4294967297}`,
		`{"language":"go","entrypoint":"../x"}`,
		`{"language":"go","entrypoint":".","extra":true}`,
		`{"Language":"go","entrypoint":"."}`,
		`{"language":"go","entrypoint":".","version":"\ud800"}`,
		`{"language":"go","entrypoint":".","language":"lua"}`,
		"{\"language\":\"go\",\"entrypoint\":\"\xff\"}",
		`{"language":"go","entrypoint":"."}}`,
		`null`,
		``,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, manifest []byte) {
		descs, err := plg.NewFSStorageFromFS("fuzz", fstest.MapFS{"p/plugin.json": {Data: manifest}}).List(t.Context())
		if err != nil {
			require.ErrorIs(t, err, plg.ErrInvalidManifest)
			require.ErrorContains(t, err, "plugin directory p")

			return
		}

		require.Len(t, descs, 1)

		desc := descs[0]
		assert.Equal(t, "p", desc.Key)
		assert.Contains(t, []plg.Language{plg.LanguageGo, plg.LanguageLua, plg.LanguageTypeScript}, desc.Language)
		assert.True(t, desc.Entrypoint != "" && fs.ValidPath(desc.Entrypoint), "entrypoint %q", desc.Entrypoint)
		assert.LessOrEqual(t, desc.MemoryLimit, uint64(4<<30))

		// Re-encoding the descriptor's fields gives the same plugin (an empty version stays empty: Register
		// hashes the files it loads).
		fields := map[string]any{"language": desc.Language, "entrypoint": desc.Entrypoint, "version": desc.Version}
		if desc.MemoryLimit != 0 {
			fields["memoryLimit"] = desc.MemoryLimit
		}

		reencoded, err := sonic.Marshal(fields)
		require.NoError(t, err)

		again, err := manifestStorage(string(reencoded)).List(t.Context())
		require.NoError(t, err, "%s", reencoded)
		assert.Equal(t, []plg.Descriptor{desc}, again, "%s", reencoded)
	})
}

// BenchmarkFSStorageList measures what Register pays per FS storage before loading: reading every plugin.json.
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
