//go:build integration

package s3storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/minio"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/examples/internal/pricingtest"
	"github.com/heynemann/go-plg/examples/plugins"
	"github.com/heynemann/go-plg/examples/pricing"
	"github.com/heynemann/go-plg/examples/s3storage"
)

// minioImage is MinIO built from its release sources (the project no longer publishes images of its own).
const minioImage = "alpine/minio:RELEASE.2025-10-15T17-29-55Z"

// client reaches the MinIO server TestMain starts for the package's tests; each test works in a bucket of its own.
var client *s3.Client //nolint:gochecknoglobals // the server TestMain starts, shared by every test of the package

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(withMinIO{m})
}

// withMinIO runs the tests against a MinIO server in a container.
type withMinIO struct{ m *testing.M }

func (w withMinIO) Run() int {
	ctx := context.Background()

	publish, err := pricingtest.PublishOnFreePort(ctx, "9000/tcp")
	if err != nil {
		fmt.Fprintln(os.Stderr, "publishing MinIO:", err)

		return 1
	}

	// The image runs MinIO as an unprivileged user that cannot write its /data volume.
	ctr, err := minio.Run(ctx, minioImage, testcontainers.WithCmd("server", "/tmp/data"), publish)
	defer func() {
		termErr := testcontainers.TerminateContainer(ctr)
		if termErr != nil {
			fmt.Fprintln(os.Stderr, "terminating MinIO:", termErr)
		}
	}()

	if err != nil {
		fmt.Fprintln(os.Stderr, "starting MinIO:", err)

		return 1
	}

	endpoint, err := ctr.ConnectionString(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "MinIO's endpoint:", err)

		return 1
	}

	client = s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String("http://" + endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(ctr.Username, ctr.Password, ""),
	})

	return w.m.Run()
}

// newBucket creates an empty bucket for one test.
func newBucket(t *testing.T) string {
	t.Helper()

	bucket := "plg-" + strings.ToLower(rand.Text())

	_, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	return bucket
}

func put(t *testing.T, bucket, key, data string) {
	t.Helper()

	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(data),
	})
	require.NoError(t, err)
}

func remove(t *testing.T, bucket, key string) {
	t.Helper()

	_, err := client.DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)
}

// upload puts every file of fsys into bucket under prefix.
func upload(t *testing.T, bucket, prefix string, fsys fs.FS) {
	t.Helper()

	err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		data, readErr := fs.ReadFile(fsys, name)
		require.NoError(t, readErr)
		put(t, bucket, prefix+name, string(data))

		return nil
	})
	require.NoError(t, err)
}

const isListingVersion = `^s3:[0-9a-f]{64}$`

func TestStorageListsPluginDirectories(t *testing.T) {
	t.Parallel()

	bucket := newBucket(t)
	upload(t, bucket, "plugins/", plugins.FS)
	put(t, bucket, "plugins/README.md", "objects directly under the prefix are not plugins")
	put(t, bucket, "plugins/.git/config", "neither are dot directories")
	put(t, bucket, "plugins/empty/", "") // a directory marker, with no plugin.json under it
	put(t, bucket, "plugins/bulk/docs/", "")
	put(t, bucket, "elsewhere/x/plugin.json", "{malformed: outside the prefix")

	store := s3storage.New(client, bucket, s3storage.WithPrefix("plugins"))
	assert.Equal(t, "s3://"+bucket+"/plugins/", store.Name())

	descs, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, descs, 3)

	for _, desc := range descs {
		assert.Regexp(t, isListingVersion, desc.Version, "%s has no version in its manifest", desc.Key)
	}

	assert.Equal(t, []plg.Descriptor{
		{Key: "bulk", Language: plg.LanguageGo, Entrypoint: ".", Version: descs[0].Version},
		{Key: "coupon", Language: plg.LanguageTypeScript, Entrypoint: "rule.ts", Version: descs[1].Version},
		{Key: "loyalty", Language: plg.LanguageLua, Entrypoint: "rule.lua", Version: descs[2].Version},
	}, descs, "sorted by key")

	files, err := store.Open(t.Context(), descs[2])
	require.NoError(t, err)
	require.NoError(t, fstest.TestFS(files, "plugin.json", "rule.lua", "tiers.lua"), "Open serves the plugin's files")

	for _, name := range []string{"plugin.json", "rule.lua", "tiers.lua"} {
		want, readErr := fs.ReadFile(plugins.FS, "loyalty/"+name)
		require.NoError(t, readErr)

		got, readErr := fs.ReadFile(files, name)
		require.NoError(t, readErr)
		assert.Equal(t, string(want), string(got), name)
	}

	bulk, err := store.Open(t.Context(), descs[0])
	require.NoError(t, err)

	_, err = fs.Stat(bulk, "docs")
	assert.ErrorIs(t, err, fs.ErrNotExist, "directory markers are not files")
}

