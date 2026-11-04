//go:build integration

package plg_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// lineWatch is a slog.Handler signalling seen each time a plugin's output line equal to line is logged.
type lineWatch struct {
	seen chan struct{}
	line string
}

func (*lineWatch) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // hugeParam: slog.Handler fixes the signature
func (w *lineWatch) Handle(_ context.Context, rec slog.Record) error {
	rec.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "line" && attr.Value.String() == w.line {
			w.seen <- struct{}{}
		}

		return true
	})

	return nil
}

func (w *lineWatch) WithAttrs([]slog.Attr) slog.Handler { return w }

func (w *lineWatch) WithGroup(string) slog.Handler { return w }

// waitKey marks the calls whose Host's Value waits for its context to end.
type waitKey struct{}

// waitingHost is a testcontract.Host whose Value, in a call marked with waitKey, signals entered and then waits
// until its context ends, returning the context's cause.
type waitingHost struct {
	*testHost

	entered chan struct{}
}

func (h *waitingHost) Value(ctx context.Context) (string, error) {
	if ctx.Value(waitKey{}) == nil {
		return h.testHost.Value(ctx)
	}

	h.entered <- struct{}{}

	<-ctx.Done()

	return "", fmt.Errorf("waited: %w", context.Cause(ctx))
}

// TestClosingStopsCallsWaitingOnTheHost closes a container, with a context that has already ended, while calls
// with a context that never ends wait on the host: a Go plugin sleeping for an hour, a plugin of each language
// waiting for an HTTP server that answers nothing within the policy's one-hour timeout (blocking, and multiplexed
// for the script languages), a plugin of each language calling a Host method that waits for its context to end,
// and a Register whose Create calls that method. Close gives up draining at once and stops them all: each call
// and the Register return ErrClosed (no PanicError, none reported), each request and Host method sees its context
// end, and Close returns.
func TestClosingStopsCallsWaitingOnTheHost(t *testing.T) {
	t.Parallel()

	// A call the fix does not stop waits until this safety deadline, which then fails the test instead of
	// hanging it.
	safety, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	// The server is released only at cleanup, before it closes, which waits for every request: until then each
	// request waits until its client gives up.
	srv := newGateServer(t)
	t.Cleanup(func() { close(srv.release) })

	policy := plg.HTTPPolicy{
		AllowedHosts: []string{hostOf(t, srv.URL)}, BlockPrivateNetworks: new(false), Timeout: time.Hour,
	}
	host := &waitingHost{testHost: newTestHost(), entered: make(chan struct{}, 4)}
	dozing := &lineWatch{seen: make(chan struct{}, 1), line: "dozing"}
	ctr := newContainer(t, plg.WithLogger(slog.New(dozing)))

	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "sleeper",
		plg.WithStorage(corpus("hostile")), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)))

	waiting := map[string]<-chan callResult{}
	waiting["sleep"] = failLater(getFooer(t, ctr, "sleeper/hostile"), "doze")

	<-dozing.seen

	for _, lang := range contractLanguages() {
		web := fetcherIn(t, lang, ctr, "web-"+lang.dir, plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit),
			plg.WithHTTP(policy))
		waiting["http "+lang.dir] = callLater(context.Background(), web, "GET "+srv.URL)

		<-srv.started

		err := plg.Register[testcontract.Hosted](t.Context(), ctr, "hosted-"+lang.dir,
			plg.WithStorage(lang.storage(t, "hosted-"+lang.dir, map[string]string{"ref": "hosted"})),
			plg.WithHost[testcontract.Host](host), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit))
		require.NoError(t, err)

		hosted, err := plg.Get[testcontract.Hosted](t.Context(), ctr, "hosted-"+lang.dir+"/ref")
		require.NoError(t, err)

		waiting["host "+lang.dir] = valueLater(hosted)

		<-host.entered
	}

	for _, lang := range scriptLanguages() {
		mux := fetcherIn(t, lang, ctr, "mux-"+lang.dir, plg.WithPool(1, 1), plg.WithConcurrentCalls(2),
			plg.WithMemoryLimit(hostileLimit), plg.WithHTTP(policy))
		waiting["multiplexed http "+lang.dir] = callLater(context.Background(), mux, "GET "+srv.URL)

		<-srv.started
	}

	creating := luaLanguage().storage(t, "creating", map[string]string{"ref": "hosted"})
	registered := make(chan callResult, 1)

	go func() {
		registered <- callResult{err: plg.Register[testcontract.Hosted](
			context.WithValue(context.Background(), waitKey{}, true), ctr, "creating", plg.WithStorage(creating),
			plg.WithHost[testcontract.Host](host), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit))}
	}()

	<-host.entered

	waiting["register"] = registered

	ended, end := context.WithCancel(t.Context())
	end()

	err := ctr.Close(ended)
	require.ErrorIs(t, err, context.Canceled, "Close gave up draining the calls")

	for name, done := range waiting {
		select {
		case got := <-done:
			require.ErrorIs(t, got.err, plg.ErrClosed, name)
			assert.NotErrorAs(t, got.err, new(*plg.PanicError), name)
		case <-safety.Done():
			assert.Fail(t, "the call was not stopped", name)
		}
	}

	for range 5 {
		select {
		case <-srv.abandoned: // every request ended with its call
		case <-safety.Done():
			require.Fail(t, "a request was not stopped")
		}
	}

	assert.Empty(t, ctr.RecentPanics(), "stopping a call is no plugin failure")
}

// failLater calls plug.Fail(context.Background(), msg) in the background.
func failLater(plug testcontract.Fooer, msg string) <-chan callResult {
	done := make(chan callResult, 1)

	go func() {
		done <- callResult{err: plug.Fail(context.Background(), msg)}
	}()

	return done
}

// valueLater calls plug.Value in the background, with a context marked with waitKey.
func valueLater(plug testcontract.Hosted) <-chan callResult {
	done := make(chan callResult, 1)

	go func() {
		out, err := plug.Value(context.WithValue(context.Background(), waitKey{}, true))
		done <- callResult{out: out, err: err}
	}()

	return done
}
