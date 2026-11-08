// Package authorrepo reads an author repository's plg.yaml, which lists the plugins a team keeps in one repository
// and the settings plg validate (and plg pack) apply to them all: shared directories copied into each plugin's
// files, limit overrides, required ownership and contract modules replaced by local checkouts.
package authorrepo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/directive"
	"github.com/heynemann/go-plg/internal/strictyaml"
)

// FileName is the name of an author repository's file, at its root.
const FileName = "plg.yaml"

var (
	// ErrInvalid reports a plg.yaml that cannot be read or breaks its rules.
	ErrInvalid = errors.New("invalid plg.yaml")

	errSize       = fmt.Errorf("is over %d bytes", plg.MaxManifestSize)
	errIrregular  = errors.New("is not a regular file")
	errNoPlugins  = errors.New("plugins must list at least one plugin directory")
	errNotLocal   = errors.New("is not a directory inside the repository (a relative path, without ..)")
	errTwice      = errors.New("is listed twice")
	errNotDir     = errors.New("is not a directory")
	errNested     = errors.New("a plugin or shared directory cannot be inside another")
	errSameKey    = errors.New("have the same key (a plugin's key is its directory's name)")
	errNoDir      = errors.New("needs a directory")
	errModulePath = errors.New("declares another module")
)

// Repo is an author repository, as its plg.yaml describes it.
type Repo struct {
	// Dir is the repository's root: the absolute directory holding plg.yaml.
	Dir string
	// Plugins are the plugins' directories, relative to Dir (slash-separated, cleaned), in plg.yaml's order.
	Plugins []string
	// Shared are the directories, relative to Dir as Plugins, copied into every plugin's files.
	Shared []string
	// Replace are the contract modules replaced by local checkouts, by module path.
	Replace []Replace
	// Limits override the contract's limits for every plugin.
	Limits directive.Limits
	// RequireOwnership makes a plugin's missing ownership an error instead of a warning.
	RequireOwnership bool
}

// Replace is a contract module replaced by a local checkout.
type Replace struct {
	// Path is the module path.
	Path string
	// Dir is the checkout: the absolute directory holding the module's go.mod.
	Dir string
}

// file is plg.yaml's content.
type file struct {
	Limits           map[string]string `yaml:"limits"`
	Replace          map[string]string `yaml:"replace"`
	Plugins          []string          `yaml:"plugins"`
	Shared           []string          `yaml:"shared"`
	RequireOwnership bool              `yaml:"requireOwnership"`
}

// Exists reports whether dir holds a plg.yaml (of any kind: Load says what is wrong with it).
func Exists(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, FileName))

	return err == nil
}

// Load reads the plg.yaml in dir, in the strict subset of YAML plugin.yaml is read in, and checks it: at least
// one plugin; plugin and shared directories that exist inside dir (local paths), none listed twice, none inside
// another, plugins of distinct keys (their directories' names); limits as a contract's //plg:limits directive
// writes them; replaced module paths that are valid, each by a directory (relative to dir unless absolute) whose
// go.mod declares that module. It fails with [ErrInvalid].
func Load(dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	repo := &Repo{Dir: abs}

	data, err := readFile(repo.File())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	var parsed file

	err = strictyaml.Decode(data, FileName, &parsed)
	if err == nil {
		err = repo.fill(&parsed)
	}

	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, repo.File(), err)
	}

	return repo, nil
}

// Find returns the author repository the plugin directory dir is in: the nearest plg.yaml in dir's parent and
// the directories above it, up to the version-control root (the first directory holding .git). It returns nil
// when there is none.
func Find(dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	for current := filepath.Dir(abs); ; current = filepath.Dir(current) {
		if Exists(current) {
			return Load(current)
		}

		if isRoot(current) {
			return nil, nil //nolint:nilnil // no repository is not an error
		}
	}
}

// isRoot reports whether dir is where Find stops looking: a version-control root, or the file system's.
func isRoot(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))

	return err == nil || filepath.Dir(dir) == dir
}

// readFile reads name, which must be a regular file of at most plg.MaxManifestSize bytes. It checks the type
// before opening, since opening a FIFO can wait for a writer.
func readFile(name string) ([]byte, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // the error names the file
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %w", name, errIrregular)
	}

	opened, err := os.Open(name) //nolint:gosec // G304: the repository the user names
	if err != nil {
		return nil, err //nolint:wrapcheck // the error names the file
	}

	defer opened.Close() //nolint:errcheck // read only

	data, err := io.ReadAll(io.LimitReader(opened, plg.MaxManifestSize+1))
	if err != nil {
		return nil, err //nolint:wrapcheck // the error names the file
	}

	if len(data) > plg.MaxManifestSize {
		return nil, fmt.Errorf("%s %w", name, errSize)
	}

	return data, nil
}

// File is the path of the repository's plg.yaml.
func (r *Repo) File() string {
	return filepath.Join(r.Dir, FileName)
}

// Lists returns the plugin directory dir as the repository lists it, and whether it does.
func (r *Repo) Lists(dir string) (string, bool) {
	abs, absErr := filepath.Abs(dir)

	rel, err := filepath.Rel(r.Dir, abs)
	if absErr != nil || err != nil {
		return "", false
	}

	rel = filepath.ToSlash(rel)

	return rel, slices.Contains(r.Plugins, rel)
}

