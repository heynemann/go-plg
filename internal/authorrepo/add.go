package authorrepo

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/heynemann/go-plg/internal/strictyaml"
)

// errOutside reports a plugin directory outside the repository.
var errOutside = errors.New("is not inside the repository")

// AddPlugin lists the plugin directory dir (which must exist, inside the repository) in plg.yaml, after the
// plugins it lists, keeping the file's comments and layout; nothing changes when it lists it already. The
// repository is read again, so it fails as [Load] fails when the plugin breaks a rule (its key is another
// plugin's), leaving plg.yaml as it was.
func (r *Repo) AddPlugin(dir string) error {
	rel, listed := r.Lists(dir)

	switch {
	case listed:
		return nil
	case rel == "" || !filepath.IsLocal(filepath.FromSlash(rel)):
		return fmt.Errorf("%w: %s %w", ErrInvalid, dir, errOutside)
	default:
	}

	data, err := readFile(r.File())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	added, err := addPlugin(data, rel)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInvalid, r.File(), err)
	}

	// The file exists: writing it keeps its mode (fileMode is for files created).
	err = os.WriteFile(r.File(), added, fileMode)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	reread, err := Load(r.Dir)
	if err != nil {
		return errors.Join(err, os.WriteFile(r.File(), data, fileMode))
	}

	*r = *reread

	return nil
}

// addPlugin returns data, a plg.yaml's content, with the plugin directory rel appended to its plugins: as a line
// after the last one of a block sequence, else by writing the file again.
func addPlugin(data []byte, rel string) ([]byte, error) {
	root, err := strictyaml.Parse(data, FileName)
	if err != nil {
		return nil, err //nolint:wrapcheck // names the file
	}

	var plugins *yaml.Node

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "plugins" {
			plugins = root.Content[i+1]
		}
	}

	if plugins == nil || plugins.Kind != yaml.SequenceNode || len(plugins.Content) == 0 {
		return nil, errNoPlugins
	}

	if plugins.Style&yaml.FlowStyle == 0 {
		item, _ := yaml.Marshal(rel) //nolint:errcheck // a string always marshals

		return appendItem(data, plugins, strings.TrimSuffix(string(item), "\n")), nil
	}

	plugins.Content = append(plugins.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: rel})

	return encode(root)
}

// appendItem returns data with an item holding scalar appended to the block sequence seq, on a line after its last
// item, its dash at the column of seq's dashes (whatever the spaces after them).
func appendItem(data []byte, seq *yaml.Node, scalar string) []byte {
	last := seq.Content[len(seq.Content)-1]
	lines := bytes.SplitAfter(data, []byte("\n"))
	item := strings.Repeat(" ", seq.Column-1) + "- " + scalar + "\n"

	if !bytes.HasSuffix(lines[last.Line-1], []byte("\n")) {
		lines[last.Line-1] = append(lines[last.Line-1], '\n')
	}

	return bytes.Join(slices.Insert(lines, last.Line, []byte(item)), nil)
}

// encode writes the document root again.
func encode(root *yaml.Node) ([]byte, error) {
	var out bytes.Buffer

	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)

	err := enc.Encode(root)
	if err == nil {
		err = enc.Close()
	}

	if err != nil {
		return nil, fmt.Errorf("writing: %w", err)
	}

	return out.Bytes(), nil
}