func TestStorageAtTheBucketRoot(t *testing.T) {
	t.Parallel()

	bucket := newBucket(t)
	put(t, bucket, "p/plugin.json", `{"language":"lua","entrypoint":"main.lua","version":"7"}`)
	put(t, bucket, "p/main.lua", "")

	store := s3storage.New(client, bucket)
	assert.Equal(t, "s3://"+bucket+"/", store.Name())

	descs, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Equal(
		t,
		[]plg.Descriptor{{Key: "p", Language: plg.LanguageLua, Entrypoint: "main.lua", Version: "7"}},
		descs,
		"a manifest's version wins",
	)
}

func TestStorageVersionsFollowTheObjects(t *testing.T) {
	t.Parallel()

	bucket := newBucket(t)
	put(t, bucket, "p/plugin.json", `{"language":"lua","entrypoint":"main.lua"}`)
	put(t, bucket, "p/main.lua", "return 1")
	put(t, bucket, "q/plugin.json", `{"language":"lua","entrypoint":"main.lua"}`)
	put(t, bucket, "q/main.lua", "return 1")

	store := s3storage.New(client, bucket)
	versions := func() (string, string) {
		t.Helper()

		descs, err := store.List(t.Context())
		require.NoError(t, err)
		require.Len(t, descs, 2)

		return descs[0].Version, descs[1].Version
	}

	first, other := versions()
	assert.Regexp(t, isListingVersion, first)
	assert.Equal(t, first, other, "the same objects under another key have the same version")

	again, _ := versions()
	assert.Equal(t, first, again, "stable while nothing changes")

	// Each step changes p; a version follows the objects, not their history, so undoing a step restores the
	// version from before it.
	steps := []struct {
		change func()
		name   string
		same   string // the earlier step whose version the change restores, if any
	}{
		{name: "rewritten", change: func() { put(t, bucket, "p/main.lua", "return 2") }},
		{name: "added", change: func() { put(t, bucket, "p/lib.lua", "") }},
		{name: "removed", change: func() { remove(t, bucket, "p/lib.lua") }, same: "rewritten"},
		{name: "renamed", change: func() {
			remove(t, bucket, "p/main.lua")
			put(t, bucket, "p/other.lua", "return 2")
		}},
		{name: "renamed back", change: func() {
			remove(t, bucket, "p/other.lua")
			put(t, bucket, "p/main.lua", "return 2")
		}, same: "rewritten"},
	}

	byStep := map[string]string{}
	previous := first

	for _, step := range steps {
		step.change()

		version, unchanged := versions()
		assert.Equal(t, other, unchanged, "%s: the other plugin keeps its version", step.name)
		assert.NotEqual(t, previous, version, "%s: a new version", step.name)

		if step.same != "" {
			assert.Equal(t, byStep[step.same], version, "%s: the objects of %s again", step.name, step.same)
		}

		byStep[step.name] = version
		previous = version
	}

	assert.NotEqual(t, byStep["added"], byStep["renamed"])
	assert.NotEqual(t, first, byStep["renamed"])
}

// countingClient counts the objects the storage downloads from a real S3 client.
type countingClient struct {
	*s3.Client

	gets atomic.Int64
}

func (c *countingClient) GetObject(
	ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options),
) (*s3.GetObjectOutput, error) {
	c.gets.Add(1)

	return c.Client.GetObject(ctx, in, opts...) //nolint:wrapcheck // passes the real client's answer through
}

