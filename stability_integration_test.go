//go:build integration

package plg_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// hostileLimit is the memory limit of the hostile corpus' instances: small, so that running out of memory is
// quick and cheap.
const hostileLimit = 48 * mib

// corpus is a filesystem storage over the Go hostile corpus (testdata/hostile/go), listing only the given
// plugin directories.
func corpus(keys ...string) *plg.FSStorage {
	return plg.NewFSStorageFromFS("hostile", only{os.DirFS(filepath.Join("testdata", "hostile", "go")), keys})
}

// only is an fs.FS showing only some of its root's directories.
type only struct {
	fs.FS

	keep []string
}

func (o only) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(o.FS, name)
	if name != "." {
		return entries, err //nolint:wrapcheck // the test filesystem's own error
	}

	return slices.DeleteFunc(entries, func(e fs.DirEntry) bool { return !slices.Contains(o.keep, e.Name()) }), err
}

// requirePanic extracts the PanicError from err and checks the fields every failure has.
func requirePanic(t *testing.T, err error, key string, phase plg.Phase) *plg.PanicError {
	t.Helper()

	failure, ok := errors.AsType[*plg.PanicError](err)
	require.True(t, ok, "a PanicError: %v", err)
	assert.Equal(t, key, failure.Key)
	assert.Equal(t, phase, failure.Phase)
	assert.WithinDuration(t, time.Now(), failure.Time, time.Minute)

	return failure
}

func TestHostileGoPluginsCannotTakeTheHostDown(t *testing.T) {
	t.Parallel()

	for _, guest := range goGuests(t) {
		t.Run(guest.name, func(t *testing.T) {
			t.Parallel()

			hostileGoPluginsCannotTakeTheHostDown(t, guest.opts...)
		})
	}
}

func hostileGoPluginsCannotTakeTheHostDown(t *testing.T, opts ...plg.RegisterOption) {
	t.Helper()

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)

	opts = append(opts, plg.WithStorage(corpus("hostile")), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer", opts...))

	hostile := getFooer(t, ctr, "fooer/hostile")

	tests := []struct {
		msg      string
		guest    []string // in the guest traceback
		deadline bool
	}{
		{msg: "panic", guest: []string{
			"/gopath/src/plg.local/plugin/plugin.go:64:2: panic: plg.local/plugin.deeper(...)\n" +
				"/gopath/src/plg.local/plugin/plugin.go:60:2: panic: plg.local/plugin.explode(...)\n" +
				"/gopath/src/plg.local/plugin/plugin.go:35:9: panic: plg.local/plugin.Fail(...)\n",
		}},
		{msg: "oom", guest: []string{"fatal error: out of memory", "runtime.throw", "runtime.mallocgc"}},
		{msg: "recurse", guest: []string{"fatal error: out of memory", "runtime.newstack", "runtime.morestack"}},
		{msg: "abort", guest: []string{"fatal error: all goroutines are asleep - deadlock!", "reflect.Select"}},
		{msg: "loop", deadline: true},
	}

	for _, tc := range tests {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		if tc.deadline {
			ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
		}

		n, err := hostile.Count(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, n, "%s: a fresh instance before each failure", tc.msg)

		err = hostile.Fail(ctx, tc.msg)

		cancel()

		failure := requirePanic(t, err, "fooer/hostile", plg.PhaseCall)
		assert.Equal(t, plg.LanguageGo, failure.Language, tc.msg)
		assert.Equal(t, "Fail", failure.Method, tc.msg)
		require.ErrorIs(t, err, plg.ErrCall, tc.msg)
		assert.Nil(t, failure.Stack, "%s: a guest failure has no host stack", tc.msg)

		for _, want := range tc.guest {
			assert.Contains(t, failure.Guest, want, tc.msg)
		}

		if tc.deadline {
			require.ErrorIs(t, err, context.DeadlineExceeded, "the stop is told apart by its cause")
			assert.Empty(t, failure.Guest, "no traceback exists for a stop: the runtime reports no stack")
		} else {
			require.NotErrorIs(t, err, context.DeadlineExceeded, tc.msg)
		}

		assert.Same(t, failure, ctr.RecentPanics()[len(ctr.RecentPanics())-1], "%s: kept in the history", tc.msg)

		got, err := hostile.Foo(t.Context(), tc.msg)
		require.NoError(t, err, "%s: the host and the plugin survive", tc.msg)
		assert.Equal(t, "hello, "+tc.msg, got)
	}

	assert.Len(t, ctr.RecentPanics(), len(tests), "one failure per case, nothing else")
	assert.Contains(t, logs.String(), `"level":"ERROR","msg":"plg: plugin failure","key":"fooer/hostile",`+
		`"phase":"call"`)
}

func TestGuestOutputIsLoggedWithThePluginKey(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "chatty", plg.WithStorage(corpus("hostile")),
		plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)))

	err := getFooer(t, ctr, "chatty/hostile").Fail(t.Context(), "print")
	failure := requirePanic(t, err, "chatty/hostile", plg.PhaseCall)
	assert.Equal(t, "after printing", failure.Value)
	assert.NotContains(t, failure.Guest, "hostile says hello", "stdout is not the traceback")

	logged := logs.String()
	assert.Contains(t, logged, `"level":"INFO","msg":"plg: guest output","key":"chatty/hostile","stream":"stdout",`+
		`"line":"hostile says hello on stdout"`)
	assert.Contains(t, logged, `"msg":"plg: guest output","key":"chatty/hostile","stream":"stdout",`+
		`"line":"and through the println builtin"`)
	assert.Contains(t, logged, `"level":"WARN","msg":"plg: guest output","key":"chatty/hostile","stream":"stderr",`+
		`"line":"/gopath/src/plg.local/plugin/plugin.go:64:2: panic: plg.local/plugin.deeper(...)"`)
}

