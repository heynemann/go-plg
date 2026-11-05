//go:build integration

package plg_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/cli"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// customGoGuest is a custom Go guest with encoding/xml and example.com/greet (testdata/buildguest/greet), built
// once per test binary by `plg build-guest` as a service would build it. It is reproducible, so after the first
// run its compiled form comes from the default on-disk compilation cache like the default guest's.
//
//nolint:gochecknoglobals // one build (seconds of go build) shared by the tests of the binary
var customGoGuest = sync.OnceValues(func() ([]byte, error) {
	service, err := os.MkdirTemp("", "plg-service-")
	if err != nil {
		return nil, fmt.Errorf("custom guest: %w", err)
	}

	dir, err := os.OpenRoot(service)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("custom guest: %w", err), os.RemoveAll(service))
	}

	wasm, err := buildCustomGoGuest(dir)

	return wasm, errors.Join(err, dir.Close(), os.RemoveAll(service))
})

var errBuildGuest = errors.New("plg build-guest failed")

// buildCustomGoGuest lays out, in the directory service, a service module requiring this checkout and the greet
// module (a local replacement relative to the service's go.mod), and runs plg build-guest in it.
func buildCustomGoGuest(service *os.Root) ([]byte, error) {
	root, errRoot := filepath.Abs(".")
	gomod, errMod := os.ReadFile("go.mod")
	gosum, errSum := os.ReadFile("go.sum")

	err := errors.Join(errRoot, errMod, errSum)
	if err == nil {
		// plg's own requirements and go.sum, so the build needs no network.
		modfile := strings.Replace(string(gomod), "module github.com/heynemann/go-plg", "module example.com/svc", 1) +
			"\nrequire (\n\tgithub.com/heynemann/go-plg v0.0.0\n\texample.com/greet v0.0.0\n)\n\n" +
			"replace github.com/heynemann/go-plg => " + root + "\n\nreplace example.com/greet => ./greet\n"

		err = errors.Join(
			os.CopyFS(
				filepath.Join(service.Name(), "greet"),
				os.DirFS(filepath.Join("testdata", "buildguest", "greet")),
			),
			service.WriteFile("go.mod", []byte(modfile), 0o600),
			service.WriteFile("go.sum", gosum, 0o600),
		)
	}

	if err != nil {
		return nil, fmt.Errorf("custom guest service module: %w", err)
	}

	var stderr bytes.Buffer

	// The packages ending in _test and _linux must not drop out through file-name build constraints.
	args := []string{
		"build-guest", "-pkg", "encoding/xml", "-pkg", "example.com/greet",
		"-pkg", "example.com/greet/helper_test", "-pkg", "example.com/greet/helper_linux", "-o", "guest.wasm",
	}
	if code := cli.Main(args, service.Name(), &stderr); code != 0 {
		return nil, fmt.Errorf("%w (exit code %d): %s", errBuildGuest, code, stderr.String())
	}

	wasm, err := service.ReadFile("guest.wasm")
	if err != nil {
		return nil, fmt.Errorf("custom guest: %w", err)
	}

	return wasm, nil
}

func requireCustomGoGuest(t *testing.T) []byte {
	t.Helper()

	wasm, err := customGoGuest()
	require.NoError(t, err)

	return wasm
}

// goGuest is a Go guest the validation and stability tests run against, with the Register options selecting it.
type goGuest struct {
	name string
	opts []plg.RegisterOption
}

// goGuests returns the linked default guest and the custom guest: a custom guest must hold its plugins to the
// same validation, limits and stability.
func goGuests(t *testing.T) []goGuest {
	t.Helper()

	return []goGuest{
		{name: "default guest"},
		{name: "custom guest", opts: []plg.RegisterOption{plg.WithGoGuest(requireCustomGoGuest(t))}},
	}
}

func TestCustomGoGuestLetsPluginsImportItsPackages(t *testing.T) {
	t.Parallel()

	wasm := requireCustomGoGuest(t)

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)
	registerFooers(t, ctr, "custom", map[string]string{"xml": "xmlfooer", "ref": "fooer"}, plg.WithGoGuest(wasm))

	got, err := getFooer(t, ctr, "custom/xml").Foo(t.Context(), "ada")
	require.NoError(t, err)
	assert.Equal(t, `hello, <person name="ada"></person> / ada / test,linux`, got,
		"encoding/xml and the third-party packages, even those ending in _test and _linux, run inside the guest")

	got, err = getFooer(t, ctr, "custom/ref").Foo(t.Context(), "bob")
	require.NoError(t, err)
	assert.Equal(t, "hello, bob", got, "the default allowlist is still there")

	registerFooers(t, ctr, "again", map[string]string{"xml": "xmlfooer"}, plg.WithGoGuest(wasm))
	assert.Equal(t, 1, strings.Count(logs.String(), `"msg":"plg: guest compiled","guest":"go sha256:`),
		"Registers given the same custom guest share its compiled module")

	err = plg.Register[testcontract.Fooer](t.Context(), ctr, "default",
		plg.WithStorage(goStorage(t, "mem", map[string]string{"xml": "xmlfooer"})))
	require.ErrorIs(t, err, plg.ErrLoad, "the default guest rejects the import")
	require.ErrorContains(t, err, "encoding/xml")

	err = plg.Register[testcontract.Fooer](t.Context(), ctr, "forbidden",
		plg.WithStorage(goStorage(t, "mem", map[string]string{"os": "forbidden"})), plg.WithGoGuest(wasm))
	require.ErrorIs(t, err, plg.ErrLoad, "the custom guest rejects what it was not built with")
	require.ErrorContains(t, err, `"os"`)
}
