package cli_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/cli"
	"github.com/heynemann/go-plg/internal/contractsrc"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const fooer = "package c\n\nimport \"context\"\n\ntype Fooer interface{ Foo(ctx context.Context) error }\n"

func contractDir(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, src := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
	}

	return dir
}

func TestGen(t *testing.T) {
	t.Parallel()

	dir := contractDir(t, map[string]string{"c.go": fooer})

	var stderr bytes.Buffer

	require.Equal(t, 0, cli.Main([]string{"gen", "Fooer"}, dir, &stderr))
	assert.Empty(t, stderr.String(), "gen is silent on success")

	src, err := fs.ReadFile(os.DirFS(dir), "fooer_plg.go")
	require.NoError(t, err)
	assert.True(t, contractsrc.IsGenerated(src))
	assert.Contains(t, string(src), "type plgFooer struct {")
	assert.Contains(t, string(src), "type plgFooerFast struct {", "fast paths by default")

	require.Equal(t, 0, cli.Main([]string{"gen", "-fast=false", "Fooer"}, dir, &stderr))

	src, err = fs.ReadFile(os.DirFS(dir), "fooer_plg.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), "type plgFooer struct {")
	assert.NotContains(t, string(src), "plgFooerFast", "-fast=false leaves them out")
}

func TestErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		files map[string]string
		name  string
		// prefix starts stderr; contains is somewhere in it.
		prefix, contains string
		args             []string
		code             int
	}{
		{name: "no command", code: 2, prefix: "usage: plg <command>"},
		{name: "unknown command", args: []string{"warp"}, code: 2, prefix: "plg: unknown command \"warp\"\n\nusage:"},
		{
			name:   "gen without interfaces",
			args:   []string{"gen"},
			code:   2,
			prefix: "plg gen: name at least one interface",
		},
		{name: "gen help", args: []string{"gen", "-h"}, code: 2, prefix: "usage: plg <command>"},
		{
			name: "gen unknown flag", args: []string{"gen", "-generic", "Fooer"}, code: 2,
			prefix: "plg gen: flag provided but not defined: -generic\n\nusage:",
		},
		{
			name:   "gen flags without interfaces",
			args:   []string{"gen", "-fast=false"},
			code:   2,
			prefix: "plg gen: name at least one interface",
		},
		{
			name: "gen failure", args: []string{"gen", "Fooer"}, code: 1,
			files:  map[string]string{"c.go": fooer, "os.go": "package c\n\nimport _ \"os\"\n"},
			prefix: "plg gen: the contract package imports packages the Go guest does not allow", contains: `"os"`,
		},
		{
			name: "refused overwrite", args: []string{"gen", "Fooer"}, code: 1,
			files:  map[string]string{"c.go": fooer, "fooer_plg.go": "package c\n"},
			prefix: "plg gen: refusing to overwrite a file plg gen did not generate", contains: "fooer_plg.go",
		},
		{name: "help", args: []string{"warm", "-h"}, code: 2, prefix: "usage: plg <command>"},
		{
			name: "warm unknown flag", args: []string{"warm", "-x"}, code: 2,
			prefix: "plg warm: flag provided but not defined: -x\n\nusage:",
		},
		{
			name: "warm arguments", args: []string{"warm", "extra"}, code: 2,
			prefix: "plg warm: unexpected arguments: extra\n\nusage:",
		},
		{
			// The relative directory is resolved against the command's directory, not the process's.
			name: "warm unusable cache", args: []string{"warm", "-cache-dir", "file/cache"}, code: 1,
			files:  map[string]string{"file": ""},
			prefix: "plg: warm: compilation cache directory /", contains: "/file/cache: creating cache directory",
		},
		{
			name: "build-guest without packages", args: []string{"build-guest", "-o", "g.wasm"}, code: 2,
			prefix: "plg build-guest: name at least one package with -pkg\n\nusage:",
		},
		{
			name: "build-guest without output", args: []string{"build-guest", "-pkg", "encoding/xml"}, code: 2,
			prefix: "plg build-guest: name the output file with -o\n\nusage:",
		},
		{
			name: "build-guest arguments", args: []string{"build-guest", "-pkg", "encoding/xml", "-o", "g", "x"},
			code: 2, prefix: "plg build-guest: unexpected arguments: x\n\nusage:",
		},
		{
			name: "build-guest failure", args: []string{"build-guest", "-pkg", "regexp", "-o", "g.wasm"}, code: 1,
			prefix: "plg build-guest: invalid extra packages: regexp is in the default allowlist already\n",
		},
		{
			name: "build-guest unknown flag", args: []string{"build-guest", "-pkgs", "x"}, code: 2,
			prefix: "plg build-guest: flag provided but not defined: -pkgs\n\nusage:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer

			assert.Equal(t, tc.code, cli.Main(tc.args, contractDir(t, tc.files), &stderr))
			assert.True(t, strings.HasPrefix(stderr.String(), tc.prefix), stderr.String())
			assert.Contains(t, stderr.String(), tc.contains)
			assert.True(t, strings.HasSuffix(stderr.String(), "\n"), "the message ends its line")
		})
	}
}

var errClosed = errors.New("stderr closed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errClosed }

func TestUnwritableStderrStillFails(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 1, cli.Main(nil, t.TempDir(), failingWriter{}))
	assert.Equal(t, 0, cli.Main([]string{"gen", "Fooer"}, contractDir(t, map[string]string{"c.go": fooer}),
		failingWriter{}), "success writes nothing")
}
