//go:build integration

package buildguest_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/buildguest"
)

// service returns a service module requiring this checkout (with plg's own go.sum, so no network is needed)
// and the greet test module through a version-specific replacement relative to its go.mod.
func service(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	files := os.DirFS(root)

	gomod, err := fs.ReadFile(files, "go.mod")
	require.NoError(t, err)

	gosum, err := fs.ReadFile(files, "go.sum")
	require.NoError(t, err)

	greet, err := fs.ReadFile(files, "testdata/buildguest/greet/greet.go")
	require.NoError(t, err)

	modfile := strings.Replace(string(gomod), "module github.com/heynemann/go-plg", "module example.com/svc", 1) +
		"\nrequire (\n\tgithub.com/heynemann/go-plg v0.0.0\n\texample.com/greet v0.0.0\n)\n\n" +
		"replace github.com/heynemann/go-plg => " + root + "\n\nreplace example.com/greet v0.0.0 => ./third/greet\n"

	return module(t, map[string]string{
		"go.mod":                modfile,
		"go.sum":                string(gosum),
		"third/greet/go.mod":    "module example.com/greet\n\ngo 1.27\n",
		"third/greet/greet.go":  string(greet),
		"third/greet/a-b/ab.go": "package ab\n\n// Dashed is in example.com/greet/a-b.\nfunc Dashed() string { return \"a-b\" }\n",
		"third/greet/a/b/b.go":  "package b\n\n// Nested is in example.com/greet/a/b.\nfunc Nested() string { return \"a/b\" }\n",
	})
}

func TestBuildFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want   error
		name   string
		msg    string
		output string
		pkgs   []string
	}{
		{
			name: "package of no required module", pkgs: []string{"example.com/elsewhere/pkg"},
			want: buildguest.ErrModule, msg: "no module it requires provides example.com/elsewhere/pkg",
		},
		{
			// The relative, version-specific replacement resolves from the temporary module: greet is found,
			// and only the missing package fails.
			name: "package yaegi cannot extract", pkgs: []string{"example.com/greet", "nosuch/pkg"},
			want: buildguest.ErrExtract, msg: "nosuch/pkg",
		},
		{
			// Both would extract to example_com-greet-a-b.go: the second would overwrite the first.
			name: "packages extracted to the same file",
			pkgs: []string{"example.com/greet/a-b", "example.com/greet/a/b"},
			want: buildguest.ErrPackages,
			msg:  "example.com/greet/a-b and example.com/greet/a/b to the same file, example_com-greet-a-b.go",
		},
		{
			name: "output the go command cannot write", pkgs: []string{"example.com/greet"},
			output: filepath.Join("go.mod", "guest.wasm"), want: buildguest.ErrGo, msg: "go build", // below a file
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := service(t)

			output := tc.output
			if output == "" {
				output = "guest.wasm"
			}

			err := buildguest.Build(t.Context(), buildguest.Options{Dir: dir, Output: output, Packages: tc.pkgs})
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, tc.msg)
			assert.NoFileExists(t, filepath.Join(dir, output))
		})
	}
}
