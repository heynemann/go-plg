package engine_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/wire"
)

// instantiateRuntime is instantiateWith, also returning the instance's runtime.
func instantiateRuntime(t *testing.T, g fakeGuest, opts *engine.InstanceOptions) (*engine.Runtime, *engine.Instance) {
	t.Helper()

	rt := newRuntime(t, newEngine(t, engine.Options{CacheDir: t.TempDir()}))

	mod, err := rt.Compile(t.Context(), "fake", g.wasm())
	require.NoError(t, err)

	inst, err := mod.Instantiate(t.Context(), opts)
	require.NoError(t, err)

	return rt, inst
}

// TestClosingTheRuntimeEndsWhatAnInstanceWaitsFor closes the runtime while a call, whose own context never ends,
// waits in a Host method or an HTTP request: the wait's context ends with ErrStopped as its cause, the call fails
// with ErrStopped, the instance is broken and stopped, and a later call, one growing the input buffer included,
// fails with ErrStopped too.
func TestClosingTheRuntimeEndsWhatAnInstanceWaitsFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		options func(wait func(ctx context.Context)) *engine.InstanceOptions
		name    string
		guest   fakeGuest
	}{
		{
			name:  "host_call",
			guest: hostCallGuest(slices.Concat(hostCall(), drop(), packed(0, 1))),
			options: func(wait func(ctx context.Context)) *engine.InstanceOptions {
				return &engine.InstanceOptions{
					Host: func(ctx context.Context, dst []byte, _ uint32, _ []byte) ([]byte, error) {
						wait(ctx)

						return append(dst, byte(wire.StatusOK)), nil
					},
				}
			},
		},
		{
			name:  "http_roundtrip",
			guest: hostGuest(slices.Concat(roundTrip(), drop(), drop(), packed(0, 1))),
			options: func(wait func(ctx context.Context)) *engine.InstanceOptions {
				return &engine.InstanceOptions{HTTP: func(ctx context.Context, dst, _ []byte, _ int) []byte {
					wait(ctx)

					return append(dst, byte(wire.StatusOK))
				}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entered, cause := make(chan struct{}), make(chan error, 1)
			rt, inst := instantiateRuntime(t, tc.guest, tc.options(func(ctx context.Context) {
				close(entered)
				<-ctx.Done()

				cause <- context.Cause(ctx)
			}))

			called := make(chan error, 1)

			go func() {
				_, err := inst.Call(context.Background(), 0, nil)
				called <- err
			}()

			<-entered
			require.NoError(t, rt.Close(t.Context()))

			require.ErrorIs(t, <-cause, engine.ErrStopped, "the wait's context ended with the stop")
			require.ErrorIs(t, <-called, engine.ErrStopped)
			assert.Zero(t, rt.Instances(), "the runtime closed once the call left the guest")
			assert.True(t, inst.Broken())
			assert.True(t, isClosed(inst.Stopped()), "the instance is stopped")

			for _, args := range [][]byte{nil, make([]byte, 64)} {
				_, err := inst.Call(context.Background(), 0, args)
				require.ErrorIs(t, err, engine.ErrStopped, "a later call, with %d bytes of arguments", len(args))
			}
		})
	}
}

// TestClosingTheRuntimeLeavesARunningInstanceToItsCall closes the runtime while a call, whose own context never
// ends, runs guest code that has just read the clock through WASI, as a Go guest's runtime does before it sleeps:
// the guest then spins, or sleeps for an hour. The call stops with ErrStopped and the runtime closes once it has
// left the guest, never freeing the instance's WASI state while the call's goroutine runs the guest: the race
// detector reports that as a data race with the clock's read.
func TestClosingTheRuntimeLeavesARunningInstanceToItsCall(t *testing.T) {
	t.Parallel()

	sleep := slices.Concat(i32Const(argsAt), i32Const(eventsAt), i32Const(1), i32Const(neventsAt), call(2), drop(),
		packed(0, 1))

	for _, tc := range []struct {
		name string
		then []byte
	}{
		{name: "spin", then: spin()},
		{name: "sleep", then: sleep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bodies := abiOK(0, 1)
			bodies["plg_call"] = slices.Concat(hostCall(), drop(),
				i32Const(0), i64Const(0), i32Const(answerAt), call(1), drop(), // the realtime clock, read into answerAt
				tc.then)
			guest := fakeGuest{
				data: hostData(), bodies: bodies, imports: []string{"host_call", "clock_time_get", "poll_oneoff"},
			}

			entered := make(chan struct{})
			rt, inst := instantiateRuntime(t, guest, &engine.InstanceOptions{
				Host: func(_ context.Context, dst []byte, _ uint32, _ []byte) ([]byte, error) {
					close(entered)

					return append(dst, byte(wire.StatusOK)), nil
				},
			})

			called := make(chan error, 1)

			go func() {
				_, err := inst.Call(context.Background(), 0, clockSub(1, uint64(time.Hour), 0))
				called <- err
			}()

			<-entered // the guest reads the clock next, then spins or sleeps
			require.NoError(t, rt.Close(t.Context()))
			require.ErrorIs(t, <-called, engine.ErrStopped)
			assert.Zero(t, rt.Instances(), "the runtime closed once the call left the guest")
			assert.True(t, inst.Broken())
		})
	}
}

