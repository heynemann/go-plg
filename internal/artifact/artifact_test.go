package artifact_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/artifact"
	"github.com/heynemann/go-plg/internal/memfs"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// pluginFiles returns a plugin's files: a manifest, an entrypoint and a shared directory, with a file ("lib.lua")
// path sorts after the directory lib/ walks before it.
func pluginFiles() map[string]string {
	return map[string]string{
		"plugin.yaml":    "implements: example.com/rewards/cohort.Provider@v1.2.0\nlanguage: lua\nentrypoint: main.lua\n",
		"main.lua":       "function Create() return {} end\n",
		"lib/tiers.lua":  "return {}\n",
		"lib.lua":        "return 1\n",
		"lib/x/deep.lua": "return 2\n",
	}
}

// build returns files as a tree, added in the order map iteration gives.
func build(t *testing.T, files map[string]string) *memfs.FS {
	t.Helper()

	var tree memfs.Builder

	for name, data := range files {
		require.NoError(t, tree.Add(name, []byte(data)))
	}

	built, err := tree.Build()
	require.NoError(t, err)

	return built
}

// record is a provenance record of the plugin's files.
func record(t *testing.T, files *memfs.FS) *adminjson.Provenance {
	t.Helper()

	return &adminjson.Provenance{
		Contract: adminjson.Module{Path: "example.com/rewards", Version: "v1.2.0"},
		Digest:   artifact.Digest(files), Commit: "0123abcd", Plg: "(devel)", Dirty: true,
		Report: sonic.NoCopyRawMessage(`{"version": 1, "plugins": [{"plugin": "silver"}], "valid": true}`),
	}
}

// write returns the artifact of files with provenance.
func write(t *testing.T, files *memfs.FS, provenance *adminjson.Provenance) []byte {
	t.Helper()

	var out bytes.Buffer

	require.NoError(t, artifact.Write(&out, files, provenance))

	return out.Bytes()
}

// entry is a tar entry of an artifact: its header and contents.
type entry struct {
	header *tar.Header
	data   string
}

// entries returns the gzip header and the entries of the artifact data.
func entries(t *testing.T, data []byte) (gzip.Header, []entry) {
	t.Helper()

	unzipped, err := gzip.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	archive := tar.NewReader(unzipped)

	var read []entry

	for {
		header, nextErr := archive.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		require.NoError(t, nextErr)

		contents, readErr := io.ReadAll(archive)
		require.NoError(t, readErr)

		read = append(read, entry{header: header, data: string(contents)})
	}

	rest, err := io.ReadAll(unzipped)
	require.NoError(t, err, "the gzip stream ends with its checksum")
	assert.Empty(t, rest, "nothing follows the archive")

	return unzipped.Header, read
}

// TestWriteIsReproducible writes the same files twice, added in different orders: the bytes are the same, and
// nothing in them depends on when or where they were written: the gzip header has no name or time, every entry is
// a regular file of mode 0644 dated the Unix epoch without owner, the provenance record (its format set, the report
// kept as written) comes first, then every file under plugin/ in the order fs.WalkDir visits them. A changed
// file or record changes the bytes.
func TestWriteIsReproducible(t *testing.T) {
	t.Parallel()

	files := build(t, pluginFiles())
	first := write(t, files, record(t, files))

	for range 5 { // map iteration order changes how the tree is built, not what is written
		again := build(t, pluginFiles())
		assert.Equal(t, first, write(t, again, record(t, again)))
	}

	header, written := entries(t, first)
	assert.Empty(t, header.Name)
	assert.True(t, header.ModTime.IsZero(), "the gzip header has no time")
	assert.Empty(t, header.Comment)

	names := make([]string, len(written))
	for i, got := range written {
		names[i] = got.header.Name

		assert.Equal(t, byte(tar.TypeReg), got.header.Typeflag, got.header.Name)
		assert.Equal(t, int64(artifact.FileMode), got.header.Mode, got.header.Name)
		assert.True(t, got.header.ModTime.Equal(time.Unix(0, 0)), got.header.Name)
		assert.Zero(t, got.header.Uid, got.header.Name)
		assert.Zero(t, got.header.Gid, got.header.Name)
		assert.Empty(t, got.header.Uname+got.header.Gname, got.header.Name)
	}

	assert.Equal(t, []string{
		"provenance.json", "plugin/lib/tiers.lua", "plugin/lib/x/deep.lua", "plugin/lib.lua", "plugin/main.lua",
		"plugin/plugin.yaml",
	}, names)

	for _, got := range written[1:] {
		assert.Equal(t, pluginFiles()[strings.TrimPrefix(got.header.Name, "plugin/")], got.data, got.header.Name)
	}

	var decoded map[string]any

	require.NoError(t, sonic.ConfigStd.UnmarshalFromString(written[0].data, &decoded))
	assert.Equal(t, map[string]any{
		"contract":   map[string]any{"path": "example.com/rewards", "version": "v1.2.0"},
		"digest":     artifact.Digest(files),
		"commit":     "0123abcd",
		"plgVersion": "(devel)",
		"dirty":      true,
		"format":     float64(artifact.Format),
		"report": map[string]any{
			"version": float64(1),
			"plugins": []any{map[string]any{"plugin": "silver"}},
			"valid":   true,
		},
	}, decoded)

	changed := pluginFiles() // a fresh copy

	changed["lib/tiers.lua"] = "return { gold = 1 }\n"
	edited := build(t, changed)
	assert.NotEqual(t, first, write(t, edited, record(t, files)), "a changed file")

	committed := record(t, files)
	committed.Commit = "4567cdef"
	assert.NotEqual(t, first, write(t, files, committed), "a changed record")
}

