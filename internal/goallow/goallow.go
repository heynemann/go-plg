// Package goallow is the Go guest's import allowlist: the packages Go plugins and contract packages may import
// (standard library packages and plg's plugin package). The list lives in packages.txt, which the Makefile also
// reads to extract the guest's symbol tables, so the guest, `plg gen` and the tests agree on one list.
package goallow

import (
	_ "embed" // packages.txt
	"slices"
	"strings"
)

//go:embed packages.txt
var list string

// Packages returns the allowlisted import paths in the order packages.txt lists them.
func Packages() []string {
	var pkgs []string

	for line := range strings.Lines(list) {
		line, _, _ = strings.Cut(line, "#")
		if pkg := strings.TrimSpace(line); pkg != "" {
			pkgs = append(pkgs, pkg)
		}
	}

	return pkgs
}

// Allowed reports whether a Go plugin or contract package may import the package with this import path.
func Allowed(importPath string) bool {
	return slices.Contains(Packages(), importPath)
}
