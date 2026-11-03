//go:build integration

package engine_test

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/heynemann/go-plg/guest/golang"
	"github.com/heynemann/go-plg/internal/contractsrc"
	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/wire"
)

func fooerType() reflect.Type { return reflect.TypeFor[testcontract.Fooer]() }

func goGuestWasm(t *testing.T) []byte {
	t.Helper()

	g, ok := guestreg.Lookup("go")
	require.True(t, ok, "guest/golang registers itself")

	wasm, err := g.Wasm()
	require.NoError(t, err)

	return wasm
}

// overlay is what the host mounts at /gopath for a Go plugin: contract source, plugin source, dispatcher.
func overlay(t *testing.T, plugin string) *memfs.FS {
	t.Helper()

	var tree memfs.Builder

	contract, err := contractsrc.Snapshot(os.DirFS(filepath.Join("..", "testcontract")))
	require.NoError(t, err)
	require.NoError(t, tree.AddTree(path.Join("src", fooerType().PkgPath()), contract, "."))
	require.NoError(t, tree.AddTree(path.Join("src", goabi.PluginImportPath),
		os.DirFS(filepath.Join("..", "..", "testdata", "plugins", "go", plugin)), "."))

	dispatcher, err := goabi.RenderDispatcher(fooerType(), nil)
	require.NoError(t, err)
	require.NoError(t, tree.Add(path.Join("src", goabi.DispatchImportPath, "dispatch.go"), dispatcher))

	fsys, err := tree.Build()
	require.NoError(t, err)

	return fsys
}

func initPayload(t *testing.T) []byte {
	t.Helper()

	p := goabi.InitPayload{GoPath: goabi.GoPath, Dispatch: goabi.DispatchImportPath}
	for m := range fooerType().Methods() {
		p.Methods = append(p.Methods, m.Name)
	}

	b, err := p.Append(nil)
	require.NoError(t, err)

	return b
}

// goModule compiles the real Go guest through the default on-disk cache, so only the first test run on a
// machine pays the cold compile.
func goModule(t *testing.T) *engine.Module {
	t.Helper()

	rt := newRuntime128(t, newEngine(t, engine.Options{}))

	mod, err := rt.Compile(t.Context(), "go", goGuestWasm(t))
	require.NoError(t, err)

	return mod
}

func newRuntime128(t *testing.T, e *engine.Engine) *engine.Runtime {
	t.Helper()

	rt := e.NewRuntime(t.Context(), engine.RuntimeOptions{MemoryLimit: 128 << 20})
	t.Cleanup(func() { assert.NoError(t, rt.Close(context.Background())) })

	return rt
}

func loadGoPlugin(t *testing.T, mod *engine.Module, plugin string) (*engine.Instance, engine.Result) {
	t.Helper()

	var stderr bytes.Buffer

	inst, err := mod.Instantiate(t.Context(), &engine.InstanceOptions{
		Mounts: []engine.Mount{{GuestPath: goabi.GoPath, FS: overlay(t, plugin)}},
		Stderr: &stderr,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, inst.Close(context.Background())) })

	res, err := inst.Init(t.Context(), initPayload(t))
	require.NoError(t, err, stderr.String())

	return inst, res
}

func callFoo(t *testing.T, inst *engine.Instance, name string) engine.Result {
	t.Helper()

	m, _ := fooerType().MethodByName("Foo")
	args, err := wire.AppendString(nil, name)
	require.NoError(t, err)

	res, err := inst.Call(t.Context(), uint32(m.Index), args) //nolint:gosec // G115: method index
	require.NoError(t, err)

	return res
}

func TestGoGuestInterpretsContractSource(t *testing.T) {
	t.Parallel()

	mod := goModule(t)

	for _, plugin := range []string{"fooer", "fooermain"} {
		t.Run(plugin, func(t *testing.T) {
			t.Parallel()

			inst, res := loadGoPlugin(t, mod, plugin)
			require.Equal(t, wire.StatusOK, res.Status, res.Message)

			res = callFoo(t, inst, "  wasm ")
			require.Equal(t, wire.StatusOK, res.Status, res.Message)

			r := wire.NewReader(res.Body)
			got, err := r.Text()
			require.NoError(t, err)
			require.NoError(t, r.End())

			want := testcontract.Greeting("wasm")
			if plugin == "fooermain" {
				want = "main says " + want
			}

			assert.Equal(t, want, got, "the contract's code ran interpreted inside the guest")
		})
	}
}

func TestGoGuestReportsLoadFailures(t *testing.T) {
	t.Parallel()

	mod := goModule(t)

	tests := []struct {
		plugin string
		msg    string
		status wire.Status
	}{
		{"forbidden", `"os"`, wire.StatusLoad},
		{"createfails", "database unreachable", wire.StatusError},
	}

	for _, tc := range tests {
		t.Run(tc.plugin, func(t *testing.T) {
			t.Parallel()

			inst, res := loadGoPlugin(t, mod, tc.plugin)
			assert.Equal(t, tc.status, res.Status)
			assert.Contains(t, res.Message, tc.msg)
			assert.False(t, inst.Broken(), "a load failure is an answer, not a trap")
		})
	}
}

// TestCompileCacheAcrossProcesses compiles the Go guest in two fresh processes sharing one cache directory:
// the first compiles cold, the second loads the persisted result. The helper is built without the race
// detector, which slows wazero's own Go code several-fold, so the timings are the ones a service sees.
func TestCompileCacheAcrossProcesses(t *testing.T) {
	t.Parallel()

	helper := filepath.Join(t.TempDir(), "compilecache")
	//nolint:gosec // G204: builds this package's own testdata helper
	build := exec.CommandContext(t.Context(), "go", "build", "-o", helper, "./testdata/compilecache")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	dir := t.TempDir()
	cold := compileInProcess(t, helper, dir)
	warm := compileInProcess(t, helper, dir)

	t.Logf("go guest compile: cold %v, warm %v", cold, warm)
	assert.Less(t, warm, time.Second, "a warm cache loads well under a second")
	assert.Less(t, warm*5, cold, "the second process hits the disk cache")
}

// compileInProcess runs the helper once and returns the compile duration it logged.
func compileInProcess(t *testing.T, helper, dir string) time.Duration {
	t.Helper()

	//nolint:gosec // G204: runs the helper built above
	out, err := exec.CommandContext(t.Context(), helper, dir).Output()
	require.NoError(t, err, string(out))

	compiled := regexp.MustCompile(`"msg":"plg: guest compiled",.*"duration":(\d+)`)

	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if m := compiled.FindSubmatch(sc.Bytes()); m != nil {
			ns, err := strconv.ParseInt(string(m[1]), 10, 64)
			require.NoError(t, err)

			return time.Duration(ns)
		}
	}

	require.FailNow(t, "the helper did not log its compile", string(out))

	return 0
}
