//go:build integration

package plg_test

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// versionedTemplate is a Go plugin whose answers carry its version: Foo(name) returns "<version>:<name>", and
// Foo("wait") asks the test API for /wait first, which answers when the test lets it. Its Close tells the test
// API when it runs; a version of "create-fails" fails Create, and "close-fails", "close-panics" and "close-hangs"
// make Close return an error, panic and never return.
const versionedTemplate = `package versioned

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/heynemann/go-plg/internal/testcontract"
)

const (
	version = "{{VERSION}}"
	api     = "{{API}}"
)

type plugin struct{ testcontract.Fooer }

func Create() (testcontract.Fooer, error) {
	if version == "create-fails" {
		return nil, errors.New("create failed on purpose")
	}

	return plugin{}, nil
}

func (plugin) Foo(_ context.Context, name string) (string, error) {
	if name != "wait" {
		return version + ":" + name, nil
	}

	body, err := get("/wait?version=" + version)

	return version + ":" + body, err
}

func (plugin) Close() error {
	switch version {
	case "close-fails":
		return errors.New("close failed on purpose")
	case "close-panics":
		panic("close panicked on purpose")
	}

	for version == "close-hangs" {
	}

	if api == "" {
		return nil
	}

	_, err := get("/closed?version=" + version)

	return err
}

func get(path string) (string, error) {
	resp, err := http.Get(api + path)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)

	return string(body), err
}
`

// versioned returns the source of the versioned plugin for version, reporting to the test API at api ("" for
// none).
func versioned(version, api string) string {
	return strings.NewReplacer("{{VERSION}}", version, "{{API}}", api).Replace(versionedTemplate)
}

// writePlugin writes a plugin directory of an FS storage: plugin.go holding source and a plugin.json without a
// version, so the plugin's version is the hash of its files.
func writePlugin(tb testing.TB, root, key, source string) {
	tb.Helper()

	dir := filepath.Join(root, key)
	require.NoError(tb, os.MkdirAll(dir, 0o750))
	require.NoError(tb, os.WriteFile(filepath.Join(dir, "plugin.json"),
		[]byte(`{"language": "go", "entrypoint": "plugin.go"}`), 0o600))
	require.NoError(tb, os.WriteFile(filepath.Join(dir, "plugin.go"), []byte(source), 0o600))
}

// putVersioned puts the versioned plugin under key in a memory storage, with version as its Version.
func putVersioned(tb testing.TB, store *plg.MemoryStorage, key, version string) {
	tb.Helper()

	desc := plg.Descriptor{Key: key, Language: plg.LanguageGo, Entrypoint: "plugin.go", Version: version}
	require.NoError(tb, store.Put(desc, fstest.MapFS{"plugin.go": {Data: []byte(versioned(version, ""))}}))
}

// eventServer is the test API of the versioned plugin. It records "wait-start:<version>", "wait-end:<version>"
// and "closed:<version>" events in the order they happen; a /wait request is answered when the test sends on
// release.
type eventServer struct {
	*httptest.Server

	events  chan string
	release chan struct{}
}

