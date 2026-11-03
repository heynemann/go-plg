package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/wire"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// logBuffer collects JSON log lines.
func logBuffer() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer

	return &buf, slog.New(slog.NewJSONHandler(&buf, nil))
}

func newEngine(t *testing.T, opts engine.Options) *engine.Engine {
	t.Helper()

	e := engine.New(t.Context(), opts)
	t.Cleanup(func() { assert.NoError(t, e.Close(context.Background())) })

	return e
}

func newRuntime(t *testing.T, e *engine.Engine) *engine.Runtime {
	t.Helper()

	rt := e.NewRuntime(t.Context(), engine.RuntimeOptions{MemoryLimit: 1 << 20})
	t.Cleanup(func() { assert.NoError(t, rt.Close(context.Background())) })

	return rt
}

// instantiate compiles and instantiates a fake guest in a fresh in-memory-cached runtime.
func instantiate(t *testing.T, g fakeGuest) (*engine.Instance, error) {
	t.Helper()

	return instantiateWith(t, g, engine.InstanceOptions{})
}

func instantiateWith(t *testing.T, g fakeGuest, opts engine.InstanceOptions) (*engine.Instance, error) {
	t.Helper()

	rt := newRuntime(t, newEngine(t, engine.Options{CacheDir: t.TempDir()}))

	mod, err := rt.Compile(t.Context(), "fake", g.wasm())
	require.NoError(t, err)

	inst, err := mod.Instantiate(t.Context(), opts)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}

	return inst, nil
}

func mustInstantiate(t *testing.T, g fakeGuest) *engine.Instance {
	t.Helper()

	inst, err := instantiate(t, g)
	require.NoError(t, err)

	return inst
}

// statusMessage encodes a non-OK result buffer.
func statusMessage(s wire.Status, msg string) []byte {
	return wire.AppendMessage([]byte{byte(s)}, msg)
}

func TestCacheDirectory(t *testing.T) {
	t.Parallel()

	module := fakeGuest{bodies: abiOK(0, 1)}.wasm()

	notADir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notADir, nil, 0o600))

	blockedVersionDir := t.TempDir()
	versionDir := "wazero-" + engine.WazeroVersion() + "-" + runtime.GOARCH + "-" + runtime.GOOS
	require.NoError(t, os.WriteFile(filepath.Join(blockedVersionDir, versionDir), nil, 0o600))

	writable := filepath.Join(t.TempDir(), "nested", "cache")

	tests := []struct {
		name     string
		dir      string
		wantDir  string
		wantWarn bool
	}{
		{name: "writable directory is created and used", dir: writable, wantDir: writable},
		{name: "path below a regular file falls back to memory", dir: filepath.Join(notADir, "cache"), wantWarn: true},
		{name: "unusable version directory falls back to memory", dir: blockedVersionDir, wantWarn: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logs, logger := logBuffer()
			e := newEngine(t, engine.Options{CacheDir: tc.dir, Logger: logger})
			assert.Equal(t, tc.wantDir, e.CacheDir())

			_, err := newRuntime(t, e).Compile(t.Context(), "fake", module)
			require.NoError(t, err, "an unusable cache directory is never fatal")

			if tc.wantWarn {
				assert.Contains(t, logs.String(), `"level":"WARN"`)
				assert.Contains(t, logs.String(), `"dir":"`+tc.dir+`"`)

				return
			}

			assert.NotContains(t, logs.String(), `"level":"WARN"`)

			entries, err := os.ReadDir(filepath.Join(tc.dir, versionDir))
			require.NoError(t, err)
			assert.NotEmpty(t, entries, "the compiled module is persisted")
		})
	}
}

