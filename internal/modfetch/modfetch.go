// Package modfetch resolves a contract module through the local go command, as `plg validate` resolves the
// contract a plugin pins: `go mod download -json` fetches it into the Go module cache (or finds it there), so
// GOPROXY, GOPRIVATE, GONOSUMDB, credentials and an offline cache work as for any Go build. It also reads the
// version a Go plugin's go.mod requires.
package modfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/bytedance/sonic"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

var (
	// ErrNoGo reports that no go command is on the PATH.
	ErrNoGo = errors.New("the go command is needed to resolve contract modules, and none is on the PATH " +
		"(install Go from https://go.dev/dl)")
	// ErrNotFound reports a package no module provides at the version asked.
	ErrNotFound = errors.New("no module provides the contract package")
	// ErrGoMod reports a Go plugin's go.mod that does not require the contract module at the pinned version.
	ErrGoMod = errors.New("the plugin's go.mod does not require the pinned contract version")

	errNoDir     = errors.New("go mod download gave no module directory")
	errGoCommand = errors.New("go mod download failed")
	errNoPackage = errors.New("the module has no directory for the package")
)

// Module is a module the go command resolved.
type Module struct {
	// Path is the module path.
	Path string
	// Version is the module's version.
	Version string
	// Dir is the module's directory in the module cache.
	Dir string
}

// Fetcher runs the go command.
type Fetcher struct {
	// Environ is the go command's environment (os.Environ()); its PATH also locates the go command.
	Environ []string
}

// download is what `go mod download -json` prints for a module.
type download struct {
	Path    string
	Version string
	Dir     string
	Error   string
}

// Fetch resolves the module providing the package pkg at version: the one of longest path among pkg and its
// prefixes, as the go command picks it, whose directory holds the package. It fails with [ErrNoGo] without a go
// command and with [ErrNotFound], naming each module path tried and why it failed, when none provides it.
func (f *Fetcher) Fetch(ctx context.Context, pkg, version string) (Module, error) {
	goCmd, err := f.lookGo()
	if err != nil {
		return Module{}, err
	}

	work, err := os.MkdirTemp("", "plg-modfetch-*")
	if err != nil {
		return Module{}, fmt.Errorf("modfetch: %w", err)
	}

	defer os.RemoveAll(work) //nolint:errcheck // an empty temporary directory

	var tried []string

	for _, candidate := range candidates(pkg) {
		got, err := f.download(ctx, goCmd, work, candidate+"@"+version)
		if err == nil {
			err = got.holds(pkg)
		}

		if err == nil {
			return Module{Path: got.Path, Version: got.Version, Dir: got.Dir}, nil
		}

		tried = append(tried, err.Error())
	}

	return Module{}, fmt.Errorf("%w %s at %s: %s", ErrNotFound, pkg, version, strings.Join(tried, "; "))
}

// holds checks that the module has a directory for the package pkg.
func (d *download) holds(pkg string) error {
	dir := filepath.Join(d.Dir, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(pkg, d.Path), "/")))

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s@%s: %w %s", d.Path, d.Version, errNoPackage, pkg)
	}

	return nil
}

// candidates returns pkg and its prefixes ending at a slash, longest first: the module paths that may provide pkg.
func candidates(pkg string) []string {
	paths := []string{pkg}

	for i := strings.LastIndexByte(pkg, '/'); i > 0; i = strings.LastIndexByte(pkg[:i], '/') {
		if module.CheckPath(pkg[:i]) == nil {
			paths = append(paths, pkg[:i])
		}
	}

	return paths
}

// download runs `go mod download -json query` in the empty directory work, outside any module and workspace.
func (f *Fetcher) download(ctx context.Context, goCmd, work, query string) (download, error) {
	var stdout, stderr bytes.Buffer

	//nolint:gosec // G204: the go command found on the PATH, with a module query built from a parsed reference
	cmd := exec.CommandContext(ctx, goCmd, "mod", "download", "-json", query)
	cmd.Dir, cmd.Stdout, cmd.Stderr = work, &stdout, &stderr

	cmd.Env = append(slices.Clone(f.Environ), "GOWORK=off", "GO111MODULE=on")

	runErr := cmd.Run()

	var got download

	err := sonic.Unmarshal(stdout.Bytes(), &got)
	if err != nil { // the go command failed before resolving anything
		return download{}, fmt.Errorf("%s: %w: %w: %s", query, errGoCommand, runErr, oneLine(stderr.String()))
	}

	if got.Error != "" { // the go command's message, which names the query
		return download{}, errors.New(oneLine(got.Error)) //nolint:err113 // the go command's message
	}

	if runErr != nil {
		return download{}, fmt.Errorf("%s: %w: %w: %s", query, errGoCommand, runErr, oneLine(stderr.String()))
	}

	if got.Dir == "" {
		return download{}, fmt.Errorf("%s: %w", query, errNoDir)
	}

	return got, nil
}

