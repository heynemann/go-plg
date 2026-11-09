package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/heynemann/go-plg"
)

// artifactExt is the extension of the files plg pack writes.
const artifactExt = ".plg"

// artifactStorage serves the .plg artifacts `plg pack` wrote into a directory, each under its file's name without
// the extension: the artifacts of the example author repository (examples/rules/ci.sh writes them to its dist/).
// A platform would keep the same blobs in a bucket or a database; only where it reads them from differs.
type artifactStorage struct {
	root *os.Root
	dir  string
}

// openArtifactStorage returns the storage of the artifacts in dir, read through an os.Root so no name leads out.
func openArtifactStorage(dir string) (*artifactStorage, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("artifacts: %w", err)
	}

	return &artifactStorage{root: root, dir: dir}, nil
}

func (s *artifactStorage) Name() string { return "artifacts " + s.dir }

// Close releases the directory.
func (s *artifactStorage) Close() error {
	return s.root.Close() //nolint:wrapcheck // os names the directory
}

// List opens each artifact and describes it by its plugin.yaml, versioned by the artifact's digest when the
// plugin.yaml names no version, so a reload skips an artifact that has not changed without opening it again.
func (s *artifactStorage) List(context.Context) ([]plg.Descriptor, error) {
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return nil, err //nolint:wrapcheck // plg names the storage, os the file
	}

	var descs []plg.Descriptor

	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || filepath.Ext(name) != artifactExt {
			continue
		}

		art, err := s.artifact(name)
		if err != nil {
			return nil, err
		}

		manifest, err := art.ReadFile("plugin.yaml")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}

		desc, err := plg.ParseManifest(manifest)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}

		desc.Key = strings.TrimSuffix(name, artifactExt)

		if desc.Version == "" { // the digest Register would give it, so a poll skips an unchanged artifact
			desc.Version = art.Provenance().Digest
		}

		descs = append(descs, desc)
	}

	return descs, nil
}

// Open returns the artifact itself, so Register loads its files without another copy and plg.Info and the admin
// handler show its provenance.
func (s *artifactStorage) Open(_ context.Context, d *plg.Descriptor) (fs.FS, error) {
	return s.artifact(d.Key + artifactExt)
}

func (s *artifactStorage) artifact(name string) (*plg.Artifact, error) {
	file, err := s.root.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // plg names the storage, os the file
	}
	defer file.Close() //nolint:errcheck // read-only: OpenArtifact has read everything it keeps

	info, err := file.Stat()
	if err != nil {
		return nil, err //nolint:wrapcheck // plg names the storage, os the file
	}

	art, err := plg.OpenArtifact(file, info.Size())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	return art, nil
}