func TestDefaultCacheDirectory(t *testing.T) {
	t.Run("under the user cache directory", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv(engine.CacheDirEnv, "")
		t.Setenv("XDG_CACHE_HOME", root)
		t.Setenv("HOME", root)

		want := filepath.Join(root, "plg", engine.WazeroVersion())
		if runtime.GOOS == "darwin" {
			want = filepath.Join(root, "Library", "Caches", "plg", engine.WazeroVersion())
		}

		e := newEngine(t, engine.Options{})
		assert.Equal(t, want, e.CacheDir())
	})

	t.Run("PLG_CACHE_DIR wins over the user cache directory", func(t *testing.T) {
		root, env := t.TempDir(), filepath.Join(t.TempDir(), "plg-cache")
		t.Setenv(engine.CacheDirEnv, env)
		t.Setenv("XDG_CACHE_HOME", root)
		t.Setenv("HOME", root)

		assert.Equal(t, env, newEngine(t, engine.Options{}).CacheDir())

		explicit := t.TempDir()
		assert.Equal(t, explicit, newEngine(t, engine.Options{CacheDir: explicit}).CacheDir(),
			"an explicit directory wins over the environment")
	})

	t.Run("no user cache directory falls back to memory", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("unsetting the user cache directory is platform specific")
		}

		t.Setenv(engine.CacheDirEnv, "")
		t.Setenv("XDG_CACHE_HOME", "")
		t.Setenv("HOME", "")

		logs, logger := logBuffer()
		e := newEngine(t, engine.Options{Logger: logger})
		assert.Empty(t, e.CacheDir())
		assert.Contains(t, logs.String(), "user cache directory")

		_, err := engine.NewOnDisk(engine.Options{})
		require.ErrorContains(t, err, "user cache directory", "NewOnDisk does not fall back to memory")
	})
}

func TestNewOnDisk(t *testing.T) {
	t.Parallel()

	notADir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notADir, nil, 0o600))

	_, err := engine.NewOnDisk(engine.Options{CacheDir: filepath.Join(notADir, "cache")})
	require.ErrorIs(t, err, syscall.ENOTDIR, "an unusable directory is an error")
	require.ErrorContains(t, err, notADir)

	dir := filepath.Join(t.TempDir(), "nested")
	e, err := engine.NewOnDisk(engine.Options{CacheDir: dir})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, e.Close(context.Background())) })
	assert.Equal(t, dir, e.CacheDir())

	_, err = newRuntime(t, e).Compile(t.Context(), "fake", fakeGuest{bodies: abiOK(0, 1)}.wasm())
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the compiled module is persisted in its wazero version directory")
}

func TestWazeroVersionMatchesGoMod(t *testing.T) {
	t.Parallel()

	gomod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	require.NoError(t, err)

	m := regexp.MustCompile(`github\.com/tetratelabs/wazero (v\S+)`).FindSubmatch(gomod)
	require.NotNil(t, m)
	assert.Equal(t, string(m[1]), engine.WazeroVersion())
}

func TestDepVersion(t *testing.T) {
	t.Parallel()

	const path = "github.com/tetratelabs/wazero"

	tests := []struct {
		name string
		want string
		deps []*debug.Module
	}{
		{"required version", "v1.2.3", []*debug.Module{{Path: "x"}, {Path: path, Version: "v1.2.3"}}},
		{
			"replaced by a version",
			"v9",
			[]*debug.Module{{Path: path, Version: "v1", Replace: &debug.Module{Version: "v9"}}},
		},
		{
			"replaced by a directory",
			"v1",
			[]*debug.Module{{Path: path, Version: "v1", Replace: &debug.Module{Path: "../w"}}},
		},
		{"devel", "dev", []*debug.Module{{Path: path, Version: "(devel)"}}},
		{"absent", "dev", []*debug.Module{{Path: "x", Version: "v1"}}},
		{"none", "dev", nil},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.want, engine.VersionOf(tc.deps, path), tc.name)
	}
}

func TestLimitPages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		limit uint64
		want  uint32
	}{
		{0, 1 << 16},
		{1, 1},
		{engine.PageSize, 1},
		{engine.PageSize*2 - 1, 1},
		{128 << 20, 2048},
		{4 << 30, 1 << 16},
		{math.MaxUint64, 1 << 16},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.want, engine.PagesFor(tc.limit), "limit %d", tc.limit)
	}
}

func TestCompileLogsDuration(t *testing.T) {
	t.Parallel()

	logs, logger := logBuffer()
	rt := newRuntime(t, newEngine(t, engine.Options{CacheDir: t.TempDir(), Logger: logger}))

	_, err := rt.Compile(t.Context(), "fake", fakeGuest{bodies: abiOK(0, 1)}.wasm())
	require.NoError(t, err)
	assert.Regexp(t, `"msg":"plg: guest compiled","guest":"fake","duration":\d+`, logs.String())

	_, err = rt.Compile(t.Context(), "garbage", []byte("not wasm"))
	assert.ErrorContains(t, err, "compiling guest garbage")
}

func TestResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		wantMsg    string
		result     []byte
		wantBody   []byte
		wantStatus wire.Status
		wantBroken bool
	}{
		{"ok with body", "", []byte{byte(wire.StatusOK), 7, 8}, []byte{7, 8}, wire.StatusOK, false},
		{"ok without body", "", []byte{byte(wire.StatusOK)}, []byte{}, wire.StatusOK, false},
		{
			"plugin error keeps the instance",
			"boom",
			statusMessage(wire.StatusError, "boom"),
			nil,
			wire.StatusError,
			false,
		},
		{"load failure", "syntax", statusMessage(wire.StatusLoad, "syntax"), nil, wire.StatusLoad, false},
		{"panic breaks the instance", "oops", statusMessage(wire.StatusPanic, "oops"), nil, wire.StatusPanic, true},
		{"abi rejection breaks the instance", "bad", statusMessage(wire.StatusABI, "bad"), nil, wire.StatusABI, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			size := uint32(len(tc.result)) //nolint:gosec // G115: tiny
			bodies := abiOK(0, size)
			bodies["plg_close"] = packed(size, 1) // the OK status byte placed after the result
			inst := mustInstantiate(
				t,
				fakeGuest{data: slices.Concat(tc.result, []byte{byte(wire.StatusOK)}), bodies: bodies},
			)

			res, err := inst.Init(t.Context(), []byte("payload"))
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, res.Status)
			assert.Equal(t, tc.wantBody, res.Body)
			assert.Equal(t, tc.wantMsg, res.Message)
			assert.Equal(t, tc.wantBroken, inst.Broken())

			if !tc.wantBroken {
				res, err = inst.Call(t.Context(), 3, []byte("args"))
				require.NoError(t, err)
				assert.Equal(t, tc.wantStatus, res.Status)
			}

			assert.NoError(t, inst.Close(t.Context()))
		})
	}
}

func TestCloseReportsAFailingPluginClose(t *testing.T) {
	t.Parallel()

	for _, status := range []wire.Status{wire.StatusError, wire.StatusPanic} {
		t.Run(status.String(), func(t *testing.T) {
			t.Parallel()

			result := statusMessage(status, "flush failed")
			bodies := abiOK(0, 1)
			bodies["plg_close"] = packed(1, uint32(len(result))) //nolint:gosec // G115: tiny

			inst := mustInstantiate(
				t,
				fakeGuest{data: slices.Concat([]byte{byte(wire.StatusOK)}, result), bodies: bodies},
			)
			_, err := inst.Init(t.Context(), nil)
			require.NoError(t, err)

			err = inst.Close(t.Context())
			closeErr, ok := errors.AsType[*engine.CloseError](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, engine.CloseError{Status: status, Message: "flush failed"}, *closeErr)
			assert.True(t, inst.Broken(), "the instance is freed all the same")
		})
	}
}

func TestABIViolations(t *testing.T) {
	t.Parallel()

	ok := statusMessage(wire.StatusOK, "")[:1]

	with := func(name string, body, data []byte) fakeGuest {
		b := abiOK(0, 1)
		b[name] = body

		return fakeGuest{data: data, bodies: b}
	}

	tests := []struct {
		name  string
		want  string
		guest fakeGuest
	}{
		{"empty result", "invalid result buffer", fakeGuest{data: ok, bodies: abiOK(0, 0)}},
		{"result beyond memory", "invalid result buffer", fakeGuest{data: ok, bodies: abiOK(engine.PageSize-1, 2)}},
		{"zero status", "invalid status 0", fakeGuest{data: []byte{0}, bodies: abiOK(0, 1)}},
		{"unknown status", "invalid status 99", fakeGuest{data: []byte{99}, bodies: abiOK(0, 1)}},
		{
			"truncated message", wire.ErrTruncated.Error(),
			fakeGuest{data: []byte{byte(wire.StatusError), 9, 0, 0, 0, 'x'}, bodies: abiOK(0, 6)},
		},
		{
			"message with trailing bytes", wire.ErrTrailing.Error(),
			fakeGuest{data: append(statusMessage(wire.StatusError, "x"), 0), bodies: abiOK(0, 7)},
		},
		{
			"alloc returns memory it does not have",
			"out-of-range buffer",
			with("plg_alloc", i32Const(engine.PageSize), ok),
		},
		{"alloc traps", "plg_alloc", with("plg_alloc", unreachable(), ok)},
		{"init traps", "plg_init", with("plg_init", unreachable(), ok)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inst := mustInstantiate(t, tc.guest)
			_, err := inst.Init(t.Context(), []byte("payload"))
			require.ErrorContains(t, err, tc.want)
			assert.True(t, inst.Broken(), "a guest that broke the ABI is never reused")
			assert.NoError(t, inst.Close(t.Context()), "a broken instance closes without calling into the guest")
		})
	}
}