func newEventServer(t *testing.T) *eventServer {
	t.Helper()

	srv := &eventServer{events: make(chan string, 64), release: make(chan struct{})}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		version := req.URL.Query().Get("version")

		switch req.URL.Path {
		case "/wait":
			srv.events <- "wait-start:" + version

			select {
			case <-srv.release:
			case <-req.Context().Done():
				return
			}

			srv.events <- "wait-end:" + version

			_, err := io.WriteString(writer, "done")
			assert.NoError(t, err)
		case "/closed":
			srv.events <- "closed:" + version
		default:
			http.NotFound(writer, req)
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

// recorded returns the events recorded so far.
func (s *eventServer) recorded() []string {
	var events []string

	for {
		select {
		case event := <-s.events:
			events = append(events, event)
		default:
			return events
		}
	}
}

// logSink collects log lines written by any goroutine; only the test's goroutine reads them, after an event
// that orders the writes it looks for before the read.
type logSink struct {
	lines chan string
	seen  strings.Builder
}

func (s *logSink) Write(p []byte) (int, error) {
	s.lines <- string(p)

	return len(p), nil
}

// String returns every line written so far.
func (s *logSink) String() string {
	for {
		select {
		case line := <-s.lines:
			s.seen.WriteString(line)
		default:
			return s.seen.String()
		}
	}
}

// sinkContainer returns a container on the default compilation cache logging to a sink it returns.
func sinkContainer(t *testing.T, opts ...plg.Option) (*plg.Container, *logSink) {
	t.Helper()

	sink := &logSink{lines: make(chan string, 4096)}
	ctr := newContainer(t, append(opts, plg.WithLogger(slog.New(slog.NewJSONHandler(sink, nil))))...)

	return ctr, sink
}

// greet calls Foo(name) and returns its answer.
func greet(t *testing.T, fooer testcontract.Fooer, name string) string {
	t.Helper()

	got, err := fooer.Foo(t.Context(), name)
	require.NoError(t, err)

	return got
}

// TestReloadServesAnEditedPluginWhileCallsInFlightFinishOnTheOldOne edits a plugin on disk while a call runs on
// it: after the reload, new calls (through the adapter obtained before too) run the new version, the call in
// flight finishes on the old one, and the old version's Close runs after it. Close drains the same way.
func TestReloadServesAnEditedPluginWhileCallsInFlightFinishOnTheOldOne(t *testing.T) {
	t.Parallel()

	api := newEventServer(t)
	root := t.TempDir()
	writePlugin(t, root, "p", versioned("v1", api.URL))

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "edit",
		plg.WithStorage(plg.NewFSStorage(root)), plg.WithPool(1, 1), plg.WithHTTP(openPolicy(hostOf(t, api.URL)))))

	fooer := getFooer(t, ctr, "edit/p")
	inFlight := make(chan string, 1)
	wait := func() {
		got, err := fooer.Foo(context.Background(), "wait")
		assert.NoError(t, err)

		inFlight <- got
	}

	go wait()

	require.Equal(t, "wait-start:v1", <-api.events)

	writePlugin(t, root, "p", versioned("v2", api.URL))
	require.NoError(t, ctr.Reload(t.Context()))

	assert.Equal(t, "v2:x", greet(t, fooer, "x"), "the adapter obtained before the reload calls the new version")
	assert.Equal(t, "v2:y", greet(t, getFooer(t, ctr, "edit/p"), "y"))
	assert.Equal(t, 1, plg.Retired(ctr), "the old version waits for its call in flight")
	assert.Empty(t, api.recorded(), "and its Close has not run")

	api.release <- struct{}{}

	assert.Equal(t, "v1:done", <-inFlight, "the call in flight finished on the old version")
	require.NoError(t, plg.AwaitRetired(t.Context(), ctr))
	assert.Equal(t, []string{"wait-end:v1", "closed:v1"}, api.recorded(),
		"the old version's Close ran after its last call")

	go wait()

	require.Equal(t, "wait-start:v2", <-api.events)

	closed := make(chan error, 1)

	go func() { closed <- ctr.Close(context.Background()) }()

	api.release <- struct{}{}

	require.NoError(t, <-closed)
	assert.Equal(t, []string{"wait-end:v2", "closed:v2"}, api.recorded(),
		"Close returned once the call in flight had finished and the plugin's Close had run")
	assert.Equal(t, "v2:done", <-inFlight)
}