// Replacement returns the replace whose module provides the package pkg (the module path is pkg or a prefix of it
// ending at a slash; the longest wins, as the go command picks a module), and whether there is one.
func (r *Repo) Replacement(pkg string) (Replace, bool) {
	var found Replace

	for _, replace := range r.Replace {
		if within(pkg, replace.Path) && len(replace.Path) > len(found.Path) {
			found = replace
		}
	}

	return found, found.Path != ""
}

// fill checks parsed and fills the repository from it.
func (r *Repo) fill(parsed *file) error {
	if len(parsed.Plugins) == 0 {
		return errNoPlugins
	}

	r.RequireOwnership = parsed.RequireOwnership

	var err error

	r.Plugins, err = r.dirs("plugins", parsed.Plugins)
	if err == nil {
		r.Shared, err = r.dirs("shared", parsed.Shared)
	}

	for _, check := range []func() error{
		r.distinct, r.uniqueKeys, func() error { return r.limits(parsed.Limits) },
		func() error { return r.replaces(parsed.Replace) },
	} {
		if err != nil {
			break
		}

		err = check()
	}

	return err
}

// dirs checks the directories the list field lists: local paths of existing directories other than the
// repository's root, none listed twice. It returns them cleaned, slash-separated.
func (r *Repo) dirs(field string, listed []string) ([]string, error) {
	cleaned := make([]string, 0, len(listed))

	for _, dir := range listed {
		clean, err := r.dir(dir)
		if err == nil && slices.Contains(cleaned, clean) {
			err = fmt.Errorf("%s %w", clean, errTwice)
		}

		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}

		cleaned = append(cleaned, clean)
	}

	return cleaned, nil
}

// dir checks one listed directory, a local path of an existing directory other than the repository's root, and
// returns it cleaned, slash-separated.
func (r *Repo) dir(dir string) (string, error) {
	clean := path.Clean(filepath.ToSlash(dir))
	if dir == "" || clean == "." || !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", fmt.Errorf("%q %w", dir, errNotLocal)
	}

	info, err := os.Stat(filepath.Join(r.Dir, filepath.FromSlash(clean)))
	if err != nil {
		return "", err //nolint:wrapcheck // the error names the directory; dirs adds the field
	}

	if !info.IsDir() {
		return "", fmt.Errorf("%s %w", clean, errNotDir)
	}

	return clean, nil
}

// distinct checks that no listed directory is inside another.
func (r *Repo) distinct() error {
	all := slices.Concat(r.Plugins, r.Shared)

	for i, a := range all {
		for _, b := range all[i+1:] {
			if within(a, b) || within(b, a) {
				return fmt.Errorf("%s and %s: %w", a, b, errNested)
			}
		}
	}

	return nil
}

// uniqueKeys checks that the plugins' keys, their directories' names, differ.
func (r *Repo) uniqueKeys() error {
	keys := make(map[string]string, len(r.Plugins))

	for _, plugin := range r.Plugins {
		key := path.Base(plugin)
		if other, ok := keys[key]; ok {
			return fmt.Errorf("plugins: %s and %s %w: %q", other, plugin, errSameKey, key)
		}

		keys[key] = plugin
	}

	return nil
}

// limits sets the limit overrides, checked as a //plg:limits directive's.
func (r *Repo) limits(written map[string]string) error {
	for _, key := range sortedKeys(written) {
		err := r.Limits.Set(key, written[key])
		if err != nil {
			return fmt.Errorf("limits: %w", err)
		}
	}

	return nil
}

// replaces sets the replaced modules, sorted by path, checking each checkout's go.mod.
func (r *Repo) replaces(written map[string]string) error {
	for _, modPath := range sortedKeys(written) {
		replace, err := r.replace(modPath, written[modPath])
		if err != nil {
			return fmt.Errorf("replace: %w", err)
		}

		r.Replace = append(r.Replace, replace)
	}

	return nil
}

// replace checks that modPath is a valid module path and dir (relative to the repository unless absolute) holds
// a go.mod declaring it.
func (r *Repo) replace(modPath, dir string) (Replace, error) {
	err := module.CheckPath(modPath)
	if err != nil {
		return Replace{}, err //nolint:wrapcheck // the error names the path; replaces adds the field
	}

	if dir == "" {
		return Replace{}, fmt.Errorf("%s %w", modPath, errNoDir)
	}

	if !filepath.IsAbs(dir) {
		dir = filepath.Join(r.Dir, dir)
	}

	goMod := filepath.Join(dir, "go.mod")

	data, err := readFile(goMod)
	if err != nil {
		return Replace{}, fmt.Errorf("%s: the directory must hold the module's go.mod: %w", modPath, err)
	}

	if declared := modfile.ModulePath(data); declared != modPath {
		return Replace{}, fmt.Errorf("%s %w: %q, not %s", goMod, errModulePath, declared, modPath)
	}

	return Replace{Path: modPath, Dir: dir}, nil
}

// sortedKeys returns m's keys, sorted.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	return keys
}

// within reports whether the directory inner is outer or inside it.
func within(inner, outer string) bool {
	return inner == outer || strings.HasPrefix(inner, outer+"/")
}
