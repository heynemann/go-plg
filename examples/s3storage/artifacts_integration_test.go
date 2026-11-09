//go:build integration

package s3storage_test

import (
	"bytes"
	"crypto/rand"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/examples/s3storage"
	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/artifact"
	"github.com/heynemann/go-plg/internal/memfs"
)

// artifactCommit is the commit the artifacts these tests pack record.
const artifactCommit = "0123456789abcdef0123456789abcdef01234567"

// packed is an artifact `plg pack` writes and the digest its provenance records.
type packed struct {
	data   string
	digest string
}

// packArtifact returns the artifact `plg pack` writes of files (each name's contents), with plg pack's own writer.
func packArtifact(t *testing.T, files map[string]string) packed {
	t.Helper()

	var tree memfs.Builder

	for name, data := range files {
		require.NoError(t, tree.Add(name, []byte(data)))
	}

	built, err := tree.Build()
	require.NoError(t, err)

	digest := artifact.Digest(built)

	var out bytes.Buffer

	require.NoError(t, artifact.Write(&out, built, &adminjson.Provenance{
		Contract: adminjson.Module{Path: "github.com/heynemann/go-plg/examples", Version: "v1.0.0"},
		Digest:   digest, Commit: artifactCommit, Plg: "(devel)",
		Report: sonic.NoCopyRawMessage(`{"version":1,"valid":true,"plugins":[]}`),
	}))

	return packed{data: out.String(), digest: digest}
}

// luaRule returns the files of a Lua rule whose main.lua is main.
func luaRule(main string) map[string]string {
	return map[string]string{"plugin.yaml": luaManifest, "main.lua": main}
}

// The artifact layout lists the .plg objects directly under the prefix, each described by its plugin.yaml and
// versioned by it or by its digest, and ignores every other object without reading it; Open returns the artifact,
// with its provenance.
func TestArtifactStorageListsArtifacts(t *testing.T) {
	t.Parallel()

	client := startMinIO(t)
	bucket := newBucket(t, client)

	vip := packArtifact(t, map[string]string{
		"plugin.yaml": luaManifest, "main.lua": "return 1", "lib/tiers.lua": "return {}",
	})
	silver := packArtifact(t, map[string]string{"plugin.yaml": luaManifest + "version: \"1.4.0\"\n", "main.lua": ""})
	put(t, client, bucket, "rules/vip.plg", vip.data)
	put(t, client, bucket, "rules/silver.plg", silver.data)

	for _, key := range []string{
		"rules/notes.txt", "rules/vip.plg.new", "rules/old/gold.plg", "rules/.hidden.plg", "rules/.plg", "gold.plg",
	} {
		put(t, client, bucket, key, "not an artifact: List fails if it reads this")
	}

	store := s3storage.New(client, bucket, s3storage.WithPrefix("rules"), s3storage.WithArtifacts())
	assert.Equal(t, "s3://"+bucket+"/rules/", store.Name())

	descs, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plg.Descriptor{
		{
			Key: "silver", Implements: ruleImplements, Language: plg.LanguageLua, Entrypoint: "main.lua",
			Version: "1.4.0",
		},
		{
			Key: "vip", Implements: ruleImplements, Language: plg.LanguageLua, Entrypoint: "main.lua",
			Version: vip.digest,
		},
	}, descs, "sorted by key; the plugin.yaml's version wins over the digest")

	files, err := store.Open(t.Context(), &descs[1])
	require.NoError(t, err)

	art, ok := files.(*plg.Artifact)
	require.True(t, ok, "Open returns the artifact itself, %T", files)
	assert.Equal(t, vip.digest, art.Provenance().Digest)
	assert.Equal(t, artifactCommit, art.Provenance().Commit)
	require.NoError(t, fstest.TestFS(files, "plugin.yaml", "main.lua", "lib/tiers.lua"))

	main, err := fs.ReadFile(files, "main.lua")
	require.NoError(t, err)
	assert.Equal(t, "return 1", string(main))
}

