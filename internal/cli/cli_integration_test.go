//go:build integration

package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// TestWarmLogsEachCompile warms the default compilation cache (which the other tests share, so this is a cache
// hit except on a machine's first run), resolved as a container resolves it. cmd/plg's integration test proves
// what warming spares a fresh process. -validate also compiles the TypeScript compiler plg validate runs.
func TestWarmLogsEachCompile(t *testing.T) {
	t.Parallel()

	def, err := engine.DefaultCacheDir()
	require.NoError(t, err)

	var stderr bytes.Buffer

	require.Equal(t, 0, plgIn([]string{"warm", "-cache-dir", def}, t.TempDir(), io.Discard, &stderr),
		stderr.String())
	assert.Contains(t, stderr.String(), `level=INFO msg="plg: guest compiled" guest=go duration=`,
		"the Go guest plg ships is compiled, and the compile logged")
	assert.NotContains(t, stderr.String(), "guest=tsgo", "the TypeScript compiler is for validate only")

	stderr.Reset()

	require.Equal(t, 0, plgIn([]string{"warm", "-validate", "-cache-dir", def}, t.TempDir(), io.Discard, &stderr),
		stderr.String())
	assert.Contains(t, stderr.String(), `level=INFO msg="plg: guest compiled" guest=tsgo duration=`)
}

// warningPlugin is a Lua plugin implementing testcontract.Fooer whose Validate finds a warning.
const warningPlugin = `local P = {}
P.__index = P
function P:Foo(name) return name end
function P:Count() return 0 end
function P:Fail(msg) end
function P:Boil(c) return { false, c } end
function P:Scalars(...) return { ... } end
function P:Spin(n) local i = 0 while i < n do i = i + 1 end return i end
function P:Validate(check)
  return { { severity = "warning", code = "p.slow", message = "slow", field = "speed" } }
end
function Create() return setmetatable({}, P) end
`

// TestStatusAndEventsOfAServingPlugin reads, through a profile without a token, a Lua plugin serving in the real
// guest whose validation finds a warning: status describes its load and its on-demand validation with their
// issues, its pool and the PanicError of a call stopped at its deadline; events lists the load, that panic with
// its message, and the validation with its issue.
func TestStatusAndEventsOfAServingPlugin(t *testing.T) {
	t.Parallel()

	ctr := plg.New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	owner := searchOwner()
	store := plg.NewMemoryStorage("rules")
	require.NoError(t, store.Put(&plg.Descriptor{
		Key: "p", Implements: "github.com/heynemann/go-plg/internal/testcontract.Fooer@v1.0.0",
		Language: plg.LanguageLua, Entrypoint: "plugin.lua", Version: "7", Owner: &owner,
	}, fstest.MapFS{"plugin.lua": {Data: []byte(warningPlugin)}}))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "lua", plg.WithStorage(store),
		plg.WithPool(1, 1), plg.WithValidation(func(context.Context, plg.PluginInfo) (testcontract.Check, error) {
			return testcontract.Check{}, nil
		})))

	fooer, err := plg.Get[testcontract.Fooer](t.Context(), ctr, "lua/p")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err = fooer.Spin(ctx, 1<<53) // the most that crosses; never reached in time

	var stopped *plg.PanicError

	require.ErrorAs(t, err, &stopped)

	_, err = fooer.Foo(t.Context(), "x") // waits for the instance replacing the stopped one: the pool settles
	require.NoError(t, err)

	validated, err := plg.Validate(t.Context(), ctr, "lua/p")
	require.NoError(t, err)
	require.NoError(t, validated.Err)

	srv := httptest.NewServer(plg.AdminHandler(ctr))
	t.Cleanup(srv.Close)

	env := &environment{ctr: ctr, srv: srv, home: t.TempDir()}
	writeProfiles(t, filepath.Join(env.home, ".config"), fmt.Sprintf("profiles:\n  - {name: local, url: %q}\n",
		srv.URL))

	state, err := plg.Info(ctr, "lua/p")
	require.NoError(t, err)

	pool := &state.Pool
	require.Equal(t, 1, pool.Instances)
	require.Equal(t, stopped, state.LastPanic)

	stamp := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	issue := []string{"", "warning p.slow (speed): slow"}
	devel := "the service's contract module github.com/heynemann/go-plg is built from a local checkout, version " +
		"(devel): the plugin's v1.0.0 is not checked"

	assert.Equal(t, [][]string{
		{"key", "lua/p"},
		{"registration", "lua"},
		{"status", "serving"},
		{"version", "7"},
		{"language", "lua"},
		{"storage", "rules"},
		{"loaded", stamp(state.LoadedAt)},
		{"team", "search"},
		{"oncall", "search-oncall"},
		{"pool", "1 instances (min 1, max 1), 0 in use, 0 calls in flight, 0 waiting"},
		{"memory", fmt.Sprintf("%d bytes (limit %d per instance)", pool.Memory, pool.MemoryLimit)},
		{"panics", "1"},
		{"last panic", stamp(stopped.Time) + " " + stopped.Error()},
		{"load", stamp(state.Load.At) + " version 7: ok"},
		{"", "warning plg.contract.devel (implements): " + devel},
		issue,
		{"validation", stamp(validated.At) + " version 7: ok"},
		issue,
	}, cells(env.ok(t, "status", "lua/p")))

	events, err := plg.Events(ctr, "lua/p")
	require.NoError(t, err)
	require.Len(t, events, 3)

	assert.Equal(t, [][]string{
		{"AT", "KIND", "VERSION", "DETAIL"},
		{stamp(events[0].At), "loaded", "7", "warning plg.contract.devel: " + devel + "; warning p.slow: slow"},
		{stamp(stopped.Time), "panicked", "7", stopped.Error()},
		{stamp(events[2].At), "validated", "7", "warning p.slow: slow"},
	}, cells(env.ok(t, "events", "lua/p")))
}