func TestMissingExports(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"plg_alloc", "plg_init", "plg_call", "plg_close"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			bodies := abiOK(0, 1)
			delete(bodies, name)

			_, err := instantiate(t, fakeGuest{data: []byte{byte(wire.StatusOK)}, bodies: bodies})
			require.ErrorIs(t, err, engine.ErrABI)
			assert.ErrorContains(t, err, name)
		})
	}
}

func TestInstantiateFailsWhenInitializationTraps(t *testing.T) {
	t.Parallel()

	bodies := abiOK(0, 1)
	bodies["_initialize"] = unreachable()

	_, err := instantiate(t, fakeGuest{bodies: bodies})
	assert.ErrorContains(t, err, "instantiating guest fake")
}

func TestInputBufferGrowsOnlyWhenNeeded(t *testing.T) {
	t.Parallel()

	bodies := abiOK(0, 1)
	bodies["plg_alloc"] = unreachable() // any growth traps

	inst := mustInstantiate(t, fakeGuest{data: []byte{byte(wire.StatusOK)}, bodies: bodies})

	_, err := inst.Call(t.Context(), 0, nil)
	require.NoError(t, err, "an empty request needs no buffer")

	_, err = inst.Call(t.Context(), 0, []byte{1})
	require.ErrorContains(t, err, "plg_alloc")
	assert.True(t, inst.Broken())
}

func TestMemoryFollowsTheGuestsGrowth(t *testing.T) {
	t.Parallel()

	grow := func(pages int32) []byte {
		return slices.Concat(i32Const(pages), []byte{0x40, 0x00}, drop(), packed(0, 1)) // memory.grow, result dropped
	}
	bodies := abiOK(0, 1)
	inst := mustInstantiate(t, fakeGuest{data: []byte{byte(wire.StatusOK)}, bodies: bodies})
	assert.Equal(t, uint64(engine.PageSize), inst.Memory(), "the module's one page")

	bodies["plg_call"] = grow(2)
	inst = mustInstantiate(t, fakeGuest{data: []byte{byte(wire.StatusOK)}, bodies: bodies})
	_, err := inst.Call(t.Context(), 0, nil)
	require.NoError(t, err)
	assert.Equal(t, uint64(3*engine.PageSize), inst.Memory())

	bodies["plg_call"] = grow(16) // the runtime's limit is 16 pages
	inst = mustInstantiate(t, fakeGuest{data: []byte{byte(wire.StatusOK)}, bodies: bodies})
	_, err = inst.Call(t.Context(), 0, nil)
	require.NoError(t, err)
	assert.Equal(t, uint64(engine.PageSize), inst.Memory(), "growth beyond the limit fails and changes nothing")
}

func TestCloseRunsPlgClose(t *testing.T) {
	t.Parallel()

	bodies := abiOK(0, 1)
	bodies["plg_close"] = unreachable()

	inst := mustInstantiate(t, fakeGuest{data: []byte{byte(wire.StatusOK)}, bodies: bodies})
	require.ErrorContains(t, inst.Close(t.Context()), "plg_close", "a healthy instance is closed through the guest")
	assert.True(t, inst.Broken(), "a closed instance is never reused")

	ended, cancel := context.WithCancel(t.Context())
	cancel()

	inst = mustInstantiate(t, fakeGuest{data: []byte{byte(wire.StatusOK)}, bodies: bodies})
	require.NoError(t, inst.Close(ended), "once its context has ended, closing frees without calling into the guest")
	assert.True(t, inst.Broken(), "a closed instance is never reused")
}

func TestCallStoppedAtDeadline(t *testing.T) {
	t.Parallel()

	bodies := abiOK(0, 1)
	bodies["plg_call"] = spin()

	inst := mustInstantiate(t, fakeGuest{data: []byte{byte(wire.StatusOK)}, bodies: bodies})

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err := inst.Call(ctx, 0, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, inst.Broken(), "a stopped instance is never reused")
}