// List downloads an artifact only when its ETag is new to it: a poll of unchanged artifacts downloads nothing.
func TestArtifactStorageDownloadsAnArtifactOnlyWhenItChanges(t *testing.T) {
	t.Parallel()

	client := startMinIO(t)
	bucket := newBucket(t, client)
	first := packArtifact(t, luaRule("return 1"))
	second := packArtifact(t, luaRule("return 2"))
	put(t, client, bucket, "p.plg", first.data)
	put(t, client, bucket, "q.plg", first.data)

	counting := &countingClient{Client: client}
	store := s3storage.New(counting, bucket, s3storage.WithArtifacts())

	versions := func() []string {
		t.Helper()

		descs, err := store.List(t.Context())
		require.NoError(t, err)

		got := make([]string, len(descs))
		for i, desc := range descs {
			got[i] = desc.Key + "@" + desc.Version
		}

		return got
	}

	assert.Equal(t, []string{"p@" + first.digest, "q@" + first.digest}, versions())
	assert.Equal(t, int64(2), counting.gets.Load(), "both artifacts downloaded")

	versions()
	assert.Equal(t, int64(2), counting.gets.Load(), "a poll with nothing changed downloads nothing")

	put(t, client, bucket, "p.plg", second.data)
	assert.Equal(t, []string{"p@" + second.digest, "q@" + first.digest}, versions())
	assert.Equal(t, int64(3), counting.gets.Load(), "only the changed artifact")

	remove(t, client, bucket, "q.plg")
	assert.Equal(t, []string{"p@" + second.digest}, versions())
	put(t, client, bucket, "q.plg", first.data)
	versions()
	assert.Equal(t, int64(4), counting.gets.Load(), "an artifact that comes back is read again")

	_, err := store.Open(t.Context(), &plg.Descriptor{Key: "p"})
	require.NoError(t, err)
	assert.Equal(t, int64(5), counting.gets.Load(), "Open downloads the artifact")
}

// An artifact List has never described that cannot be opened, or whose plugin.yaml is malformed, fails List, which
// names it; Open fails on it the same way, but for the plugin.yaml, which only List reads.
func TestArtifactStorageRefusesArtifactsItCannotOpen(t *testing.T) {
	t.Parallel()

	client := startMinIO(t)
	valid := packArtifact(t, luaRule("")).data
	tooMany := luaRule("")
	tooMany["big.bin"] = strings.Repeat("\x00", plg.MaxPluginFileSize+1)
	overLimits := packArtifact(t, tooMany).data
	malformed := packArtifact(t, map[string]string{"plugin.yaml": "language: lua\nentrypoint: main.lua\n"}).data

	tests := map[string]struct {
		is    error
		data  string
		opens bool
	}{
		"not an artifact":         {data: "not an artifact", is: plg.ErrInvalidArtifact},
		"cut short":               {data: valid[:len(valid)/2], is: plg.ErrInvalidArtifact},
		"a file over plg's limit": {data: overLimits, is: plg.ErrPluginTooLarge},
		"malformed plugin.yaml":   {data: malformed, is: plg.ErrInvalidManifest, opens: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			bucket := newBucket(t, client)
			put(t, client, bucket, "a.plg", valid)
			put(t, client, bucket, "p.plg", tc.data)

			store := s3storage.New(client, bucket, s3storage.WithArtifacts())

			_, err := store.List(t.Context())
			require.ErrorIs(t, err, tc.is)
			require.ErrorContains(t, err, "artifact p.plg")

			_, err = store.Open(t.Context(), &plg.Descriptor{Key: "p"})
			if tc.opens {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, tc.is)
			require.ErrorContains(t, err, "artifact p.plg")
		})
	}
}

// An artifact over the storage's maximum size is refused before it is read: List as the listing states its size,
// before downloading it, and Open as its download does; a download that does not state its size is read up to the
// limit only.
func TestArtifactStorageSizeLimit(t *testing.T) {
	t.Parallel()

	client := startMinIO(t)
	bucket := newBucket(t, client)
	data := packArtifact(t, luaRule("return 1")).data
	size := int64(len(data))
	put(t, client, bucket, "p.plg", data)

	atLimit := s3storage.New(client, bucket, s3storage.WithArtifacts(), s3storage.WithMaxPluginSize(size))
	_, err := atLimit.List(t.Context())
	require.NoError(t, err, "exactly at the limit")

	_, err = atLimit.Open(t.Context(), &plg.Descriptor{Key: "p"})
	require.NoError(t, err, "exactly at the limit")

	counting := &countingClient{Client: client}
	over := s3storage.New(counting, bucket, s3storage.WithArtifacts(), s3storage.WithMaxPluginSize(size-1))

	_, err = over.List(t.Context())
	require.ErrorContains(t, err, "artifact p.plg: too large", "one byte over the limit")
	assert.Zero(t, counting.gets.Load(), "refused as listed, before downloading")

	_, err = over.Open(t.Context(), &plg.Descriptor{Key: "p"})
	require.ErrorContains(t, err, "artifact p.plg")
	require.ErrorContains(t, err, "too large")
	assert.Zero(t, counting.read.Load(), "refused as its download states, before reading")

	unstated := &countingClient{Client: client, hideLength: true}
	_, err = s3storage.New(unstated, bucket, s3storage.WithArtifacts(), s3storage.WithMaxPluginSize(size-1)).
		Open(t.Context(), &plg.Descriptor{Key: "p"})
	require.ErrorContains(t, err, "artifact p.plg")
	require.ErrorContains(t, err, "too large")
	assert.LessOrEqual(t, unstated.read.Load(), size, "read up to the limit and one byte")
}

