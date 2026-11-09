package gorestrict

import (
	"go/parser"
	"go/token"
	"path"
	"strconv"
	"strings"
)

// The platform the Go guest's interpreter runs on, whose files it loads.
const (
	guestOS   = "wasip1"
	guestArch = "wasm"
)

// GuestGoMinor is N of the Go 1.N release that built the committed Go guest (guest/golang/golang.wasm.gz, built
// with the Makefile's GOTOOLCHAIN): yaegi in that guest holds the go1.1 to go1.N build tags whatever Go builds the
// host. A test pins it to both. A guest plg build-guest builds uses the plugin module's Go instead, so for it the
// go1.N tags above GuestGoMinor can be judged differently than that guest loads them.
const GuestGoMinor = 27

// GuestLoads reports whether the Go guest's interpreter, yaegi v0.16.1 on wasip1/wasm, loads the Go source file name
// (its base name decides) whose content is src, deciding as yaegi does for the files of a package it imports. By name,
// it skips files not ending in .go, names starting with "_" or "." and _test.go files. Of the elements after the name's
// first "_" (separated by "_"), one alone that is an operating system or architecture yaegi knows (see knownOS and
// knownArch) other than wasip1 and wasm skips the file (x_windows.go, x_amd64.go); of two or more, the last one being
// such an architecture does (x_y_amd64.go, x_wasip1_amd64.go), and so does the last but one being such an operating
// system followed by an architecture yaegi knows (x_linux_wasm.go). Any other name loads: x_riscv64.go, x_hurd.go,
// x_y_windows.go. It then reads the comments before the package clause: every `// +build` line among them must hold
// (one of its space-separated options does when each of the option's comma-separated tags does: wasip1, wasm, or go1.N
// up to N = GuestGoMinor, the Go release that built the committed guest; negated by a leading "!"). `//go:build` lines
// are not read. A package clause that does not parse fails the guest's load: GuestLoads returns the parser's error,
// positioned in fset. yaegi panics on an empty tag (`// +build a,`), which here does not hold; a `// yaegi:tags` line,
// which adds tags for the files yaegi reads after it, is not followed.
func GuestLoads(fset *token.FileSet, name string, src []byte) (bool, error) {
	if !nameLoads(path.Base(name)) {
		return false, nil
	}

	header, err := parser.ParseFile(fset, name, src, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		return false, err //nolint:wrapcheck // the parser's error names the file:line:column
	}

	for _, group := range header.Comments {
		for line := range strings.SplitSeq(strings.TrimSpace(group.Text()), "\n") {
			if !buildLineHolds(line) {
				return false, nil
			}
		}
	}

	return true, nil
}

// nameLoads is yaegi's skipFile, inverted, for the base name base of a file of an imported package.
func nameLoads(base string) bool {
	stem, ok := strings.CutSuffix(base, ".go")
	if !ok || stem == "" || strings.HasPrefix(stem, "_") || strings.HasPrefix(stem, ".") ||
		strings.HasSuffix(stem, "_test") {
		return false
	}

	_, suffix, ok := strings.Cut(stem, "_")

	return !ok || suffixLoads(strings.Split(suffix, "_"))
}

// suffixLoads reports whether yaegi loads a file whose name has the elements elems after its first "_".
func suffixLoads(elems []string) bool {
	last := elems[len(elems)-1]
	if len(elems) == 1 {
		return !foreignOS(last) && !foreignArch(last)
	}

	return !foreignArch(last) && (!foreignOS(elems[len(elems)-2]) || !knownArch(last))
}

// foreignOS reports whether name is an operating system yaegi knows other than the guest's.
func foreignOS(name string) bool {
	return name != guestOS && knownOS(name)
}

// foreignArch reports whether name is an architecture yaegi knows other than the guest's.
func foreignArch(name string) bool {
	return name != guestArch && knownArch(name)
}

// knownOS reports whether yaegi v0.16.1 knows name as an operating system in a file name (interp/build.go's
// knownOs, which lacks some of Go's, such as hurd and zos).
func knownOS(name string) bool {
	switch name {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "js", "linux", "netbsd", "openbsd",
		"plan9", "solaris", "wasip1", "windows":
		return true
	default:
		return false
	}
}

// knownArch reports whether yaegi v0.16.1 knows name as an architecture in a file name (interp/build.go's knownArch,
// which lacks some of Go's, such as riscv64 and sparc64).
func knownArch(name string) bool {
	switch name {
	case "386", "amd64", "arm", "arm64", "loong64", "mips", "mips64", "mips64le", "mipsle", "ppc64", "ppc64le", "s390x",
		"wasm":
		return true
	default:
		return false
	}
}

// buildLineHolds reports whether a line of a comment holds for the guest: true unless it is a `+build` line whose
// space-separated options all fail.
func buildLineHolds(line string) bool {
	options, ok := strings.CutPrefix(line, "+build ")
	if !ok {
		return true
	}

	for option := range strings.SplitSeq(strings.TrimSpace(options), " ") {
		if buildOptionHolds(option) {
			return true
		}
	}

	return false
}

// buildOptionHolds reports whether every comma-separated tag of option holds.
func buildOptionHolds(option string) bool {
	for tag := range strings.SplitSeq(option, ",") {
		if !buildTagHolds(tag) {
			return false
		}
	}

	return true
}

// buildTagHolds reports whether tag, negated by a leading "!", holds for the guest. An empty tag does not.
func buildTagHolds(tag string) bool {
	if tag == "" {
		return false
	}

	name, negated := strings.CutPrefix(tag, "!")

	holds := name == guestOS || name == guestArch
	if version, ok := strings.CutPrefix(name, "go1."); ok && name != "go1." {
		minor, err := strconv.Atoi(version)
		holds = err == nil && minor <= GuestGoMinor
	}

	return holds != negated
}
