// Package s3storage is a [plg.Storage] over an S3 bucket (or any S3-compatible store, such as MinIO), built on
// aws-sdk-go-v2. It keeps plg's built-in layout under a key prefix: one "directory" per plugin, named after the
// plugin's key, holding the plugin's files and a plugin.yaml manifest read by [plg.ParseManifest]:
//
//	plugins/bulk/plugin.yaml      {language: go, entrypoint: .}
//	plugins/bulk/bulk.go
//	plugins/loyalty/plugin.yaml   {language: lua, entrypoint: rule.lua}
//	plugins/loyalty/rule.lua
//
// Objects directly under the prefix and "directories" whose names start with a dot are not plugins and are
// ignored. A manifest without a version gets one from the plugin's object listing (each object's path, size and
// ETag), so a poll ([plg.WithPollInterval]) costs one listing of the prefix and detects changes without
// downloading any plugin file; each manifest is downloaded again only when its ETag changes. Open downloads a
// plugin's objects, at most [DefaultMaxPluginSize] bytes together unless [WithMaxPluginSize] says otherwise. List and
// Open stop paging through a listing, failing with [plg.ErrPluginTooLarge], as soon as it brings a plugin directory
// over [plg.MaxPluginEntries] objects or [plg.MaxPluginPathBytes] bytes of object paths together.
package s3storage

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing/fstest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/puzpuzpuz/xsync/v4"

	"github.com/heynemann/go-plg"
)

// Client is the part of the S3 API the storage uses; *s3.Client implements it.
type Client interface {
	s3.ListObjectsV2APIClient
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

const (
	// DefaultMaxPluginSize bounds the bytes of one plugin's objects that Open downloads.
	DefaultMaxPluginSize = 32 << 20
	manifestName         = "plugin.yaml"
	// listPageSize is S3's largest page of a listing.
	listPageSize = 1000
	// readOnly is the mode of the files Open returns.
	readOnly fs.FileMode = 0o444
)

// Storage is a [plg.Storage] over the plugins under a prefix of an S3 bucket. It is safe for concurrent use.
type Storage struct {
	client Client
	// manifests caches each parsed plugin.yaml by its object key, with the ETag it was parsed at.
	manifests     *xsync.Map[string, manifest]
	bucket        string
	prefix        string
	maxPluginSize int64
	pageSize      int32
}

// manifest is a parsed plugin.yaml and the ETag of the object it was read from.
type manifest struct {
	etag string
	desc plg.Descriptor
}

var _ plg.Storage = (*Storage)(nil)

// Option configures a [Storage].
type Option func(*Storage)

// WithPrefix sets the key prefix the plugin directories are under, such as "plugins/" (a missing final slash is
// added); the default is the bucket's root.
func WithPrefix(prefix string) Option {
	return func(s *Storage) {
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}

		s.prefix = prefix
	}
}

// WithMaxPluginSize sets how many bytes of a plugin's objects Open downloads at most; a larger plugin fails to
// open. The default is [DefaultMaxPluginSize]. Register refuses more than [plg.MaxPluginSize] bytes (and an object
// over [plg.MaxPluginFileSize]) whatever this is, so a larger n only downloads plugins that cannot load.
func WithMaxPluginSize(n int64) Option {
	return func(s *Storage) {
		s.maxPluginSize = n
	}
}

// New returns a storage over the plugins in bucket.
func New(client Client, bucket string, opts ...Option) *Storage {
	store := &Storage{
		client:        client,
		manifests:     xsync.NewMap[string, manifest](),
		bucket:        bucket,
		maxPluginSize: DefaultMaxPluginSize,
		pageSize:      listPageSize,
	}

	for _, opt := range opts {
		opt(store)
	}

	return store
}

// Name returns "s3://<bucket>/<prefix>".
func (s *Storage) Name() string {
	return "s3://" + s.bucket + "/" + s.prefix
}

// object is one object of a plugin directory: its path inside the directory, size and ETag.
type object struct {
	path string
	etag string
	size int64
}

