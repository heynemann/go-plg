package plg_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/artifact"
	"github.com/heynemann/go-plg/internal/memfs"
)

// artifactFiles returns the files of the artifacts these tests open.
func artifactFiles() map[string]string {
	return map[string]string{
		"plugin.yaml":   "implements: example.com/rewards/cohort.Provider@v1.2.0\nlanguage: lua\nentrypoint: main.lua\n",
		"main.lua":      "function Create() return {} end\n",
		"lib/tiers.lua": "return {}\n",
	}
}

// artifactTree returns files as a tree.
func artifactTree(t *testing.T, files map[string]string) *memfs.FS {
	t.Helper()

	var tree memfs.Builder

	for name, data := range files {
		require.NoError(t, tree.Add(name, []byte(data)))
	}

	built, err := tree.Build()
	require.NoError(t, err)

	return built
}

// artifactRecord is a provenance record of files.
func artifactRecord(files *memfs.FS) *adminjson.Provenance {
	return &adminjson.Provenance{
		Contract: adminjson.Module{Path: "example.com/rewards", Version: "v1.2.0"},
		Digest:   artifact.Digest(files), Commit: "0123abcd", Plg: "v0.9.0", Dirty: true,
		Report: sonic.NoCopyRawMessage(`{"plugins":[{"plugin":"silver","valid":true}],"version":1,"valid":true}`),
	}
}

// packArtifact returns the artifact plg pack writes for files.
func packArtifact(t *testing.T, files map[string]string) []byte {
	t.Helper()

	tree := artifactTree(t, files)

	var out bytes.Buffer

	require.NoError(t, artifact.Write(&out, tree, artifactRecord(tree)))

	return out.Bytes()
}

// openArtifact opens the artifact data.
func openArtifact(data []byte) (*plg.Artifact, error) {
	return plg.OpenArtifact(bytes.NewReader(data), int64(len(data))) //nolint:wrapcheck // the tests check plg's error
}

// rawEntry is an entry of a hand-made archive: its header (a regular file named name of len(data) bytes unless
// set), and its data.
type rawEntry struct {
	header *tar.Header
	name   string
	data   string
}

// rawArchive returns a gzip-compressed tar archive of entries, then trailing bytes of zeros after the archive's
// end; with open set, the archive is cut after the last header, its data missing.
//
//nolint:revive // flag-parameter: open picks the one malformed shape (data missing) the tests need besides whole ones
func rawArchive(t *testing.T, trailing int64, open bool, entries ...rawEntry) []byte {
	t.Helper()

	var out bytes.Buffer

	compressed := gzip.NewWriter(&out)
	archive := tar.NewWriter(compressed)

	for i, entry := range entries {
		header := entry.header
		if header == nil {
			header = &tar.Header{Typeflag: tar.TypeReg, Name: entry.name, Size: int64(len(entry.data)), Mode: 0o644}
		}

		require.NoError(t, archive.WriteHeader(header))

		if open && i == len(entries)-1 {
			require.NoError(t, compressed.Close())

			return out.Bytes()
		}

		_, err := io.WriteString(archive, entry.data)
		require.NoError(t, err)
	}

	require.NoError(t, archive.Close())

	_, err := io.CopyN(compressed, zeros{}, trailing)
	require.NoError(t, err)
	require.NoError(t, compressed.Close())

	return out.Bytes()
}

// zeros reads zeros forever.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}

// provenanceEntry is the provenance record entry of files (default artifactFiles()) as plg pack writes it.
func provenanceEntry(t *testing.T, files map[string]string) rawEntry {
	t.Helper()

	if files == nil {
		files = artifactFiles()
	}

	record := artifactRecord(artifactTree(t, files))
	record.Format = artifact.Format

	data, err := sonic.ConfigStd.Marshal(record)
	require.NoError(t, err)

	return rawEntry{name: artifact.ProvenanceName, data: string(data)}
}