// lookGo finds the go command on the environment's PATH (exec.LookPath reads the process's own).
func (f *Fetcher) lookGo() (string, error) {
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	for _, dir := range filepath.SplitList(getenv(f.Environ, "PATH")) {
		if candidate := filepath.Join(dir, name); dir != "" && executable(candidate) {
			return candidate, nil
		}
	}

	return "", ErrNoGo
}

// executable reports whether the file name is a regular file that may be executed.
func executable(name string) bool {
	info, err := os.Stat(name)

	return err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0)
}

// getenv returns the last value of key in environ, as the go command reads it.
func getenv(environ []string, key string) string {
	value := ""

	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value = v
		}
	}

	return value
}

// oneLine joins a multi-line message.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Requirement is what a Go plugin's go.mod says of the contract module.
type Requirement struct {
	// GoMod is the go.mod file's path.
	GoMod string
	// Version is the version it requires, or that a replace directive puts in its place.
	Version string
	// Dir is the local directory a replace directive puts in its place; Version is then empty.
	Dir string
}

// Required reads the go.mod of the module holding the directory dir (dir's or the nearest above it) and returns
// what it requires of the module modPath, after its replace directives. It fails with [ErrGoMod] when there is
// no go.mod or it does not require modPath.
func Required(dir, modPath string) (Requirement, error) {
	found, err := findGoMod(dir)
	if err != nil {
		return Requirement{}, err
	}

	goMod := found.path

	file, err := modfile.Parse(goMod, found.data, nil)
	if err != nil {
		return Requirement{}, fmt.Errorf("modfetch: %w", err)
	}

	req := Requirement{GoMod: goMod}

	for _, required := range file.Require {
		if required.Mod.Path == modPath {
			req.Version = required.Mod.Version
		}
	}

	if req.Version == "" {
		return req, fmt.Errorf("%w: %s does not require %s (Go plugins import the contract package)", ErrGoMod,
			goMod, modPath)
	}

	for _, replace := range file.Replace {
		if replace.Old.Path == modPath && (replace.Old.Version == "" || replace.Old.Version == req.Version) {
			req.replaced(replace.New)
		}
	}

	return req, nil
}

// replaced records the replacement of the module required: a version, or a directory (relative to the go.mod's).
func (r *Requirement) replaced(by module.Version) {
	if by.Version != "" {
		r.Version = by.Version

		return
	}

	r.Version, r.Dir = "", by.Path
	if !filepath.IsAbs(r.Dir) {
		r.Dir = filepath.Join(filepath.Dir(r.GoMod), r.Dir)
	}
}

// ImportPath returns the import path of the package in the directory dir: the module path of the go.mod in dir or
// the nearest directory above it, joined with dir's path below that go.mod's directory. It fails with [ErrGoMod]
// when there is no go.mod or it declares no module path.
func ImportPath(dir string) (string, error) {
	found, err := findGoMod(dir)
	if err != nil {
		return "", err
	}

	modPath := modfile.ModulePath(found.data)
	if modPath == "" {
		return "", fmt.Errorf("%w: %s declares no module path", ErrGoMod, found.path)
	}

	return path.Join(modPath, found.below), nil
}

// goModFile is a go.mod file read.
type goModFile struct {
	path  string
	below string // the directory findGoMod started from, below the go.mod's (slash-separated, "" for the same)
	data  []byte
}

// findGoMod reads the go.mod in dir or the nearest directory above it.
func findGoMod(dir string) (goModFile, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return goModFile{}, fmt.Errorf("modfetch: %w", err)
	}

	start := dir

	for {
		found := goModFile{path: filepath.Join(dir, "go.mod")}

		found.data, err = os.ReadFile(found.path)
		if err == nil {
			found.below = filepath.ToSlash(strings.TrimLeft(strings.TrimPrefix(start, dir), string(filepath.Separator)))

			return found, nil
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return goModFile{}, fmt.Errorf("modfetch: %w", err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return goModFile{}, fmt.Errorf("%w: no go.mod in %s or above it (Go plugins are Go modules requiring "+
				"the contract module)", ErrGoMod, dir)
		}

		dir = parent
	}
}