// List lists the prefix and returns its plugins, sorted by key. It fails with [plg.ErrInvalidManifest], naming
// the plugin, if a plugin directory has no plugin.yaml or a malformed one, and with [plg.ErrPluginTooLarge] as soon
// as the listing brings a plugin directory over [plg.MaxPluginEntries] objects or [plg.MaxPluginPathBytes] bytes of
// object paths, which Register would refuse to copy (see [Storage.Open]).
func (s *Storage) List(ctx context.Context) ([]plg.Descriptor, error) {
	dirs, err := s.listDirs(ctx)
	if err != nil {
		return nil, err
	}

	descs := make([]plg.Descriptor, 0, len(dirs))

	for dir, listed := range dirs {
		desc, err := s.describe(ctx, dir, listed.objects)
		if err != nil {
			return nil, err
		}

		descs = append(descs, desc)
	}

	// Forget the manifests of plugins no longer listed.
	s.manifests.Range(func(key string, _ manifest) bool {
		dir, _ := strings.CutSuffix(strings.TrimPrefix(key, s.prefix), "/"+manifestName)
		if _, ok := dirs[dir]; !ok {
			s.manifests.Delete(key)
		}

		return true
	})

	slices.SortFunc(descs, func(a, b plg.Descriptor) int { return cmp.Compare(a.Key, b.Key) })

	return descs, nil
}

// listingVersion returns a version for a plugin directory from its listing: "s3:" and a SHA-256 over every
// object's path, size and ETag, in path order. It changes whenever an object is added, removed, renamed or
// rewritten.
func listingVersion(objects []object) string {
	slices.SortFunc(objects, func(a, b object) int { return strings.Compare(a.path, b.path) })

	sum := sha256.New()
	buf := make([]byte, 0, binary.MaxVarintLen64)

	for _, obj := range objects {
		for _, field := range []string{obj.path, obj.etag} {
			_, _ = sum.Write(binary.AppendUvarint(buf, uint64(len(field))))
			_, _ = sum.Write([]byte(field))
		}

		_, _ = sum.Write(binary.AppendVarint(buf, obj.size))
	}

	return "s3:" + hex.EncodeToString(sum.Sum(nil))
}

var (
	errPluginKey  = errors.New("a plugin key is a directory name")
	errObjectPath = errors.New("object key is not a valid path inside the plugin")
)

// Open downloads the objects of the plugin directory named by desc's key. It fails if the directory holds
// nothing, if an object's key is not a valid path inside it (such as a/../b) or if they hold more than the
// storage's maximum plugin size together. Its listing stops, failing with [plg.ErrPluginTooLarge], as soon as it
// brings the directory over [plg.MaxPluginEntries] objects or [plg.MaxPluginPathBytes] bytes of object paths
// together, before anything is downloaded: Register would refuse the plugin's files, and a listing of S3 has no end
// of its own.
func (s *Storage) Open(ctx context.Context, desc *plg.Descriptor) (fs.FS, error) {
	dir := desc.Key
	if dir == "" || dir == "." || dir == ".." || strings.Contains(dir, "/") {
		return nil, fmt.Errorf("%w %q: %w", plg.ErrInvalidDescriptor, dir, errPluginKey)
	}

	var listed listing

	err := s.walk(ctx, s.prefix+dir+"/", func(path string, obj object) error {
		return listed.add(dir, path, obj)
	})
	if err != nil {
		return nil, err
	}

	return s.fetch(ctx, dir, listed.objects)
}

// listDirs lists the prefix and returns the objects of each plugin directory, by its name.
func (s *Storage) listDirs(ctx context.Context) (map[string]*listing, error) {
	dirs := make(map[string]*listing)

	err := s.walk(ctx, s.prefix, func(key string, obj object) error {
		dir, path, ok := strings.Cut(key, "/")
		if !ok || dir == "" || strings.HasPrefix(dir, ".") || path == "" || strings.HasSuffix(path, "/") {
			return nil
		}

		listed := dirs[dir]
		if listed == nil {
			listed = &listing{}
			dirs[dir] = listed
		}

		return listed.add(dir, path, obj)
	})
	if err != nil {
		return nil, err
	}

	return dirs, nil
}

// listing is the objects of a plugin directory listed so far, and the bytes of their paths together.
type listing struct {
	objects []object
	paths   int
}

// add adds obj, at path in the plugin directory dir, failing with [plg.ErrPluginTooLarge] when it brings the
// directory over [plg.MaxPluginEntries] objects or [plg.MaxPluginPathBytes] bytes of paths.
func (l *listing) add(dir, path string, obj object) error {
	l.paths += len(path)

	switch {
	case len(l.objects) == plg.MaxPluginEntries:
		return fmt.Errorf("s3storage: plugin directory %s: %w: over %d objects",
			dir, plg.ErrPluginTooLarge, plg.MaxPluginEntries)
	case l.paths > plg.MaxPluginPathBytes:
		return fmt.Errorf("s3storage: plugin directory %s: %w: object paths over %d bytes together",
			dir, plg.ErrPluginTooLarge, plg.MaxPluginPathBytes)
	}

	obj.path = path
	l.objects = append(l.objects, obj)

	return nil
}