// TestReloadKeepsTheLastGoodVersionOfABrokenUpdate breaks a plugin on disk in several ways, and adds a new
// plugin broken the same way: the old version keeps serving, the new plugin stays absent, and both are logged
// and reported. A fixed update then replaces the old version.
func TestReloadKeepsTheLastGoodVersionOfABrokenUpdate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "p", versioned("v1", ""))

	ctr, logs := sinkContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "broken",
		plg.WithStorage(plg.NewFSStorage(root)), plg.WithPool(1, 1)))

	fooer := getFooer(t, ctr, "broken/p")
	good := plg.Generation(ctr, "broken/p")

	for name, source := range map[string]string{
		"syntax error":   "package versioned\n\nfunc {\n",
		"failing Create": versioned("create-fails", ""),
	} {
		writePlugin(t, root, "p", source)
		writePlugin(t, root, "q", source)

		err := ctr.Reload(t.Context())
		require.ErrorIs(t, err, plg.ErrLoad, name)
		require.ErrorContains(t, err, "broken/p", name)
		require.ErrorContains(t, err, "broken/q", name)

		assert.Same(t, good, plg.Generation(ctr, "broken/p"), name)
		assert.Equal(t, "v1:x", greet(t, fooer, "x"), "%s: the last good version keeps serving", name)

		_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, "broken/q")
		require.ErrorIs(t, err, plg.ErrNotFound, "%s: the new plugin is skipped", name)
	}

	require.NoError(t, os.WriteFile(filepath.Join(root, "p", "plugin.json"), []byte(`{"language": "go"`), 0o600))
	require.ErrorIs(t, ctr.Reload(t.Context()), plg.ErrInvalidManifest, "a storage that cannot be listed")
	assert.Same(t, good, plg.Generation(ctr, "broken/p"), "changes nothing")

	writePlugin(t, root, "p", versioned("v2", ""))
	writePlugin(t, root, "q", versioned("v2", ""))
	require.NoError(t, ctr.Reload(t.Context()))

	assert.Equal(t, "v2:x", greet(t, fooer, "x"))
	assert.Equal(t, "v2:y", greet(t, getFooer(t, ctr, "broken/q"), "y"))
	require.NoError(t, plg.AwaitRetired(t.Context(), ctr))
	assert.Equal(t, uint64(2*128*mib), plg.MemoryReserved(ctr), "failed loads gave their reservations back")

	lines := logs.String()
	assert.Equal(t, 2, strings.Count(lines, `"msg":"plg: reload kept the last good version of a plugin",`+
		`"key":"broken/p","version":"sha256:`))
	assert.Equal(t, 2, strings.Count(lines, `"msg":"plg: reload skipped a new plugin that failed to load",`+
		`"key":"broken/q"`))
	assert.Contains(t, lines, `"msg":"plg: reload failed","name":"broken"`)
	assert.Contains(t, lines, `"msg":"plg: plugin version closed","key":"broken/p","version":"sha256:`)
}

// TestReloadRemovesPlugins deletes a plugin directory: after the reload the plugin is gone, through Get, All and
// an adapter obtained earlier, its version is closed and its memory given back, and the other plugin is
// untouched. Putting the directory back serves the plugin again, through the same adapter too.
func TestReloadRemovesPlugins(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePlugin(t, root, "a", versioned("v1", ""))
	writePlugin(t, root, "b", versioned("v1", ""))

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "rm",
		plg.WithStorage(plg.NewFSStorage(root)), plg.WithPool(1, 1)))

	removed := getFooer(t, ctr, "rm/b")
	kept := plg.Generation(ctr, "rm/a")

	require.NoError(t, os.RemoveAll(filepath.Join(root, "b")))
	require.NoError(t, ctr.Reload(t.Context()))

	_, err := plg.Get[testcontract.Fooer](t.Context(), ctr, "rm/b")
	require.ErrorIs(t, err, plg.ErrNotFound)

	_, err = removed.Foo(t.Context(), "x")
	require.ErrorIs(t, err, plg.ErrNotFound, "calls through an adapter obtained earlier fail too")

	var keys []string

	for key := range plg.All[testcontract.Fooer](t.Context(), ctr) {
		keys = append(keys, key)
	}

	assert.Equal(t, []string{"rm/a"}, keys)
	assert.Same(t, kept, plg.Generation(ctr, "rm/a"), "the other plugin is untouched")

	require.NoError(t, plg.AwaitRetired(t.Context(), ctr))
	assert.Equal(t, uint64(128*mib), plg.MemoryReserved(ctr), "the removed plugin's reservation is given back")
	assert.Equal(t, uint64(128*mib), plg.MemoryInUse(ctr), "and its instance closed")

	writePlugin(t, root, "b", versioned("v2", ""))
	require.NoError(t, ctr.Reload(t.Context()))
	assert.Equal(t, "v2:x", greet(t, removed, "x"), "an adapter calls whatever plugin its key names")
}

// TestPollingFollowsTheStorage polls a directory storage: a poll that finds nothing changed leaves the plugin
// and its instances alone, and an edit is served after the next poll without any explicit reload.
func TestPollingFollowsTheStorage(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		writePlugin(t, root, "p", versioned("v1", ""))

		ctr := newContainer(t)
		require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "poll",
			plg.WithStorage(plg.NewFSStorage(root)), plg.WithPool(1, 1), plg.WithPollInterval(time.Minute)))

		fooer := getFooer(t, ctr, "poll/p")
		first := plg.Generation(ctr, "poll/p")

		time.Sleep(time.Minute)
		synctest.Wait() // the poll ran

		assert.Same(t, first, plg.Generation(ctr, "poll/p"), "an unchanged plugin is not reloaded")
		assert.Equal(t, uint64(1), instanceID(t, ctr, "poll/p"), "nor are its instances restarted")

		writePlugin(t, root, "p", versioned("v2", ""))

		time.Sleep(time.Minute)
		synctest.Wait()

		assert.Equal(t, "v2:x", greet(t, fooer, "x"), "the edit is served")
		assert.Zero(t, plg.Retired(ctr), "and the old version is closed")
	})
}