func TestStorageDownloadsAManifestOnlyWhenItChanges(t *testing.T) {
	t.Parallel()

	bucket := newBucket(t)
	put(t, bucket, "p/plugin.json", `{"language":"lua","entrypoint":"main.lua"}`)
	put(t, bucket, "p/main.lua", "")
	put(t, bucket, "q/plugin.json", `{"language":"lua","entrypoint":"main.lua"}`)

	counting := &countingClient{Client: client}
	store := s3storage.New(counting, bucket)

	list := func() []plg.Descriptor {
		t.Helper()

		descs, err := store.List(t.Context())
		require.NoError(t, err)

		return descs
	}

	list()
	assert.Equal(t, int64(2), counting.gets.Load(), "both manifests downloaded")

	list()
	assert.Equal(t, int64(2), counting.gets.Load(), "a poll with nothing changed downloads nothing")

	put(t, bucket, "p/plugin.json", `{"language":"lua","entrypoint":"init.lua"}`)

	descs := list()

	assert.Equal(t, int64(3), counting.gets.Load(), "only the changed manifest")
	assert.Equal(t, "init.lua", descs[0].Entrypoint, "the new manifest is read")

	remove(t, bucket, "q/plugin.json")
	list()
	put(t, bucket, "q/plugin.json", `{"language":"lua","entrypoint":"main.lua"}`)
	list()
	assert.Equal(t, int64(4), counting.gets.Load(), "a plugin that comes back is read again")
}

func TestStorageRejectsBadManifests(t *testing.T) {
	t.Parallel()

	tests := map[string]map[string]string{
		"no manifest": {"p/main.lua": ""},
		"malformed":   {"p/plugin.json": `{"language":"lua"`},
		"unknown key": {"p/plugin.json": `{"language":"lua","entrypoint":"main.lua","memory_limit":1}`},
		"too large": {
			"p/plugin.json": `{"language":"lua","entrypoint":"main.lua"}` + strings.Repeat(" ", 64<<10),
		},
		"one bad of many": {"a/plugin.json": `{"language":"lua","entrypoint":"main.lua"}`, "p/plugin.json": `[]`},
	}

	for name, objects := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			bucket := newBucket(t)
			for key, data := range objects {
				put(t, bucket, key, data)
			}

			_, err := s3storage.New(client, bucket).List(t.Context())
			require.ErrorIs(t, err, plg.ErrInvalidManifest)
			assert.ErrorContains(t, err, "plugin directory p")
		})
	}
}

func TestStorageManifestAtTheSizeLimit(t *testing.T) {
	t.Parallel()

	bucket := newBucket(t)
	manifest := `{"language":"lua","entrypoint":"main.lua"}`
	put(t, bucket, "p/plugin.json", manifest+strings.Repeat(" ", 64<<10-len(manifest)))

	descs, err := s3storage.New(client, bucket).List(t.Context())
	require.NoError(t, err)
	assert.Len(t, descs, 1)
}

func TestStorageOpenFailures(t *testing.T) {
	t.Parallel()

	bucket := newBucket(t)
	manifest := `{"language":"lua","entrypoint":"main.lua"}`
	put(t, bucket, "big/plugin.json", manifest)
	put(t, bucket, "big/main.lua", strings.Repeat("-", 100-len(manifest))) // 100 bytes together, main.lua first
	put(t, bucket, "odd/plugin.json", `{"language":"lua","entrypoint":"main.lua"}`)
	put(t, bucket, "odd/a/main.lua", "")

	store := s3storage.New(client, bucket, s3storage.WithMaxPluginSize(100))

	files, err := store.Open(t.Context(), plg.Descriptor{Key: "big"})
	require.NoError(t, err, "exactly at the limit")
	require.NoError(t, fstest.TestFS(files, "plugin.json", "main.lua"))

	_, err = s3storage.New(client, bucket, s3storage.WithMaxPluginSize(99)).
		Open(t.Context(), plg.Descriptor{Key: "big"})
	require.ErrorContains(t, err, "too large", "one byte over the limit")

	_, err = s3storage.New(client, bucket, s3storage.WithMaxPluginSize(int64(101-len(manifest)))).
		Open(t.Context(), plg.Descriptor{Key: "big"})
	require.ErrorContains(t, err, "too large", "main.lua fits, plugin.json no longer does")

	_, err = s3storage.New(&oddKeysClient{Client: client}, bucket).Open(t.Context(), plg.Descriptor{Key: "odd"})
	require.ErrorContains(t, err, `"a//main.lua"`, "an object key that is not a path")

	_, err = store.Open(t.Context(), plg.Descriptor{Key: "none"})
	require.ErrorIs(t, err, fs.ErrNotExist)

	for _, key := range []string{"", ".", "..", "big/main.lua"} {
		_, err = store.Open(t.Context(), plg.Descriptor{Key: key})
		require.ErrorIs(t, err, plg.ErrInvalidDescriptor, "key %q", key)
	}

	_, err = s3storage.New(client, "no-such-bucket").List(t.Context())
	require.ErrorContains(t, err, "s3://no-such-bucket/")

	_, err = s3storage.New(client, "no-such-bucket").Open(t.Context(), plg.Descriptor{Key: "big"})
	require.ErrorContains(t, err, "s3://no-such-bucket/big/")
}