// TestClosingTheRuntimeStopsInstantiations closes the runtime while a guest's initialization waits in a Host
// method, then instantiates the guest again: the initialization's context ends with ErrStopped and its
// instantiation fails with it, and no instance starts on the closed runtime.
func TestClosingTheRuntimeStopsInstantiations(t *testing.T) {
	t.Parallel()

	bodies := abiOK(0, 1)
	bodies["_initialize"] = slices.Concat(hostCall(), drop())
	guest := fakeGuest{data: hostData(), bodies: bodies, imports: []string{"host_call", "take_result"}}

	rt := newRuntime(t, newEngine(t, engine.Options{CacheDir: t.TempDir()}))

	mod, err := rt.Compile(t.Context(), "fake", guest.wasm())
	require.NoError(t, err)

	entered, cause := make(chan struct{}), make(chan error, 1)
	opts := &engine.InstanceOptions{Host: func(ctx context.Context, dst []byte, _ uint32, _ []byte) ([]byte, error) {
		close(entered)
		<-ctx.Done()

		cause <- context.Cause(ctx)

		return append(dst, byte(wire.StatusOK)), nil
	}}
	instantiated := make(chan error, 1)

	go func() {
		_, failed := mod.Instantiate(context.Background(), opts)
		instantiated <- failed
	}()

	<-entered
	require.NoError(t, rt.Close(t.Context()))
	require.ErrorIs(t, <-cause, engine.ErrStopped, "the initialization's context ended with the stop")
	require.ErrorIs(t, <-instantiated, engine.ErrStopped)
	assert.Zero(t, rt.Instances(), "the runtime closed once the initialization left the guest")

	_, err = mod.Instantiate(t.Context(), &engine.InstanceOptions{})
	require.ErrorIs(t, err, engine.ErrStopped, "no instance starts on a closed runtime")
}

// isClosed reports whether ch is closed.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestAStoppedInstanceEndsItsHostFunctionsAndSleeps closes an instance's runtime from the instance's own Host
// method: that method's context ends with ErrStopped, a Host method called afterwards gets a context ended
// already, a poll_oneoff afterwards returns at once instead of sleeping out its clock, each call fails with
// ErrStopped, and the runtime closes once the call has left the guest.
func TestAStoppedInstanceEndsItsHostFunctionsAndSleeps(t *testing.T) {
	t.Parallel()

	const clock = 20 * time.Second // the stop must come long before

	twice := hostCallGuest(slices.Concat(hostCall(), drop(), hostCall(), drop(), packed(0, 1)))

	tests := []struct {
		args  []byte
		name  string
		guest fakeGuest
		calls int // Host method calls
	}{
		{name: "host calls", guest: twice, calls: 2},
		{name: "a sleep", guest: pollAfterHostCall(pollOf(1)), args: clockSub(1, uint64(clock), 0), calls: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				rt     *engine.Runtime
				causes []error
			)

			opts := &engine.InstanceOptions{
				Host: func(ctx context.Context, dst []byte, _ uint32, _ []byte) ([]byte, error) {
					if len(causes) == 0 {
						require.NoError(t, rt.Close(ctx))
					}

					causes = append(causes, context.Cause(ctx))

					return append(dst, byte(wire.StatusOK)), nil
				},
			}

			rt, inst := instantiateRuntime(t, tc.guest, opts)

			start := time.Now()
			_, err := inst.Call(context.Background(), 0, tc.args)
			require.ErrorIs(t, err, engine.ErrStopped)
			assert.Less(t, time.Since(start), clock/2, "nothing waited")
			assert.Len(t, causes, tc.calls)

			for _, cause := range causes {
				require.ErrorIs(t, cause, engine.ErrStopped)
			}

			assert.True(t, inst.Broken())
			assert.Zero(t, rt.Instances(), "the runtime closed once the call left the guest")
		})
	}
}