// fileEntries are the entries of artifactFiles().
func fileEntries() []rawEntry {
	return []rawEntry{
		{name: "plugin/lib/tiers.lua", data: artifactFiles()["lib/tiers.lua"]},
		{name: "plugin/main.lua", data: artifactFiles()["main.lua"]},
		{name: "plugin/plugin.yaml", data: artifactFiles()["plugin.yaml"]},
	}
}

// TestOpenArtifactReturnsItsFilesAndProvenance opens what plg pack writes: the plugin's files, as an fs.FS that
// passes fstest.TestFS, and the provenance it records. A hand-made archive of the same entries opens the same.
func TestOpenArtifactReturnsItsFilesAndProvenance(t *testing.T) {
	t.Parallel()

	art, err := openArtifact(packArtifact(t, artifactFiles()))
	require.NoError(t, err)

	require.NoError(t, fstest.TestFS(art, "plugin.yaml", "main.lua", "lib/tiers.lua"))

	for name, want := range artifactFiles() {
		got, readErr := fs.ReadFile(art, name)
		require.NoError(t, readErr)
		assert.Equal(t, want, string(got), name)
	}

	entries, err := art.ReadDir(".")
	require.NoError(t, err)
	assert.Len(t, entries, 3)

	info, err := art.Stat("lib")
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	manifest, err := art.ReadFile("plugin.yaml")
	require.NoError(t, err)

	desc, err := plg.ParseManifest(manifest)
	require.NoError(t, err)
	assert.Equal(t, plg.LanguageLua, desc.Language, "a storage lists an artifact by its plugin.yaml")

	assert.Equal(t, plg.Provenance{
		Contract: plg.ContractModule{Path: "example.com/rewards", Version: "v1.2.0"},
		Digest:   artifact.Digest(artifactTree(t, artifactFiles())), Commit: "0123abcd", PlgVersion: "v0.9.0",
		Dirty:  true,
		Report: []byte(`{"plugins":[{"plugin":"silver","valid":true}],"version":1,"valid":true}`),
	}, art.Provenance())

	handMade, err := openArtifact(rawArchive(t, 0, false,
		append([]rawEntry{provenanceEntry(t, nil)}, fileEntries()...)...))
	require.NoError(t, err)
	assert.Equal(t, art.Provenance(), handMade.Provenance())
}

// TestOpenArtifactRejectsDamagedArchives cuts an artifact short at every kind of place and flips its bytes:
// each fails with ErrInvalidArtifact, never with files.
func TestOpenArtifactRejectsDamagedArchives(t *testing.T) {
	t.Parallel()

	data := packArtifact(t, artifactFiles())

	for _, cut := range []int{0, 5, 10, 64, len(data) / 2, len(data) - 8, len(data) - 1} {
		art, err := openArtifact(data[:cut])
		require.ErrorIs(t, err, plg.ErrInvalidArtifact, "cut at %d of %d", cut, len(data))
		assert.Nil(t, art)
		assert.Contains(t, err.Error(), "plg: opening an artifact: plg: invalid artifact: ")
	}

	for _, at := range []int{len(data) / 2, len(data) - 6} { // the compressed data, the checksum
		damaged := bytes.Clone(data)
		damaged[at] ^= 0xff

		_, err := openArtifact(damaged)
		require.ErrorIs(t, err, plg.ErrInvalidArtifact, "byte %d flipped", at)
	}

	_, err := openArtifact([]byte("not an artifact"))
	require.ErrorIs(t, err, plg.ErrInvalidArtifact)

	cutData := rawArchive(t, 0, true, provenanceEntry(t, nil), rawEntry{name: "plugin/main.lua", data: "x"})
	_, err = openArtifact(cutData)
	require.ErrorIs(t, err, plg.ErrInvalidArtifact, "a file's data missing")
	require.ErrorContains(t, err, "reading main.lua")
}

