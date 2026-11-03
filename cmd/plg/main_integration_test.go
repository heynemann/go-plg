//go:build integration

package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const (
	contractSrc = `package contract

//go:generate go run github.com/heynemann/go-plg/cmd/plg gen Greeter

import "context"

type Greeter interface {
	Greet(ctx context.Context, name string) (string, error)
}
`
	// checkSrc registers Greeter: Register fails with ErrNoContract unless the generated init registered it.
	checkSrc = `package main

import (
	"context"
	"os"

	"github.com/heynemann/go-plg"

	"example.com/svc/contract"
)

func main() {
	ctx := context.Background()
	c := plg.New()
	err := plg.Register[contract.Greeter](ctx, c, "greeter", plg.WithStorage(plg.NewMemoryStorage("none")))
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	if err := c.Close(ctx); err != nil {
		os.Exit(1)
	}
}
`
)

// service lays out a module that uses plg like a service: a contract package with a go:generate line and a
// program registering the contract. plg is replaced by this checkout.
func service(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	gomod, err := fs.ReadFile(os.DirFS(root), "go.mod")
	require.NoError(t, err)

	gosum, err := fs.ReadFile(os.DirFS(root), "go.sum")
	require.NoError(t, err)

	// plg's own requirements, so the build list needs no network.
	modfile := strings.Replace(string(gomod), "module github.com/heynemann/go-plg", "module example.com/svc", 1) +
		"\nrequire github.com/heynemann/go-plg v0.0.0\n\nreplace github.com/heynemann/go-plg => " + root + "\n"

	dir := t.TempDir()
	files := map[string]string{
		"go.mod":               modfile,
		"go.sum":               string(gosum),
		"contract/contract.go": contractSrc,
		"cmd/check/main.go":    checkSrc,
	}

	for name, src := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
	}

	return dir
}

func goCmd(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()

	//nolint:gosec // G204: runs the go command on the test's own module
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = dir

	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// generate runs go generate in a fresh service module and returns it.
func generate(t *testing.T) string {
	t.Helper()

	dir := service(t)

	out, err := goCmd(t, dir, "generate", "./...")
	require.NoError(t, err, out)

	return dir
}

func TestGoGenerate(t *testing.T) {
	t.Parallel()

	dir := generate(t)
	generated := os.DirFS(filepath.Join(dir, "contract"))

	first, err := fs.ReadFile(generated, "greeter_plg.go")
	require.NoError(t, err)

	out, err := goCmd(t, dir, "generate", "./...")
	require.NoError(t, err, out)

	second, err := fs.ReadFile(generated, "greeter_plg.go")
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second), "generating twice gives no diff")

	out, err = goCmd(t, dir, "vet", "./...")
	require.NoError(t, err, out)

	out, err = goCmd(t, dir, "run", "./cmd/check")
	require.NoError(t, err, "the generated init registers the contract: %s", out)
}

func TestChangedInterfaceFailsTheBuildUntilRegenerated(t *testing.T) {
	t.Parallel()

	dir := generate(t)

	changed := strings.Replace(contractSrc, "}\n", "\tWave(ctx context.Context) error\n}\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "contract", "contract.go"), []byte(changed), 0o600))

	out, err := goCmd(t, dir, "build", "./...")
	require.Error(t, err)
	assert.Contains(t, out, "greeter_plg.go")
	assert.Contains(t, out, "missing method Wave")

	out, err = goCmd(t, dir, "generate", "./...")
	require.NoError(t, err, out)

	out, err = goCmd(t, dir, "build", "./...")
	require.NoError(t, err, out)
}

func TestDisallowedImportFailsGoGenerate(t *testing.T) {
	t.Parallel()

	dir := service(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "contract", "os.go"),
		[]byte("package contract\n\nimport _ \"os\"\n"), 0o600))

	out, err := goCmd(t, dir, "generate", "./...")
	require.Error(t, err)
	assert.Contains(t, out, "plg gen: the contract package imports packages the Go guest does not allow")
	assert.Contains(t, out, `os.go:3:10: "os"`)
	assert.NoFileExists(t, filepath.Join(dir, "contract", "greeter_plg.go"))
}
