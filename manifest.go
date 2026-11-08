package plg

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"

	"go.yaml.in/yaml/v3"

	"github.com/heynemann/go-plg/internal/strictyaml"
)

// manifestName is the file describing a plugin directory of an [FSStorage].
const manifestName = "plugin.yaml"

// legacyManifestName is the manifest plg read before plugin.yaml: never read, only named in the hint for a plugin
// directory that still has one.
const legacyManifestName = "plugin.json"

// MaxManifestSize is the largest plugin.yaml, in bytes: [FSStorage] and [ParseManifest] reject a bigger one with
// [ErrInvalidManifest], and a storage reading manifests from elsewhere should read at most this much plus one.
const MaxManifestSize = 64 << 10

// manifest is the content of a plugin.yaml.
type manifest struct {
	MemoryLimit *manifestInt `yaml:"memoryLimit"`
	Owner       *Owner       `yaml:"owner"`
	Language    Language     `yaml:"language"`
	Entrypoint  string       `yaml:"entrypoint"`
	Version     string       `yaml:"version"`
}

// manifestInt is a non-negative integer of a plugin.yaml. yaml's own decoding into an integer truncates a float
// and wraps a number over its range, so it is decoded from the node: an integer scalar only, in range.
type manifestInt uint64

var errManifestInt = errors.New("must be a non-negative integer of at most 64 bits")

// UnmarshalYAML decodes an integer scalar (decimal, or 0x, 0o, 0b prefixed) into n.
func (n *manifestInt) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return fmt.Errorf("line %d: %w, not %q", node.Line, errManifestInt, node.Value)
	}

	value, err := strconv.ParseUint(node.Value, 0, 64)
	if err != nil {
		return fmt.Errorf("line %d: %w, not %q", node.Line, errManifestInt, node.Value)
	}

	*n = manifestInt(value)

	return nil
}

var (
	errManifestLanguage    = errors.New(`language must be "go", "lua" or "typescript"`)
	errManifestEntrypoint  = errors.New("entrypoint must be a slash-separated path inside the plugin directory")
	errManifestMemoryLimit = fmt.Errorf("memoryLimit must be from %d bytes (one wasm page) to 4 GiB", minMemoryLimit)
)

// ParseManifest parses and checks a plugin.yaml exactly as [FSStorage] does, for storages keeping the same
// manifest in another place (an object store, a database). The returned Descriptor has no Key, and an empty
// Version when the manifest gives none: the storage then picks one (Register falls back to a content hash).
//
// A plugin.yaml is one YAML document, a mapping with the keys [FSStorage] describes, in a strict subset of YAML:
// anchors, aliases, merge keys (<<), tags other than YAML's core types and more than one document are errors, as
// are unknown and duplicate keys (keys are case sensitive). String values take a scalar verbatim, so
// version: 1.10 is "1.10". A malformed manifest, or one over [MaxManifestSize] bytes, fails with
// [ErrInvalidManifest].
func ParseManifest(data []byte) (Descriptor, error) {
	desc, err := decodeManifest(data)
	if err != nil {
		return Descriptor{}, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}

	return desc, nil
}

// decodeManifest parses and checks a plugin.yaml; the returned descriptor has no key.
func decodeManifest(data []byte) (Descriptor, error) {
	if len(data) > MaxManifestSize {
		return Descriptor{}, errManifestSize
	}

	var parsed manifest

	err := strictyaml.Decode(data, manifestName, &parsed)
	if err != nil {
		return Descriptor{}, err //nolint:wrapcheck // strictyaml's errors name the file and the line
	}

	return checkManifest(parsed)
}

// checkManifest checks the values of a decoded plugin.yaml and returns its descriptor, without a key.
func checkManifest(parsed manifest) (Descriptor, error) {
	switch parsed.Language {
	case LanguageGo, LanguageLua, LanguageTypeScript:
	default:
		return Descriptor{}, fmt.Errorf("%w, not %q", errManifestLanguage, parsed.Language)
	}

	if parsed.Entrypoint == "" || !fs.ValidPath(parsed.Entrypoint) {
		return Descriptor{}, fmt.Errorf("%w, not %q", errManifestEntrypoint, parsed.Entrypoint)
	}

	desc := Descriptor{Language: parsed.Language, Entrypoint: parsed.Entrypoint, Version: parsed.Version}

	if parsed.MemoryLimit != nil {
		limit := uint64(*parsed.MemoryLimit)
		if !validMemoryLimit(limit) {
			return Descriptor{}, fmt.Errorf("%w, not %d", errManifestMemoryLimit, limit)
		}

		desc.MemoryLimit = limit
	}

	if parsed.Owner != nil {
		err := parsed.Owner.check()
		if err != nil {
			return Descriptor{}, err
		}

		desc.Owner = parsed.Owner
	}

	return desc, nil
}