// TestOpenArtifactHoldsFilesToTheStorageLimits opens archives over each limit of a plugin's files: a file stating
// more than MaxPluginFileSize bytes (failing before its data is read: there is none), files over MaxPluginSize
// together, more than MaxPluginEntries files, paths over MaxPluginPathBytes, and a stream that decompresses past
// what those limits allow. Each fails with ErrPluginTooLarge, naming what brought it over.
func TestOpenArtifactHoldsFilesToTheStorageLimits(t *testing.T) {
	t.Parallel()

	head := func() []rawEntry { return []rawEntry{provenanceEntry(t, nil)} }

	huge := rawArchive(t, 0, true, append(head(), rawEntry{header: &tar.Header{
		Typeflag: tar.TypeReg, Name: "plugin/huge.bin", Size: plg.MaxPluginFileSize + 1, Mode: 0o644,
	}})...)
	_, err := openArtifact(huge)
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
	require.ErrorContains(t, err, "huge.bin is over 16777216 bytes")

	full := strings.Repeat("\x00", plg.MaxPluginFileSize)
	together := head()

	for _, name := range []string{"a", "b", "c", "d", "e"} {
		together = append(together, rawEntry{name: "plugin/" + name, data: full})
	}

	_, err = openArtifact(rawArchive(t, 0, false, together...))
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
	require.ErrorContains(t, err, "e brings the plugin's files over 67108864 bytes")

	many := head()

	for i := range plg.MaxPluginEntries + 1 {
		many = append(many, rawEntry{name: fmt.Sprintf("plugin/f%05d", i)})
	}

	_, err = openArtifact(rawArchive(t, 0, false, many...))
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
	require.ErrorContains(t, err, "brings the plugin over 16384 entries")

	long := head()
	for i := range plg.MaxPluginPathBytes/4000 + 1 { // paths of 4000 bytes each
		long = append(long, rawEntry{name: fmt.Sprintf("plugin/%04d", i) + strings.Repeat("x", 3996)})
	}

	_, err = openArtifact(rawArchive(t, 0, false, long...))
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
	require.ErrorContains(t, err, "brings the plugin's entry paths over 2097152 bytes")

	// Directories count as a storage's listing counts them: once each, with its path. One file 1,500 directories
	// deep has a 3 KB path but brings 2.25 MB of directory paths; 8,193 files in a directory each are 16,386
	// entries. Both are refused while reading the headers, before the plugin's files are built.
	deep := strings.Repeat("d/", 1500) + "f"
	_, err = openArtifact(rawArchive(t, 0, false, append(head(), rawEntry{name: "plugin/" + deep})...))
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
	require.ErrorContains(t, err, "brings the plugin's entry paths over 2097152 bytes")

	dirs := head()
	for i := range plg.MaxPluginEntries/2 + 1 {
		dirs = append(dirs, rawEntry{name: fmt.Sprintf("plugin/d%05d/f", i)})
	}

	_, err = openArtifact(rawArchive(t, 0, false, dirs...))
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
	require.ErrorContains(t, err, "brings the plugin over 16384 entries")

	bomb := rawArchive(t, 128<<20, false, append([]rawEntry{provenanceEntry(t, nil)}, fileEntries()...)...)
	require.Less(t, len(bomb), 1<<20, "128 MiB of zeros compress well")

	_, err = openArtifact(bomb)
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)
	require.ErrorContains(t, err, "the artifact decompresses to more than")
}