// walk calls fn with every object under prefix, its key relative to prefix, page by page, stopping at the first
// error fn returns.
func (s *Storage) walk(ctx context.Context, prefix string, fn func(key string, obj object) error) error {
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(s.pageSize),
	})

	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("s3storage: listing s3://%s/%s: %w", s.bucket, prefix, err)
		}

		for _, obj := range page.Contents {
			key := strings.TrimPrefix(aws.ToString(obj.Key), prefix)

			err = fn(key, object{etag: aws.ToString(obj.ETag), size: aws.ToInt64(obj.Size)})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

var errNoManifest = errors.New("no " + manifestName)

// describe returns the descriptor of the plugin directory dir, whose objects are objects.
func (s *Storage) describe(ctx context.Context, dir string, objects []object) (plg.Descriptor, error) {
	i := slices.IndexFunc(objects, func(obj object) bool { return obj.path == manifestName })
	if i < 0 {
		return plg.Descriptor{}, fmt.Errorf("%w: plugin directory %s: %w", plg.ErrInvalidManifest, dir, errNoManifest)
	}

	desc, err := s.manifest(ctx, s.prefix+dir+"/"+manifestName, objects[i])
	if err != nil {
		return plg.Descriptor{}, fmt.Errorf("s3storage: plugin directory %s: %w", dir, err)
	}

	desc.Key = dir
	if desc.Version == "" {
		desc.Version = listingVersion(objects)
	}

	return desc, nil
}

// manifest returns the parsed plugin.yaml at key, from the cache while its ETag is unchanged.
func (s *Storage) manifest(ctx context.Context, key string, obj object) (plg.Descriptor, error) {
	if cached, ok := s.manifests.Load(key); ok && cached.etag == obj.etag {
		return cached.desc, nil
	}

	got, err := s.download(ctx, key, plg.MaxManifestSize)
	if errors.Is(err, errTooLarge) {
		return plg.Descriptor{}, fmt.Errorf("%w: %w", plg.ErrInvalidManifest, err)
	}

	if err != nil {
		return plg.Descriptor{}, err
	}

	desc, err := plg.ParseManifest(got.data)
	if err != nil {
		return plg.Descriptor{}, err //nolint:wrapcheck // the caller names the plugin
	}

	s.manifests.Store(key, manifest{etag: got.etag, desc: desc})

	return desc, nil
}

var errTooLarge = errors.New("too large")

// downloaded is an object's content and its ETag.
type downloaded struct {
	etag string
	data []byte
}

// download returns the content of the object at key, of at most limit bytes, and its ETag.
func (s *Storage) download(ctx context.Context, key string, limit int64) (downloaded, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return downloaded{}, fmt.Errorf("getting %s: %w", key, err)
	}

	data, err := io.ReadAll(io.LimitReader(out.Body, limit+1))

	closeErr := out.Body.Close()
	if err == nil {
		err = closeErr
	}

	if err != nil {
		return downloaded{}, fmt.Errorf("reading %s: %w", key, err)
	}

	if int64(len(data)) > limit {
		return downloaded{}, fmt.Errorf("%s: %w: over %d bytes", key, errTooLarge, limit)
	}

	return downloaded{etag: aws.ToString(out.ETag), data: data}, nil
}

// fetch downloads the objects of the plugin directory dir into an in-memory tree.
func (s *Storage) fetch(ctx context.Context, dir string, objects []object) (fs.FS, error) {
	files := make(fstest.MapFS, len(objects))
	budget := s.maxPluginSize

	for _, obj := range objects {
		if strings.HasSuffix(obj.path, "/") { // a "directory" marker some tools create
			continue
		}

		if !fs.ValidPath(obj.path) {
			return nil, fmt.Errorf("s3storage: plugin directory %s: %q: %w", dir, obj.path, errObjectPath)
		}

		if obj.size > budget {
			return nil, fmt.Errorf(
				"s3storage: plugin directory %s: %w: over %d bytes",
				dir,
				errTooLarge,
				s.maxPluginSize,
			)
		}

		got, err := s.download(ctx, s.prefix+dir+"/"+obj.path, budget)
		if err != nil {
			return nil, fmt.Errorf("s3storage: plugin directory %s: %w", dir, err)
		}

		budget -= int64(len(got.data))
		files[obj.path] = &fstest.MapFile{Data: got.data, Mode: readOnly}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("s3storage: plugin directory %s: %w", dir, fs.ErrNotExist)
	}

	return files, nil
}