// oddKeysClient lists the keys of a real client with "/a/" turned into "/a//": AWS S3 accepts such keys (MinIO
// refuses them), and they are not paths an fs.FS may hold.
type oddKeysClient struct{ *s3.Client }

func (c *oddKeysClient) ListObjectsV2(
	ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options),
) (*s3.ListObjectsV2Output, error) {
	out, err := c.Client.ListObjectsV2(ctx, in, opts...)
	if err != nil {
		return nil, err //nolint:wrapcheck // passes the real client's answer through
	}

	for i, obj := range out.Contents {
		out.Contents[i].Key = aws.String(strings.Replace(aws.ToString(obj.Key), "/a/", "/a//", 1))
	}

	return out, nil
}

func TestStorageReadsEveryPageOfAListing(t *testing.T) {
	t.Parallel()

	bucket := newBucket(t)
	upload(t, bucket, "", plugins.FS) // 9 objects

	store := s3storage.New(client, bucket)
	s3storage.SetPageSize(store, 2)

	descs, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, descs, 3)

	files, err := store.Open(t.Context(), plg.Descriptor{Key: "coupon"})
	require.NoError(t, err)
	require.NoError(t, fstest.TestFS(files, "plugin.json", "rule.ts", "coupons.ts"))
}

func TestStorageDownloadFailures(t *testing.T) {
	t.Parallel()

	bucket := newBucket(t)
	put(t, bucket, "p/plugin.json", `{"language":"lua","entrypoint":"main.lua"}`)

	failing := &failingClient{Client: client}
	store := s3storage.New(failing, bucket)

	_, err := store.List(t.Context())
	require.ErrorIs(t, err, errInjected)
	require.ErrorContains(t, err, "plugin directory p")

	_, err = store.Open(t.Context(), plg.Descriptor{Key: "p"})
	require.ErrorIs(t, err, errInjected)
}

var errInjected = errors.New("injected download failure")

// failingClient lists like a real client and fails every download.
type failingClient struct{ *s3.Client }

func (*failingClient) GetObject(
	context.Context,
	*s3.GetObjectInput,
	...func(*s3.Options),
) (*s3.GetObjectOutput, error) {
	return nil, errInjected
}

// The storage serves plg: the example rules in Go, Lua and TypeScript load from S3, and a reload follows the
// bucket.
func TestStorageServesThePricingRules(t *testing.T) {
	t.Parallel()

	bucket := newBucket(t)
	upload(t, bucket, "rules/", plugins.FS)

	container := pricingtest.Register(t, s3storage.New(client, bucket, s3storage.WithPrefix("rules/")))
	assert.Equal(t, pricingtest.Discounts(), pricingtest.Quote(t, container))

	coupons, err := fs.ReadFile(plugins.FS, "coupon/coupons.ts")
	require.NoError(t, err)
	put(t, bucket, "rules/coupon/coupons.ts", string(bytes.Replace(coupons, []byte("500"), []byte("700"), 1)))

	for _, key := range []string{"plugin.json", "rule.lua", "tiers.lua"} {
		remove(t, bucket, "rules/loyalty/"+key)
	}

	require.NoError(t, container.Reload(t.Context()))

	want := pricingtest.Discounts()
	want["pricing/coupon"] = pricing.Discount{Cents: 700, Reason: "coupon WELCOME5"}
	delete(want, "pricing/loyalty")
	assert.Equal(t, want, pricingtest.Quote(t, container), "the changed rule reloaded, the removed one gone")
}