// An artifact republished in a form that cannot be opened keeps the descriptor of its last good version under a
// version of its own, so Open fails for that plugin alone, and a poll downloads it no more; the next good version
// is described by its own plugin.yaml.
func TestArtifactStorageKeepsDescribingARepublishedArtifact(t *testing.T) {
	t.Parallel()

	client := startMinIO(t)
	bucket := newBucket(t, client)

	good := packArtifact(t, map[string]string{
		"plugin.yaml": luaManifest + "owner:\n  team: {name: loyalty}\n  contact: {oncall: loyalty-oncall}\n",
		"main.lua":    "return 1",
	})
	better := packArtifact(t, luaRule("return 2"))

	noise := make([]byte, 8<<10)
	_, err := rand.Read(noise)
	require.NoError(t, err)

	huge := packArtifact(t, map[string]string{"plugin.yaml": luaManifest, "main.lua": "", "noise": string(noise)})
	put(t, client, bucket, "p.plg", good.data)

	counting := &countingClient{Client: client}
	store := s3storage.New(counting, bucket, s3storage.WithArtifacts(),
		s3storage.WithMaxPluginSize(int64(len(good.data)+len(better.data))))

	list := func() plg.Descriptor {
		t.Helper()

		descs, err := store.List(t.Context())
		require.NoError(t, err)
		require.Len(t, descs, 1)

		return descs[0]
	}

	lastGood := list()
	require.Equal(t, good.digest, lastGood.Version)
	require.NotNil(t, lastGood.Owner)

	versions := map[string]bool{good.digest: true}

	for _, step := range []struct {
		is   error
		name string
		data string
		msg  string
	}{
		{name: "not an artifact", data: "not an artifact", is: plg.ErrInvalidArtifact, msg: "invalid artifact"},
		{name: "cut short", data: good.data[:len(good.data)/2], is: plg.ErrInvalidArtifact, msg: "invalid artifact"},
		{name: "over the maximum size", data: huge.data, msg: "too large"},
	} {
		put(t, client, bucket, "p.plg", step.data)

		got := list()
		assert.Regexp(t, isListingVersion, got.Version, step.name)
		assert.NotContains(t, versions, got.Version, "%s: a version of its own", step.name)

		versions[got.Version] = true
		want := lastGood
		want.Version = got.Version
		assert.Equal(t, want, got, "%s: described by the last good plugin.yaml", step.name)

		_, err := store.Open(t.Context(), &got)
		require.ErrorContains(t, err, "artifact p.plg", step.name)
		require.ErrorContains(t, err, step.msg, step.name)

		if step.is != nil {
			require.ErrorIs(t, err, step.is, step.name)
		}

		gets := counting.gets.Load()

		assert.Equal(t, got, list(), "%s: the same on the next poll", step.name)
		assert.Equal(t, gets, counting.gets.Load(), "%s: which downloads nothing", step.name)
	}

	put(t, client, bucket, "p.plg", better.data)
	assert.Equal(t, plg.Descriptor{
		Key: "p", Implements: ruleImplements, Language: plg.LanguageLua, Entrypoint: "main.lua", Version: better.digest,
	}, list(), "the next good version is described by its own plugin.yaml")
}

// A failed download fails List, naming the artifact, even for an artifact it described before: the next poll
// tries again, and no plugin is rejected for it. Open fails on it too, and on an artifact that is not there.
func TestArtifactStorageDownloadFailures(t *testing.T) {
	t.Parallel()

	client := startMinIO(t)
	bucket := newBucket(t, client)
	first := packArtifact(t, luaRule("return 1"))
	second := packArtifact(t, luaRule("return 2"))
	put(t, client, bucket, "p.plg", first.data)

	counting := &countingClient{Client: client}
	store := s3storage.New(counting, bucket, s3storage.WithArtifacts())

	counting.fail.Store(true)

	_, err := store.List(t.Context())
	require.ErrorIs(t, err, errInjected)
	require.ErrorContains(t, err, "artifact p.plg")

	counting.fail.Store(false)

	_, err = store.List(t.Context())
	require.NoError(t, err)

	put(t, client, bucket, "p.plg", second.data)
	counting.fail.Store(true)

	_, err = store.List(t.Context())
	require.ErrorIs(t, err, errInjected, "a described artifact whose new version cannot be downloaded")

	_, err = store.Open(t.Context(), &plg.Descriptor{Key: "p"})
	require.ErrorIs(t, err, errInjected)

	counting.fail.Store(false)

	descs, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, second.digest, descs[0].Version, "the next poll reads it")

	_, err = store.Open(t.Context(), &plg.Descriptor{Key: "none"})

	var missing *types.NoSuchKey

	require.ErrorAs(t, err, &missing)
	require.ErrorContains(t, err, "artifact none.plg")

	_, err = s3storage.New(client, "no-such-bucket", s3storage.WithArtifacts()).List(t.Context())
	require.ErrorContains(t, err, "s3://no-such-bucket/")
}