// TestDigestHashesPathsAndContents pins the content digest to its definition, computed here over fs.WalkDir as
// Register computed a versionless plugin's version before artifacts shared it: every regular file's path (with
// its length) and SHA-256, in walk order. Renaming, moving or editing a file changes it; how the tree was built
// does not.
func TestDigestHashesPathsAndContents(t *testing.T) {
	t.Parallel()

	files := build(t, pluginFiles())
	sum := sha256.New()

	require.NoError(t, fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		data, readErr := fs.ReadFile(files, name)
		contents := sha256.Sum256(data)
		sum.Write(append(append(binary.AppendVarint(nil, int64(len(name))), name...), contents[:]...))

		return readErr //nolint:wrapcheck // the walk's own error, reported by require
	}))

	digest := artifact.Digest(files)
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum.Sum(nil)), digest)
	assert.Equal(t, digest, artifact.Digest(build(t, pluginFiles())))

	for name, change := range map[string]func(map[string]string){
		"edited":  func(files map[string]string) { files["main.lua"] += "-- edited\n" },
		"renamed": func(files map[string]string) { files["main2.lua"] = files["main.lua"]; delete(files, "main.lua") },
		"moved":   func(files map[string]string) { files["lib/lib.lua"] = files["lib.lua"]; delete(files, "lib.lua") },
		"added":   func(files map[string]string) { files["empty"] = "" },
	} {
		changed := pluginFiles() // a fresh copy

		change(changed)
		assert.NotEqual(t, digest, artifact.Digest(build(t, changed)), name)
	}

	assert.Equal(t, "sha256:"+hex.EncodeToString(sha256.New().Sum(nil)), artifact.Digest(build(t, nil)),
		"no files: the hash of nothing")
}

// failingWriter fails every write after the first ok bytes.
type failingWriter struct {
	ok int
}

var errWrite = errors.New("disk full")

func (w *failingWriter) Write(buf []byte) (int, error) {
	if len(buf) > w.ok {
		n := w.ok
		w.ok = 0

		return n, errWrite
	}

	w.ok -= len(buf)

	return len(buf), nil
}

// TestWriteFails writes nothing for a record whose report is not JSON or that is over the limit, and fails when
// the writer does, at any point.
func TestWriteFails(t *testing.T) {
	t.Parallel()

	files := build(t, pluginFiles())

	broken := record(t, files)
	broken.Report = sonic.NoCopyRawMessage(`{"not": json`)

	var out bytes.Buffer

	require.ErrorContains(t, artifact.Write(&out, files, broken), "writing the provenance record")
	assert.Zero(t, out.Len())

	huge := record(t, files)
	huge.Report = sonic.NoCopyRawMessage(`"` + strings.Repeat("x", artifact.MaxProvenanceSize) + `"`)
	require.ErrorIs(t, artifact.Write(&out, files, huge), artifact.ErrProvenanceSize)
	assert.Zero(t, out.Len())

	whole := len(write(t, files, record(t, files)))
	for _, ok := range []int{0, 10, whole - 1} { // the gzip header, the archive, the checksum
		err := artifact.Write(&failingWriter{ok: ok}, files, record(t, files))
		require.ErrorIs(t, err, errWrite, "after %d bytes", ok)
		require.ErrorContains(t, err, "writing the artifact", "after %d bytes", ok)
	}
}