// TestReloadUnderHeavyCallTraffic reloads a plugin repeatedly while goroutines keep calling it, through one
// adapter and through Get: every call succeeds, and the versions a goroutine sees never go back, since each
// reload publishes its version in one atomic step. Every replaced version is closed in the end.
func TestReloadUnderHeavyCallTraffic(t *testing.T) {
	t.Parallel()

	const workers, minCalls, reloads = 8, 20, 6

	store := plg.NewMemoryStorage("busy")
	putVersioned(t, store, "k", "0")

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "busy", plg.WithStorage(store),
		plg.WithPool(2, 4)))

	adapter := getFooer(t, ctr, "busy/k")
	errs := make(chan error, workers+1)

	var (
		wg       sync.WaitGroup
		reloaded atomic.Bool
	)

	for worker := range workers {
		wg.Go(func() {
			err := callThroughReloads(t.Context(), ctr, adapter, worker, minCalls, &reloaded)
			if err != nil {
				errs <- err
			}
		})
	}

	for version := 1; version <= reloads; version++ {
		putVersioned(t, store, "k", strconv.Itoa(version))

		err := ctr.Reload(t.Context())
		if err != nil {
			errs <- err

			break
		}
	}

	reloaded.Store(true)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	assert.Equal(t, strconv.Itoa(reloads)+":x", greet(t, adapter, "x"))
	require.NoError(t, plg.AwaitRetired(t.Context(), ctr))
	assert.Zero(t, plg.Retired(ctr))
	assert.Equal(t, uint64(4*128*mib), plg.MemoryReserved(ctr), "only the last version holds a reservation")
}

// callThroughReloads calls Foo until the reloads are over (and at least minCalls times), on odd calls through
// a plugin it gets anew, and checks every answer: its own name, from a version no older than the last one seen.
func callThroughReloads(ctx context.Context, ctr *plg.Container, adapter testcontract.Fooer, worker, minCalls int,
	reloaded *atomic.Bool,
) error {
	last := -1

	for call := 0; call < minCalls || !reloaded.Load(); call++ {
		fooer := adapter

		if call%2 == 1 {
			var err error

			fooer, err = plg.Get[testcontract.Fooer](ctx, ctr, "busy/k")
			if err != nil {
				return fmt.Errorf("get: %w", err)
			}
		}

		name := fmt.Sprintf("w%d-c%d", worker, call)

		got, err := fooer.Foo(ctx, name)
		if err != nil {
			return fmt.Errorf("Foo(%q): %w", name, err)
		}

		prefix, ok := strings.CutSuffix(got, ":"+name)
		version, convErr := strconv.Atoi(prefix)

		if !ok || convErr != nil || version < last {
			return fmt.Errorf("%w: Foo(%q) returned %q after version %d", errUnexpected, name, got, last)
		}

		last = version
	}

	return nil
}

// TestReloadHonoursTheMemoryBudget reloads within a budget of two plugins' worst case: a version being replaced
// keeps its reservation until it is closed, so an update needs room for both, and what does not fit fails to
// load like a broken plugin.
func TestReloadHonoursTheMemoryBudget(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t, plg.WithMemoryBudget(2*128*mib))
	store := plg.NewMemoryStorage("budget")
	putVersioned(t, store, "a", "v1")
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "budget", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	putVersioned(t, store, "a", "v2")
	putVersioned(t, store, "b", "v1")

	err := plg.ReloadInterface(t.Context(), ctr, "budget")
	require.ErrorIs(t, err, plg.ErrMemoryBudget)
	require.ErrorContains(t, err, "budget/b")
	assert.Equal(t, "v2", plg.Version(ctr, "budget/a"), "the update fit beside the version it replaces")

	_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, "budget/b")
	require.ErrorIs(t, err, plg.ErrNotFound, "the new plugin did not fit")

	require.NoError(t, plg.AwaitRetired(t.Context(), ctr))
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "budget"), "once v1 is closed, it does")
	assert.Equal(t, "v1:x", greet(t, getFooer(t, ctr, "budget/b"), "x"))

	putVersioned(t, store, "a", "v3")
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "budget"), plg.ErrMemoryBudget,
		"the budget is full: no update fits")
	assert.Equal(t, "v2", plg.Version(ctr, "budget/a"), "the last good version keeps serving")
	assert.Equal(t, uint64(2*128*mib), plg.MemoryReserved(ctr))
}

