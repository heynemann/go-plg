package s3storage

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/heynemann/go-plg"
)

// artifactExt is the extension of the artifacts `plg pack` writes, which the objects of the artifact layout have.
const artifactExt = ".plg"

// listArtifacts lists the artifacts under the prefix and returns their descriptors, sorted by key (see the package
// documentation).
func (s *Storage) listArtifacts(ctx context.Context) ([]plg.Descriptor, error) {
	var artifacts []object

	err := s.walk(ctx, s.prefix, func(name string, obj object) error {
		key, ok := strings.CutSuffix(name, artifactExt)
		if ok && key != "" && !strings.HasPrefix(key, ".") && !strings.Contains(key, "/") {
			obj.path = name
			artifacts = append(artifacts, obj)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	descs := make([]plg.Descriptor, 0, len(artifacts))
	listed := make(map[string]struct{}, len(artifacts))

	for _, obj := range artifacts {
		desc, err := s.describeArtifact(ctx, obj)
		if err != nil {
			return nil, fmt.Errorf("s3storage: artifact %s: %w", obj.path, err)
		}

		descs = append(descs, desc)
		listed[s.prefix+obj.path] = struct{}{}
	}

	// Forget the artifacts no longer listed: one published again is described anew.
	s.manifests.Range(func(key string, _ manifest) bool {
		if _, ok := listed[key]; !ok {
			s.manifests.Delete(key)
		}

		return true
	})

	slices.SortFunc(descs, func(a, b plg.Descriptor) int { return cmp.Compare(a.Key, b.Key) })

	return descs, nil
}

// describeArtifact returns the descriptor of the artifact obj: the one cached while its ETag is unchanged,
// otherwise its plugin.yaml's, downloaded. An artifact that cannot be opened keeps the descriptor of the version
// listed before it, if any, under a version of its own, so that Open fails for that one plugin.
func (s *Storage) describeArtifact(ctx context.Context, obj object) (plg.Descriptor, error) {
	key := s.prefix + obj.path

	cached, known := s.manifests.Load(key)
	if known && cached.etag == obj.etag {
		return cached.desc, nil
	}

	var (
		art  *plg.Artifact
		etag string
		err  error
	)

	if obj.size > s.maxPluginSize { // as listed: refused before downloading any of it
		err = fmt.Errorf("%w: over %d bytes", errTooLarge, s.maxPluginSize)
	} else {
		art, etag, err = s.readArtifact(ctx, key)
	}

	if err != nil {
		if !known || !unopenable(err) {
			return plg.Descriptor{}, err
		}

		desc := cached.desc
		desc.Version = listingVersion([]object{obj})
		s.manifests.Store(key, manifest{etag: obj.etag, desc: desc})

		return desc, nil
	}

	desc, err := artifactDescriptor(art, strings.TrimSuffix(obj.path, artifactExt))
	if err != nil {
		return plg.Descriptor{}, err
	}

	s.manifests.Store(key, manifest{etag: etag, desc: desc})

	return desc, nil
}

// artifactDescriptor returns the descriptor of art, the artifact of the plugin key: its plugin.yaml's, versioned
// by its provenance's digest unless the plugin.yaml names a version.
func artifactDescriptor(art *plg.Artifact, key string) (plg.Descriptor, error) {
	var desc plg.Descriptor

	data, err := art.ReadFile(manifestName) // OpenArtifact made sure there is one
	if err == nil {
		desc, err = plg.ParseManifest(data)
	}

	if err != nil {
		return plg.Descriptor{}, err //nolint:wrapcheck // the caller names the artifact
	}

	desc.Key = key
	if desc.Version == "" { // the digest Register would give it
		desc.Version = art.Provenance().Digest
	}

	return desc, nil
}

// unopenable reports whether err says an artifact cannot be opened whatever the attempt: it is not an artifact,
// over plg's limits or over the storage's maximum size.
func unopenable(err error) bool {
	return errors.Is(err, plg.ErrInvalidArtifact) || errors.Is(err, plg.ErrPluginTooLarge) ||
		errors.Is(err, errTooLarge)
}

// readArtifact downloads the artifact at key, of at most the storage's maximum plugin size, and opens it; it also
// returns the ETag of what it downloaded.
func (s *Storage) readArtifact(ctx context.Context, key string) (*plg.Artifact, string, error) {
	got, err := s.download(ctx, key, s.maxPluginSize)
	if err != nil {
		return nil, "", err
	}

	art, err := plg.OpenArtifact(bytes.NewReader(got.data), int64(len(got.data)))
	if err != nil {
		return nil, "", err //nolint:wrapcheck // the caller names the artifact
	}

	return art, got.etag, nil
}

// openArtifact downloads and opens the artifact of the plugin key.
func (s *Storage) openArtifact(ctx context.Context, key string) (fs.FS, error) {
	name := key + artifactExt

	art, _, err := s.readArtifact(ctx, s.prefix+name)
	if err != nil {
		return nil, fmt.Errorf("s3storage: artifact %s: %w", name, err)
	}

	return art, nil
}
