package buildguest_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/buildguest"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// module writes a module with the given files into a fresh directory and returns it.
func module(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, src := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
	}

	return dir
}

func TestBuildRejectsItsArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want error
		name string
		msg  string
		dir  string
		pkgs []string
	}{
		{name: "no packages", want: buildguest.ErrPackages, msg: "name at least one package"},
		{name: "empty path", pkgs: []string{""}, want: buildguest.ErrPackages, msg: `"" is not an import path`},
		{name: "relative path", pkgs: []string{"./x"}, want: buildguest.ErrPackages, msg: `"./x" is not`},
		{name: "absolute path", pkgs: []string{"/x"}, want: buildguest.ErrPackages, msg: `"/x" is not`},
		{
			name: "repeated", pkgs: []string{"encoding/xml", "encoding/xml"}, want: buildguest.ErrPackages,
			msg: "encoding/xml is listed twice",
		},
		{
			name: "allowlisted", pkgs: []string{"encoding/xml", "strings"}, want: buildguest.ErrPackages,
			msg: "strings is in the default allowlist already",
		},
		{
			name: "outside a module", pkgs: []string{"encoding/xml"}, dir: "none",
			want: buildguest.ErrModule, msg: "is not inside a Go module",
		},
		{
			name: "without plg", pkgs: []string{"encoding/xml"}, dir: "plain", want: buildguest.ErrModule,
			msg: "it does not require github.com/heynemann/go-plg",
		},
		{
			name: "missing directory", pkgs: []string{"encoding/xml"}, dir: "missing", want: buildguest.ErrGo,
			msg: "go env GOMOD",
		},
		{
			name: "unreadable go.sum", pkgs: []string{"encoding/xml"}, dir: "sumdir",
			want: syscall.EISDIR, msg: "copying the service's go.sum",
		},
		{
			name: "malformed go.mod", pkgs: []string{"encoding/xml"}, dir: "malformed", want: buildguest.ErrGo,
			msg: "go mod edit -json: exit status 1\ngo: errors parsing go.mod",
		},
		{
			// The relative replacement is made absolute for the temporary module, and still names no module.
			name: "missing replacement", pkgs: []string{"encoding/xml"}, dir: "replaced", want: buildguest.ErrGo,
			msg: "go list -m -f {{.Path}} all: exit status 1",
		},
	}

	replaced := module(t, map[string]string{"go.mod": "module example.com/replaced\n\ngo 1.27\n\n" +
		"require example.com/y v0.0.0\n\nreplace example.com/y => ./y\n"})

	dirs := map[string]string{
		"none":      t.TempDir(),
		"plain":     module(t, map[string]string{"go.mod": "module example.com/plain\n\ngo 1.27\n"}),
		"missing":   filepath.Join(t.TempDir(), "missing"),
		"sumdir":    module(t, map[string]string{"go.mod": "module example.com/s\n\ngo 1.27\n", "go.sum/x": ""}),
		"malformed": module(t, map[string]string{"go.mod": "module example.com/m\n\ngarbage\n"}),
		"replaced":  replaced,
	}

	tests[len(tests)-1].msg += "\ngo: example.com/y@v0.0.0 (replaced by " + filepath.Join(replaced, "y") + ")"

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			output := filepath.Join(t.TempDir(), "guest.wasm")
			err := buildguest.Build(
				t.Context(),
				buildguest.Options{Dir: dirs[tc.dir], Output: output, Packages: tc.pkgs},
			)
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, tc.msg)
			assert.NoFileExists(t, output)
		})
	}
}

func TestBuildNeedsATemporaryDirectory(t *testing.T) {
	dir := module(t, map[string]string{"go.mod": "module example.com/plain\n\ngo 1.27\n"})
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))

	err := buildguest.Build(t.Context(), buildguest.Options{
		Dir: dir, Output: "guest.wasm", Packages: []string{"encoding/xml"},
	})
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.ErrorContains(t, err, "creating the build directory")
}