// TestReloadKeepsThePreviousVersionOfADuplicateKey lists a served key in a second storage: the reload keeps the
// version served and reports the duplicate, until the key is listed once again.
func TestReloadKeepsThePreviousVersionOfADuplicateKey(t *testing.T) {
	t.Parallel()

	first, second := plg.NewMemoryStorage("first"), plg.NewMemoryStorage("second")
	putVersioned(t, first, "k", "v1")

	ctr, logs := sinkContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "dup", plg.WithStorage(first),
		plg.WithStorage(second), plg.WithPool(1, 1)))

	putVersioned(t, second, "k", "v2")
	require.ErrorIs(t, ctr.Reload(t.Context()), plg.ErrDuplicateKey)
	assert.Equal(t, "v1", plg.Version(ctr, "dup/k"))
	assert.Contains(t, logs.String(), `"msg":"plg: reload kept the last good version of a plugin","key":"dup/k",`+
		`"version":"v1"`)

	first.Delete("k")
	require.NoError(t, ctr.Reload(t.Context()))
	assert.Equal(t, "v2", plg.Version(ctr, "dup/k"))
}

func TestReloadInterfaceReloadsOnlyItsRegistration(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	stores := map[string]*plg.MemoryStorage{"one": plg.NewMemoryStorage("one"), "two": plg.NewMemoryStorage("two")}

	for name, store := range stores {
		putVersioned(t, store, "k", "v1")
		require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, name, plg.WithStorage(store),
			plg.WithPool(1, 1)))
		putVersioned(t, store, "k", "v2")
	}

	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "one"))
	assert.Equal(t, "v2", plg.Version(ctr, "one/k"))
	assert.Equal(t, "v1", plg.Version(ctr, "two/k"))

	require.NoError(t, ctr.Reload(t.Context()))
	assert.Equal(t, "v2", plg.Version(ctr, "two/k"))
}

// TestAHangingCloseCannotHoldARetiredVersion removes a plugin whose Close never returns: closing its version is
// stopped at the close timeout, reported and logged, and its memory given back.
func TestAHangingCloseCannotHoldARetiredVersion(t *testing.T) {
	t.Parallel()

	ctr, logs := sinkContainer(t)
	plg.SetCloseTimeout(ctr, 100*time.Millisecond)

	store := plg.NewMemoryStorage("hang")
	putVersioned(t, store, "k", "close-hangs")
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "hang", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	store.Delete("k")
	require.NoError(t, ctr.Reload(t.Context()))
	require.NoError(t, plg.AwaitRetired(t.Context(), ctr))

	panics := ctr.RecentPanics()
	require.Len(t, panics, 1)
	assert.Equal(t, plg.PhaseClose, panics[0].Phase)
	assert.Equal(t, "hang/k", panics[0].Key)
	require.ErrorIs(t, panics[0], context.DeadlineExceeded, "stopped at the close timeout")

	assert.Contains(t, logs.String(), `"msg":"plg: closing a plugin version failed","key":"hang/k"`)
	assert.Zero(t, plg.MemoryInUse(ctr))
	assert.Zero(t, plg.MemoryReserved(ctr))
}

