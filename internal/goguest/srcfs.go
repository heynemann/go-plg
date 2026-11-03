package goguest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"slices"
)

// sourceFS is where the interpreter reads Go sources from: the real file system (the host's read-only mount
// inside the guest), with absolute names as yaegi builds them from GoPath.
//
// Go files are rewritten by nameParams on the way in, to work around yaegi v0.16.1 mis-binding the frame slots
// of methods with unnamed parameters (`func (f *T) M(context.Context) (int, error)` reads its receiver from
// the wrong slot and panics). Naming such parameters `_` is equivalent Go and avoids the bug.
type sourceFS struct{}

var _ fs.ReadFileFS = sourceFS{}

func (sourceFS) Open(name string) (fs.File, error) {
	f, err := os.Open(name) //nolint:gosec // G304: the interpreter resolves names inside the read-only mount
	if err != nil {
		return nil, fmt.Errorf("goguest: %w", err)
	}

	return f, nil
}

func (sourceFS) ReadFile(name string) ([]byte, error) {
	src, err := os.ReadFile(name) //nolint:gosec // G304: as Open
	if err != nil {
		return nil, fmt.Errorf("goguest: %w", err)
	}

	if path.Ext(name) != ".go" {
		return src, nil
	}

	return nameParams(src), nil
}

// nameParams names every unnamed parameter of every function declaration and function literal `_`. Only
// "_ " is inserted, so line numbers are unchanged. Source that does not parse is returned as is, for the
// interpreter to report.
func nameParams(src []byte) []byte {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return src
	}

	var offsets []int

	ast.Inspect(file, func(n ast.Node) bool {
		var params *ast.FieldList

		switch fn := n.(type) {
		case *ast.FuncDecl:
			params = fn.Type.Params
		case *ast.FuncLit:
			params = fn.Type.Params
		default:
			return true
		}

		for _, field := range params.List {
			if len(field.Names) == 0 {
				offsets = append(offsets, fset.Position(field.Type.Pos()).Offset)
			}
		}

		return true
	})

	if len(offsets) == 0 {
		return src
	}

	slices.Sort(offsets)

	out := make([]byte, 0, len(src)+2*len(offsets))
	prev := 0

	for _, off := range offsets {
		out = append(append(out, src[prev:off]...), "_ "...)
		prev = off
	}

	return append(out, src[prev:]...)
}
