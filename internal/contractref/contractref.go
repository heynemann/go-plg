// Package contractref reads the interface a plugin declares it implements (plugin.yaml's implements,
// "<package path>.<Interface>@<version>"), finds the version of the contract module a program was built with in its
// build info, and decides whether a plugin's pinned version is compatible with it.
package contractref

import (
	"errors"
	"fmt"
	"go/token"
	"runtime/debug"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// Devel is the version a module built from a local checkout has: the main module of a build without a version
// stamped (tests, go run, a build outside version control) or a module replaced by a local directory.
const Devel = "(devel)"

// Ref is the interface a plugin declares it implements.
type Ref struct {
	// Package is the import path of the interface's package: its module's path, then the package's directory in it.
	Package string
	// Interface is the interface's name in Package.
	Interface string
	// Version is the contract module's version the plugin was written against, a canonical semantic version
	// (v1.2.3, v1.2.3-rc.1, a pseudo-version).
	Version string
}

var (
	errMissing = errors.New("implements is required: the interface the plugin implements and the version of its " +
		"contract module, as in example.com/contracts/rewards.Cohort@v1.4.0")
	errNoVersion   = errors.New(`it must end with "@" and the contract module's version, as in @v1.4.0`)
	errNoInterface = errors.New("it must name the interface after its package path, as in " +
		"example.com/contracts/rewards.Cohort")
	errInterface = errors.New("the interface name must be a Go identifier")
	errVersion   = errors.New("the version must be a canonical semantic version, as in v1.4.0")
)

// Parse parses "<package path>.<Interface>@<version>". The package path must be a valid import path, the interface a
// Go identifier and the version canonical semver with its leading v (v1.4.0, not 1.4.0, v1.4 or v1.4.0+meta).
func Parse(value string) (Ref, error) {
	if value == "" {
		return Ref{}, errMissing
	}

	name, version, found := strings.Cut(value, "@")
	if !found {
		return Ref{}, fmt.Errorf("implements %q: %w", value, errNoVersion)
	}

	dot := strings.LastIndexByte(name, '.')
	if dot <= strings.LastIndexByte(name, '/') {
		return Ref{}, fmt.Errorf("implements %q: %w", value, errNoInterface)
	}

	ref := Ref{Package: name[:dot], Interface: name[dot+1:], Version: version}

	err := module.CheckImportPath(ref.Package)
	if err != nil {
		return Ref{}, fmt.Errorf("implements %q: %w", value, err)
	}

	if !token.IsIdentifier(ref.Interface) {
		return Ref{}, fmt.Errorf("implements %q: %w, not %q", value, errInterface, ref.Interface)
	}

	if !semver.IsValid(version) || semver.Canonical(version) != version {
		return Ref{}, fmt.Errorf("implements %q: %w, not %q", value, errVersion, version)
	}

	return ref, nil
}

// Name is the interface's qualified name, "<package path>.<Interface>".
func (r Ref) Name() string {
	return r.Package + "." + r.Interface
}

// String is r as Parse reads it.
func (r Ref) String() string {
	return r.Name() + "@" + r.Version
}

// Module is the module providing a package in a program's build info.
type Module struct {
	// Path is the module's path.
	Path string
	// Version is the module's version as built: a replacement's version when it is replaced by another module
	// version, [Devel] when it is replaced by a local directory or is the main module built from a checkout (no
	// stamped version, or one the go command derived from an untagged or modified checkout).
	Version string
	// Dir is the local directory replacing the module; empty unless it is replaced by one.
	Dir string
}

// Devel reports whether the module was built from a local checkout, so its version says nothing.
func (m Module) Devel() bool {
	return m.Version == Devel
}

// Built returns the module providing the package pkg in the running program's build info ([debug.ReadBuildInfo]).
// A program without build info, or whose build info has no module providing pkg, gets a module of path pkg and
// version [Devel]: its version cannot be told.
func Built(pkg string) Module {
	bi, ok := debug.ReadBuildInfo()
	if ok {
		if mod, found := ModuleOf(bi, pkg); found {
			return mod
		}
	}

	return Module{Path: pkg, Version: Devel}
}

// ModuleOf returns the module of info providing the package pkg: the main module or a dependency, whichever has the
// longest path that is pkg or a prefix of it ending at a slash. A package of an external test (its last element
// ending in _test) belongs to the module of the package it tests. It reports false when no module provides pkg.
func ModuleOf(info *debug.BuildInfo, pkg string) (Module, bool) {
	found := lookup(info, pkg)
	if found == nil {
		trimmed, isTest := strings.CutSuffix(pkg, "_test")
		if !isTest {
			return Module{}, false
		}

		found = lookup(info, trimmed)
		if found == nil {
			return Module{}, false
		}
	}

	return asBuilt(found, found == &info.Main), true
}

// asBuilt returns the module found, the main module when main is set, as the program was built with it.
func asBuilt(found *debug.Module, main bool) Module {
	mod := Module{Path: found.Path, Version: found.Version}

	switch {
	case found.Replace != nil && (found.Replace.Version == Devel || found.Replace.Version == ""):
		// A directory replacement: the go command records it as version (devel).
		mod.Version, mod.Dir = Devel, found.Replace.Path
	case found.Replace != nil:
		mod.Version = found.Replace.Version
	case mod.Version == "":
		mod.Version = Devel
	case main && (module.IsPseudoVersion(mod.Version) || semver.Build(mod.Version) != ""):
		// The go command stamps a main module built in a repository with a pseudo-version for an untagged commit
		// and +dirty for uncommitted changes: a local checkout, not a release.
		mod.Version = Devel
	default:
	}

	return mod
}

// lookup returns the module of info with the longest path providing pkg; nil when none does.
func lookup(info *debug.BuildInfo, pkg string) *debug.Module {
	var found *debug.Module

	consider := func(mod *debug.Module) {
		if provides(mod.Path, pkg) && (found == nil || len(mod.Path) > len(found.Path)) {
			found = mod
		}
	}

	consider(&info.Main)

	for _, dep := range info.Deps {
		consider(dep)
	}

	return found
}

// provides reports whether the module path provides the package pkg.
func provides(path, pkg string) bool {
	rest, found := strings.CutPrefix(pkg, path)

	return path != "" && found && (rest == "" || rest[0] == '/')
}

// ErrIncompatible reports a plugin's pinned contract version that the program's contract version cannot serve.
var ErrIncompatible = errors.New("incompatible contract version")

// Compatible checks that a plugin pinned to the contract version pinned can be served by a program built with the
// contract version built, both canonical semver: the same major version and a minor version not newer than built's.
// Contracts follow semantic versioning, so within a major version a newer contract only adds to what the plugin
// relies on.
func Compatible(built, pinned string) error {
	if semver.Major(pinned) != semver.Major(built) {
		return fmt.Errorf("%w: the plugin is written against %s, a major version other than the service's %s",
			ErrIncompatible, pinned, built)
	}

	if semver.Compare(semver.MajorMinor(pinned), semver.MajorMinor(built)) > 0 {
		return fmt.Errorf("%w: the plugin is written against %s, a minor version newer than the service's %s",
			ErrIncompatible, pinned, built)
	}

	return nil
}