// TestFailuresClosingRetiredVersionsAreReported removes plugins whose Close returns an error or panics, and one
// whose closing the user's logger panics on: an error is logged, a panic in the guest or on the host is a
// PanicError in PhaseClose, and each version is closed all the same, its memory given back.
func TestFailuresClosingRetiredVersionsAreReported(t *testing.T) {
	t.Parallel()

	sink := &logSink{lines: make(chan string, 4096)}
	logger := panickingLogger{Handler: slog.NewJSONHandler(sink, nil), msg: "plg: plugin version closed"}
	ctr := newContainer(t, plg.WithLogger(slog.New(logger)))

	store := plg.NewMemoryStorage("closes")
	versions := map[string]string{"fails": "close-fails", "panics": "close-panics", "ok": "v1"}

	for key, version := range versions {
		putVersioned(t, store, key, version)
	}

	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "x", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	for key := range versions {
		store.Delete(key)
	}

	require.NoError(t, ctr.Reload(t.Context()))
	require.NoError(t, plg.AwaitRetired(t.Context(), ctr), "every version is closed")
	assert.Zero(t, plg.MemoryInUse(ctr))
	assert.Zero(t, plg.MemoryReserved(ctr))

	panics := map[string]*plg.PanicError{}

	for _, pe := range ctr.RecentPanics() {
		assert.Equal(t, plg.PhaseClose, pe.Phase, pe.Key)

		panics[pe.Key] = pe
	}

	require.Len(t, panics, 2, "a Close returning an error is no PanicError")
	assert.Equal(t, "close panicked on purpose", panics["x/panics"].Value)
	assert.Contains(t, panics["x/panics"].Guest, "plugin.go:", "the plugin's traceback")
	assert.Equal(t, "logger exploded", panics["x/ok"].Value)
	assert.Contains(t, string(panics["x/ok"].Stack), "(*Container).closeRetired")

	lines := sink.String()
	assert.Contains(t, lines, `"msg":"plg: closing a plugin version failed","key":"x/fails","version":"close-fails",`+
		`"error":"plg: closing x/fails: closing instance 1: engine: plugin Close failed: guest status error: `+
		`close failed on purpose"`)
	assert.Contains(t, lines, `"msg":"plg: closing a plugin version failed","key":"x/panics"`)

	putVersioned(t, store, "fails", "close-fails")
	require.NoError(t, ctr.Reload(t.Context()))
	require.ErrorContains(t, ctr.Close(t.Context()), "close failed on purpose",
		"Close returns the failures of the versions it closes")
}

// TestAPanicAfterAPluginStartedClosesItsInstances loads a plugin with a logger that panics on the load's log line,
// once the plugin's instance has started: Register fails with a PanicError in PhaseLoad, and that instance is
// closed, its memory given back (and its refiller stopped, which goleak checks for the package).
func TestAPanicAfterAPluginStartedClosesItsInstances(t *testing.T) {
	t.Parallel()

	logger := panickingLogger{Handler: slog.DiscardHandler, msg: "plg: plugin loaded"}
	ctr := newContainer(t, plg.WithLogger(slog.New(logger)))

	store := plg.NewMemoryStorage("loud")
	putVersioned(t, store, "k", "v1")

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "loud", plg.WithStorage(store), plg.WithPool(1, 1))
	require.ErrorIs(t, err, plg.ErrLoad)
	assert.Equal(t, "logger exploded", requirePanic(t, err, "loud/k", plg.PhaseLoad).Value)
	assert.Zero(t, plg.MemoryInUse(ctr), "the started instance is closed")
	assert.Zero(t, plg.MemoryReserved(ctr))
}

// gatedStorage is a memory storage whose List and Open, once gated, report that they were entered. A gated List
// then waits for release, ignoring its context as a storage may; a gated Open waits for its context to end.
type gatedStorage struct {
	*plg.MemoryStorage

	entered  chan string
	release  chan struct{}
	gateList atomic.Bool
	gateOpen atomic.Bool
}

func newGatedStorage(tb testing.TB) *gatedStorage {
	tb.Helper()

	store := &gatedStorage{
		MemoryStorage: plg.NewMemoryStorage("gated"),
		entered:       make(chan string, 1),
		release:       make(chan struct{}),
	}
	putVersioned(tb, store.MemoryStorage, "k", "v1")

	return store
}

func (s *gatedStorage) List(ctx context.Context) ([]plg.Descriptor, error) {
	if s.gateList.Load() {
		s.entered <- "list"

		<-s.release
	}

	return s.MemoryStorage.List(ctx) //nolint:wrapcheck // the memory storage's own error, unchanged
}

func (s *gatedStorage) Open(ctx context.Context, desc plg.Descriptor) (fs.FS, error) {
	if s.gateOpen.Load() {
		s.entered <- "open"

		<-ctx.Done()

		return nil, fmt.Errorf("gated open: %w", context.Cause(ctx))
	}

	return s.MemoryStorage.Open(ctx, desc) //nolint:wrapcheck // the memory storage's own error, unchanged
}