func TestGoStatementsFailRegisterWithTheirPosition(t *testing.T) {
	t.Parallel()

	err := plg.Register[testcontract.Fooer](t.Context(), newContainer(t), "fooer",
		plg.WithStorage(corpus("goroutine")))
	require.ErrorIs(t, err, plg.ErrGoStatement)
	require.ErrorIs(t, err, plg.ErrLoad)
	assert.ErrorContains(t, err, "go statements are not allowed in plugins: plugin.go:18:2, worker.go:8:2",
		"every go statement, by file, line and column")
}

func TestCreateRunningOutOfMemoryFailsRegister(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer", plg.WithStorage(corpus("createoom")),
		plg.WithMemoryLimit(hostileLimit))
	require.ErrorIs(t, err, plg.ErrLoad)

	failure := requirePanic(t, err, "fooer/createoom", plg.PhaseCreate)
	assert.Contains(t, failure.Guest, "out of memory")
	assert.Equal(t, []*plg.PanicError{failure}, ctr.RecentPanics())
}

func TestCreatePanicIsAPanicError(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer",
		plg.WithStorage(goStorage(t, "mem", map[string]string{"bad": "createpanics"})))

	failure := requirePanic(t, err, "fooer/bad", plg.PhaseCreate)
	assert.Equal(t, "Create panicked on purpose", failure.Value)
	assert.Contains(t, failure.Guest, "/gopath/src/plg.local/plugin/plugin.go:7:2: panic: plg.local/plugin.Create(...)")
}

// openPanics is a storage whose Open, or the files it returns, panic.
type openPanics struct {
	*plg.MemoryStorage

	files fs.FS // returned by Open; nil makes Open panic
}

func (s openPanics) Open(context.Context, plg.Descriptor) (fs.FS, error) {
	if s.files == nil {
		panic("open bug")
	}

	return s.files, nil
}

// readPanics is a plugin source tree whose files panic when read.
type readPanics struct {
	fstest.MapFS
}

func (readPanics) ReadFile(string) ([]byte, error) {
	panic("read bug")
}

func (r readPanics) Open(name string) (fs.File, error) {
	if name != "." {
		panic("read bug")
	}

	return r.MapFS.Open(name) //nolint:wrapcheck // the test filesystem's own error
}

func TestPanickingStorageOpenFailsRegister(t *testing.T) {
	t.Parallel()

	listed := goStorage(t, "mem", map[string]string{"ref": "fooer"})

	for _, tc := range []struct {
		store  plg.Storage
		phase  plg.Phase
		method string
		value  string
	}{
		{openPanics{MemoryStorage: listed}, plg.PhaseStorage, "Open", "open bug"},
		{
			openPanics{MemoryStorage: listed, files: readPanics{fstest.MapFS{"plugin.go": {}}}},
			plg.PhaseLoad, "", "read bug",
		},
	} {
		ctr := newContainer(t)
		err := plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer", plg.WithStorage(tc.store))
		require.ErrorIs(t, err, plg.ErrLoad)

		failure := requirePanic(t, err, "fooer/ref", tc.phase)
		assert.Equal(t, tc.method, failure.Method)
		assert.Equal(t, tc.value, failure.Value)
		assert.Contains(t, string(failure.Stack), "stability_integration_test.go", "where it panicked")

		require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer", plg.WithStorage(listed)),
			"the name was released and the container still works")
	}
}

func TestPanickingOnPanicCallbackIsContained(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	ctr := newContainer(t, plg.WithOnPanic(func(_ context.Context, failure *plg.PanicError) {
		calls.Add(1)
		panic("callback bug on " + failure.Key)
	}))
	registerFooers(t, ctr, "fooer", map[string]string{"ref": "fooer"})

	ref := getFooer(t, ctr, "fooer/ref")

	err := ref.Fail(t.Context(), "panic")
	failure := requirePanic(t, err, "fooer/ref", plg.PhaseCall)
	assert.Equal(t, "plugin panicked on purpose", failure.Value)
	assert.Equal(t, int32(1), calls.Load(), "the callback is not called for its own panic")

	recent := ctr.RecentPanics()
	require.Len(t, recent, 2)
	assert.Same(t, failure, recent[0])

	callback := recent[1]
	assert.Equal(t, plg.PhaseCallback, callback.Phase)
	assert.Equal(t, "fooer/ref", callback.Key)
	assert.Equal(t, "OnPanic", callback.Method)
	assert.Equal(t, "callback bug on fooer/ref", callback.Value)
	assert.Contains(t, string(callback.Stack), "stability_integration_test.go")

	got, err := ref.Foo(t.Context(), "still")
	require.NoError(t, err)
	assert.Equal(t, "hello, still", got)
	assert.True(t, strings.HasPrefix(failure.Error(), "plg: fooer/ref: call Fail failed: "), failure.Error())
}