// TestOpenArtifactRejectsWhatPackNeverWrites opens archives holding entries outside the plugin's directory (path
// traversal, absolute and unclean paths), entries that are not regular files (links, directories, devices), a file
// twice or as a directory too, and archives whose provenance record is missing, doubled, not a file, too large,
// not JSON, of another format or of other files, or that lack plugin.yaml: each fails with ErrInvalidArtifact.
func TestOpenArtifactRejectsWhatPackNeverWrites(t *testing.T) {
	t.Parallel()

	provenance := provenanceEntry(t, nil)
	files := fileEntries()
	with := func(entries ...rawEntry) []rawEntry {
		return append(append([]rawEntry{provenance}, files...), entries...)
	}
	header := func(typ byte, name, link string) rawEntry {
		return rawEntry{header: &tar.Header{Typeflag: typ, Name: name, Linkname: link, Mode: 0o644}}
	}
	record := func(edit func(*adminjson.Provenance)) rawEntry {
		rec := artifactRecord(artifactTree(t, artifactFiles()))
		rec.Format = artifact.Format
		edit(rec)

		data, err := sonic.ConfigStd.Marshal(rec)
		require.NoError(t, err)

		return rawEntry{name: artifact.ProvenanceName, data: string(data)}
	}

	tests := []struct {
		name    string
		msg     string
		entries []rawEntry
	}{
		{
			"parent", `the entry "plugin/../../etc/passwd" is not inside the plugin`,
			with(rawEntry{name: "plugin/../../etc/passwd", data: "x"}),
		},
		{"escape", `the entry "../evil" is not inside the plugin`, with(rawEntry{name: "../evil", data: "x"})},
		{
			"absolute", `the entry "/etc/cron.d/evil" is not inside the plugin`,
			with(rawEntry{name: "/etc/cron.d/evil", data: "x"}),
		},
		{"outside", `the entry "evil.txt" is not inside the plugin`, with(rawEntry{name: "evil.txt", data: "x"})},
		{"unclean", `the entry "plugin/./x" is not inside the plugin`, with(rawEntry{name: "plugin/./x", data: "x"})},
		{"the directory", `the entry "plugin/" is not inside the plugin`, with(header(tar.TypeDir, "plugin/", ""))},
		{"symlink", "plugin/link is not a regular file", with(header(tar.TypeSymlink, "plugin/link", "/etc/passwd"))},
		{
			"hard link",
			"plugin/hard is not a regular file",
			with(header(tar.TypeLink, "plugin/hard", "plugin/main.lua")),
		},
		{"directory", "plugin/dir is not a regular file", with(header(tar.TypeDir, "plugin/dir", ""))},
		{"device", "plugin/null is not a regular file", with(header(tar.TypeChar, "plugin/null", ""))},
		{"twice", `"main.lua" added twice`, with(rawEntry{name: "plugin/main.lua", data: "x"})},
		{
			"file and directory", `"main.lua" is both a file and a directory`,
			with(rawEntry{name: "plugin/main.lua/x", data: "x"}),
		},
		{"no provenance", "no provenance.json", files},
		{"two records", "two provenance.json entries", with(provenance)},
		{
			"record not a file", "provenance.json is not a regular file",
			append([]rawEntry{header(tar.TypeSymlink, "provenance.json", "x")}, files...),
		},
		{"record too large", "the provenance record is over 4194304 bytes", append([]rawEntry{{
			name: artifact.ProvenanceName, data: strings.Repeat(" ", artifact.MaxProvenanceSize+1),
		}}, files...)},
		{
			"record not JSON",
			"provenance.json: ",
			append([]rawEntry{{name: artifact.ProvenanceName, data: "{"}}, files...),
		},
		{
			"another format", "format 2, but this plg reads format 1",
			append([]rawEntry{record(func(rec *adminjson.Provenance) { rec.Format = 2 })}, files...),
		},
		{"another digest", "its files' digest is sha256:", append([]rawEntry{
			record(func(rec *adminjson.Provenance) { rec.Digest = "sha256:00" }),
		}, files...)},
		{"edited file", "but its provenance records sha256:", append([]rawEntry{provenance}, files[0], files[1],
			rawEntry{name: "plugin/plugin.yaml", data: artifactFiles()["plugin.yaml"] + "version: \"2\"\n"})},
		{"no plugin.yaml", "no plugin/plugin.yaml", append([]rawEntry{provenanceEntry(t, map[string]string{
			"main.lua": artifactFiles()["main.lua"], "lib/tiers.lua": artifactFiles()["lib/tiers.lua"],
		})}, files[:2]...)},
	}

	for _, tc := range tests {
		art, err := openArtifact(rawArchive(t, 0, false, tc.entries...))
		require.ErrorIs(t, err, plg.ErrInvalidArtifact, tc.name)
		require.NotErrorIs(t, err, plg.ErrPluginTooLarge, tc.name)
		require.ErrorContains(t, err, tc.msg, tc.name)
		assert.Nil(t, art, tc.name)
	}
}
