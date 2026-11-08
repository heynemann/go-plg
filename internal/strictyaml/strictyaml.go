// Package strictyaml decodes the YAML files plg reads (plugin.yaml, the plg command's profiles.yaml) in one strict
// subset: exactly one document, a mapping, with no anchors, aliases, merge keys (<<) or tags other than YAML's
// core types, no duplicate or non-scalar keys, and no keys the target type does not have (keys are case
// sensitive). The caller bounds the input's size.
package strictyaml

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

var (
	errNoDocument = errors.New("holds no document")
	errDocuments  = errors.New("must hold exactly one document")
	errMapping    = errors.New("must be a mapping")
	errAnchor     = errors.New("anchors are not allowed")
	errAlias      = errors.New("aliases are not allowed")
	errMergeKey   = errors.New("merge keys (<<) are not allowed")
	errTag        = errors.New("tags other than YAML's core types are not allowed")
	errKey        = errors.New("mapping keys must be plain scalars")
	errDuplicate  = errors.New("duplicate key")
)

// allowedTags are the tags a node may carry, written or implied: YAML's core schema. A timestamp is implied by an
// unquoted date, which a string field takes verbatim.
var allowedTags = map[string]bool{ //nolint:gochecknoglobals // a constant set; Go has no constant maps
	"!!str": true, "!!int": true, "!!float": true, "!!bool": true, "!!null": true, "!!timestamp": true,
	"!!map": true, "!!seq": true,
}

// Decode checks that data, the content of the file name (named in the errors), is one document within the
// subset, then decodes it into out, failing on keys out does not have.
func Decode(data []byte, name string, out any) error {
	err := checkTree(data, name)
	if err != nil {
		return err
	}

	// The tree is checked: no alias can expand, so decoding it again is bounded by the input's size.
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	err = decoder.Decode(out)
	if err != nil {
		return fmt.Errorf("parsing: %w", err)
	}

	return nil
}

// checkTree parses data into a node tree and checks it holds exactly one document, a mapping, within the subset.
func checkTree(data []byte, name string) error {
	root, err := document(data, name)
	if err != nil {
		return err
	}

	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s %w", name, errMapping)
	}

	return checkNode(root)
}

// Parse checks that data, the content of the file name (named in the errors), is one document within the subset
// and returns its root node, of any kind (a mapping, a sequence or a scalar). The caller bounds the input's size.
func Parse(data []byte, name string) (*yaml.Node, error) {
	root, err := document(data, name)
	if err == nil {
		err = checkNode(root)
	}

	if err != nil {
		return nil, err
	}

	return root, nil
}

// document parses data, which must hold exactly one document, and returns the document's root node.
func document(data []byte, name string) (*yaml.Node, error) {
	doc, err := oneDocument(data, name)
	if err != nil {
		return nil, err
	}

	if len(doc.Content) != 1 {
		return nil, fmt.Errorf("%s %w", name, errNoDocument)
	}

	return doc.Content[0], nil
}

// oneDocument parses data, which must hold exactly one document, into a node tree.
func oneDocument(data []byte, name string) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))

	var doc, next yaml.Node

	err := decoder.Decode(&doc)
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s %w", name, errNoDocument)
	}

	if err == nil {
		err = decoder.Decode(&next)
		if err == nil {
			return nil, fmt.Errorf("%s %w", name, errDocuments)
		}
	}

	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing: %w", err)
	}

	return &doc, nil
}

// checkNode checks node and the nodes below it against the subset.
func checkNode(node *yaml.Node) error {
	err := checkOwn(node)
	if err == nil && node.Kind == yaml.MappingNode {
		err = checkKeys(node)
	}

	for i := 0; err == nil && i < len(node.Content); i++ {
		err = checkNode(node.Content[i])
	}

	return err
}

// checkOwn checks what node itself is, without the nodes below it.
func checkOwn(node *yaml.Node) error {
	switch {
	case node.Kind == yaml.AliasNode:
		return fmt.Errorf("line %d: %w", node.Line, errAlias)
	case node.Anchor != "":
		return fmt.Errorf("line %d: %w (&%s)", node.Line, errAnchor, node.Anchor)
	case node.Tag == "!!merge":
		return fmt.Errorf("line %d: %w", node.Line, errMergeKey)
	case !allowedTags[node.Tag]:
		return fmt.Errorf("line %d: %w (%s)", node.Line, errTag, node.Tag)
	default:
		return nil
	}
}

// checkKeys checks that the keys of the mapping node are scalars, none of them repeated.
func checkKeys(node *yaml.Node) error {
	seen := make(map[string]bool, len(node.Content)/2)

	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Tag == "!!merge" {
			return fmt.Errorf("line %d: %w", key.Line, errMergeKey)
		}

		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: %w", key.Line, errKey)
		}

		if seen[key.Value] {
			return fmt.Errorf("line %d: %w %q", key.Line, errDuplicate, key.Value)
		}

		seen[key.Value] = true
	}

	return nil
}
