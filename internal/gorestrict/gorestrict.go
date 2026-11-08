// Package gorestrict checks a Go plugin's source against what the Go guest runs: no go statements (goroutines
// inside a WebAssembly guest give no parallelism and are paused between calls) and, for plg validate, only imports
// the guest can provide. Register checks the go statements before loading a Go plugin; plg validate checks both
// before loading it, so that an author sees every position at once.
package gorestrict

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

var (
	// ErrGoStatement reports source containing go statements; the message names the position of each.
	ErrGoStatement = errors.New("plg: go statements are not allowed in plugins")
	// ErrImport reports source importing packages the Go guest cannot provide; the message names each import
	// with its position.
	ErrImport = errors.New("plg: the plugin imports packages the Go guest cannot provide")
)

// SyntaxError is a Go file of the plugin that does not parse: the Go guest fails to load it.
type SyntaxError struct {
	// Err is the parser's error, naming the file:line:column.
	Err error
}

func (e *SyntaxError) Error() string {
	return e.Err.Error()
}

func (e *SyntaxError) Unwrap() error {
	return e.Err
}

// IsSource reports whether the entry named name is a Go source file a plugin instance receives: a regular .go
// file that is not a test.
func IsSource(name string, entry fs.DirEntry) bool {
	return entry.Type().IsRegular() && path.Ext(name) == ".go" && !strings.HasSuffix(name, "_test.go")
}

// Check parses every Go source file below dir in files (see [IsSource]; symbolic links are not copied into a
// plugin, so they are not read) and fails with [ErrGoStatement] naming the position (file:line:column, the file
// relative to files) of every go statement and, when allowed is not nil, with [ErrImport] naming every import
// allowed refuses, joined. A file that does not parse fails with a [*SyntaxError].
func Check(files fs.FS, dir string, allowed func(importPath string) bool) error {
	fset := token.NewFileSet()

	var statements, imports []string

	err := fs.WalkDir(files, dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || !IsSource(name, entry) {
			return err
		}

		src, err := fs.ReadFile(files, name)
		if err != nil {
			return fmt.Errorf("reading the plugin source: %w", err)
		}

		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return &SyntaxError{Err: err}
		}

		ast.Inspect(file, func(node ast.Node) bool {
			if stmt, ok := node.(*ast.GoStmt); ok {
				statements = append(statements, fset.Position(stmt.Go).String())
			}

			return true
		})

		if allowed != nil {
			imports = append(imports, Refused(fset, file, allowed)...)
		}

		return nil
	})
	if err != nil {
		return err //nolint:wrapcheck // the callback's errors are wrapped already; WalkDir adds the path to its own
	}

	var errs []error

	if len(statements) > 0 {
		errs = append(errs, fmt.Errorf("%w: %s", ErrGoStatement, strings.Join(statements, ", ")))
	}

	if len(imports) > 0 {
		errs = append(errs, fmt.Errorf("%w: %s", ErrImport, strings.Join(imports, ", ")))
	}

	return errors.Join(errs...)
}

// Refused lists the imports of file whose path allowed refuses (a path literal that does not unquote, which only a
// file that does not parse could hold, is refused), each as its position and the path as written: `a.go:3:8: "os"`.
func Refused(fset *token.FileSet, file *ast.File, allowed func(importPath string) bool) []string {
	var listed []string

	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !allowed(importPath) {
			listed = append(listed, fmt.Sprintf("%s: %s", fset.Position(imp.Path.Pos()), imp.Path.Value))
		}
	}

	return listed
}
