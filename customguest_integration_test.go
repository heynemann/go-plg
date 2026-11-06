//go:build integration

package plg_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/cli"
	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// prepareGuests runs before the tests (TestMain). It compiles every guest they load into the default on-disk
// compilation cache, the one their containers use: the guests the binary links (plg.Warm; `make test` has already
// compiled them without the race detector, so these are cache hits there) and, meanwhile, the custom Go guest once
// built. A cold cache (a fresh machine, or CI whenever a guest artifact changes the cache key) then costs one
// compile per guest instead of one per container that loads it first: the three tests sharing the custom guest
// used to compile it cold concurrently, each for a minute and more under the race detector, which took the
// package past go test's 10-minute default timeout on CI (PLAN.md, "Test time").
func prepareGuests() error {
	warmed := make(chan error, 1)

	go func() { warmed <- plg.Warm(compileOnEveryCore()) }()

	_, err := customGoGuest()
	if err != nil {
		// The tests using the custom guest fail with this error; the others do not depend on it.
		fmt.Fprintln(os.Stderr, "plg tests: building the custom Go guest:", err)
	}

	return <-warmed
}

// compileOnEveryCore returns a context compiling a guest on every core (a wazero experimental option): only
// prepareGuests uses it, the containers of the tests compile as plg does.
func compileOnEveryCore() context.Context {
	return experimental.WithCompilationWorkers(context.Background(), runtime.GOMAXPROCS(0))
}

// customGoGuest is a custom Go guest with encoding/xml and example.com/greet (testdata/buildguest/greet), built
// once per test binary by `plg build-guest` as a service would build it, then compiled once into the default
// on-disk compilation cache, where the containers of the tests using it find it. It is reproducible, so after
// the first run on a machine that compile is a cache hit too.
//
//nolint:gochecknoglobals // one build (seconds of go build) and one compile shared by the tests of the binary
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

	err = errors.Join(err, dir.Close(), os.RemoveAll(service))
	if err != nil {
		return nil, err
	}

	return wasm, compileIntoDefaultCache(compileOnEveryCore(), wasm)
})

// compileIntoDefaultCache compiles a guest into the default on-disk compilation cache, as plg.Warm does the
// linked guests.
func compileIntoDefaultCache(ctx context.Context, wasm []byte) error {
	eng, err := engine.NewOnDisk(engine.Options{})
	if err != nil {
		return fmt.Errorf("custom guest: %w", err)
	}

	rt := eng.NewRuntime(ctx, engine.RuntimeOptions{})
	_, err = rt.Compile(ctx, "custom go", wasm)

	return errors.Join(err, rt.Close(ctx), eng.Close(ctx))
}

var errBuildGuest = errors.New("plg build-guest failed")

// buildCustomGoGuest lays out, in the directory service, a service module requiring this checkout and the greet
// module (testdata/buildguest/greet, a local replacement relative to the service's go.mod), and runs plg
// build-guest in it. The build records each replacement's directory, which build-guest makes absolute: the greet
// module stays where it is, so the guest is the same bytes whatever the service directory, and its compiled form
// a cache hit after the first run on a machine.
func buildCustomGoGuest(service *os.Root) ([]byte, error) {
	err := layOutService(service)
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

// layOutService writes the service module's go.mod and go.sum: plg's own requirements and go.sum (so the build
// needs no network), plg replaced by this checkout and greet by its directory, relative to the service's.
func layOutService(service *os.Root) error {
	root, errRoot := filepath.Abs(".")
	gomod, errMod := os.ReadFile("go.mod")
	gosum, errSum := os.ReadFile("go.sum")
	greet, errGreet := filepath.Rel(service.Name(), filepath.Join(root, "testdata", "buildguest", "greet"))

	err := errors.Join(errRoot, errMod, errSum, errGreet)
	if err != nil {
		return err
	}

	modfile := strings.Replace(string(gomod), "module github.com/heynemann/go-plg", "module example.com/svc", 1) +
		"\nrequire (\n\tgithub.com/heynemann/go-plg v0.0.0\n\texample.com/greet v0.0.0\n)\n\n" +
		"replace github.com/heynemann/go-plg => " + root + "\n\n" +
		"replace example.com/greet => " + filepath.ToSlash(greet) + "\n"

	return errors.Join(
		service.WriteFile("go.mod", []byte(modfile), 0o600),
		service.WriteFile("go.sum", gosum, 0o600),
	)
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