// TestCloseStopsAReloadInProgress closes the container while a reload is loading a plugin: Close stops the
// load and waits for the reload, which fails with ErrClosed.
func TestCloseStopsAReloadInProgress(t *testing.T) {
	t.Parallel()

	store := newGatedStorage(t)
	ctr := plg.New()
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "closing", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	putVersioned(t, store.MemoryStorage, "k", "v2")
	store.gateOpen.Store(true)

	reloaded := make(chan error, 1)

	go func() { reloaded <- plg.ReloadInterface(context.Background(), ctr, "closing") }()

	require.Equal(t, "open", <-store.entered)
	require.NoError(t, ctr.Close(t.Context()))
	require.ErrorIs(t, <-reloaded, plg.ErrClosed)
}

// TestAReloadOutlivingCloseChangesNothing closes the container, giving up at once, while a reload is listing a
// storage that ignores its context: when the listing returns, the reload finds the container closed and
// publishes nothing.
func TestAReloadOutlivingCloseChangesNothing(t *testing.T) {
	t.Parallel()

	store := newGatedStorage(t)
	ctr := plg.New()
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "late", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	store.Delete("k")
	store.gateList.Store(true)

	reloaded := make(chan error, 1)

	go func() { reloaded <- plg.ReloadInterface(context.Background(), ctr, "late") }()

	require.Equal(t, "list", <-store.entered)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, ctr.Close(canceled), context.Canceled, "Close gave up waiting for the reload")

	store.release <- struct{}{}

	require.ErrorIs(t, <-reloaded, plg.ErrClosed, "the removal it found is not published")
}

// BenchmarkReloadUnchanged is a poll that finds nothing to do: listing a directory of three plugins (versions
// from their content hashes) and comparing them with the versions served.
func BenchmarkReloadUnchanged(b *testing.B) {
	ctr := unchangedReloads(b)

	b.ReportAllocs()

	for b.Loop() {
		err := ctr.Reload(b.Context())
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReloadUnchangedParallel asks for those reloads from many goroutines: requests made while a round runs
// are served together by the next one.
func BenchmarkReloadUnchangedParallel(b *testing.B) {
	ctr := unchangedReloads(b)

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := ctr.Reload(b.Context())
			if err != nil {
				b.Error(err)

				return
			}
		}
	})
}

// unchangedReloads returns a container serving three plugins from a directory that does not change.
func unchangedReloads(b *testing.B) *plg.Container {
	b.Helper()

	root := b.TempDir()
	for _, key := range []string{"a", "b", "c"} {
		writePlugin(b, root, key, versioned("v1", ""))
	}

	ctr := newContainer(b)
	require.NoError(b, plg.Register[testcontract.Fooer](b.Context(), ctr, "bench",
		plg.WithStorage(plg.NewFSStorage(root)), plg.WithPool(1, 1)))

	return ctr
}

// BenchmarkReloadChanged reloads a plugin whose version changes every time: the new version starts its
// instance (loading the plugin and running Create) and is published, and the old one is closed.
func BenchmarkReloadChanged(b *testing.B) {
	store := plg.NewMemoryStorage("bench")
	putVersioned(b, store, "k", "0")

	ctr := newContainer(b)
	require.NoError(b, plg.Register[testcontract.Fooer](b.Context(), ctr, "bench", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	version := 0

	b.ReportAllocs()

	for b.Loop() {
		version++
		putVersioned(b, store, "k", strconv.Itoa(version))

		err := ctr.Reload(b.Context())
		if err == nil {
			err = plg.AwaitRetired(b.Context(), ctr)
		}

		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRegister registers one Go plugin under a new name each time: listing the storage, reserving memory,
// copying and checking the source, starting the instance (running Create) and publishing it.
func BenchmarkRegister(b *testing.B) {
	store := plg.NewMemoryStorage("bench")
	putVersioned(b, store, "k", "v1")

	ctr := newContainer(b)
	name := 0

	b.ReportAllocs()

	for b.Loop() {
		name++

		err := plg.Register[testcontract.Fooer](b.Context(), ctr, "bench"+strconv.Itoa(name), plg.WithStorage(store),
			plg.WithPool(1, 1))
		if err != nil {
			b.Fatal(err)
		}
	}
}
