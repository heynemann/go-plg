package plg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"

	"go.yaml.in/yaml/v3"
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
	errManifestEmpty       = errors.New("plugin.yaml holds no document")
	errManifestDocuments   = errors.New("plugin.yaml must hold exactly one document")
	errManifestMapping     = errors.New("plugin.yaml must be a mapping")
	errManifestAnchor      = errors.New("anchors are not allowed")
	errManifestAlias       = errors.New("aliases are not allowed")
	errManifestMergeKey    = errors.New("merge keys (<<) are not allowed")
	errManifestTag         = errors.New("tags other than YAML's core types are not allowed")
	errManifestKey         = errors.New("mapping keys must be plain scalars")
	errManifestDuplicate   = errors.New("duplicate key")
)

// allowedTags are the tags a plugin.yaml node may carry, written or implied: YAML's core schema. A timestamp is
// implied by an unquoted date, which a string field takes verbatim.
var allowedTags = map[string]bool{ //nolint:gochecknoglobals // a constant set; Go has no constant maps
	"!!str": true, "!!int": true, "!!float": true, "!!bool": true, "!!null": true, "!!timestamp": true,
	"!!map": true, "!!seq": true,
}

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

	err := checkManifestTree(data)
	if err != nil {
		return Descriptor{}, err
	}

	// The tree is checked: no alias can expand, so decoding it again is bounded by the size cap.
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var parsed manifest

	err = decoder.Decode(&parsed)
	if err != nil {
		return Descriptor{}, fmt.Errorf("parsing: %w", err)
	}

	return checkManifest(parsed)
}

// checkManifestTree parses data into a node tree and checks it holds exactly one document, a mapping, within the
// subset [ParseManifest] describes.
func checkManifestTree(data []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))

	var doc yaml.Node

	err := decoder.Decode(&doc)
	if errors.Is(err, io.EOF) {
		return errManifestEmpty
	}

	if err != nil {
		return fmt.Errorf("parsing: %w", err)
	}

	var next yaml.Node

	err = decoder.Decode(&next)
	if !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("parsing: %w", err)
		}

		return errManifestDocuments
	}

	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errManifestMapping
	}

	return checkNode(doc.Content[0])
}

// checkNode checks node and the nodes below it against the subset [ParseManifest] describes.
func checkNode(node *yaml.Node) error {
	switch {
	case node.Kind == yaml.AliasNode:
		return fmt.Errorf("line %d: %w", node.Line, errManifestAlias)
	case node.Anchor != "":
		return fmt.Errorf("line %d: %w (&%s)", node.Line, errManifestAnchor, node.Anchor)
	case node.Tag == "!!merge":
		return fmt.Errorf("line %d: %w", node.Line, errManifestMergeKey)
	case !allowedTags[node.Tag]:
		return fmt.Errorf("line %d: %w (%s)", node.Line, errManifestTag, node.Tag)
	}

	if node.Kind == yaml.MappingNode {
		err := checkKeys(node)
		if err != nil {
			return err
		}
	}

	for _, child := range node.Content {
		err := checkNode(child)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkKeys checks that the keys of the mapping node are scalars, none of them repeated.
func checkKeys(node *yaml.Node) error {
	seen := make(map[string]bool, len(node.Content)/2)

	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Tag == "!!merge" {
			return fmt.Errorf("line %d: %w", key.Line, errManifestMergeKey)
		}

		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: %w", key.Line, errManifestKey)
		}

		if seen[key.Value] {
			return fmt.Errorf("line %d: %w %q", key.Line, errManifestDuplicate, key.Value)
		}

		seen[key.Value] = true
	}

	return nil
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
