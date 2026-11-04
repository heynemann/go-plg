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
			assert.True(t, inst.Broken())
			assert.True(t, isClosed(inst.Stopped()), "the instance is stopped")

			for _, args := range [][]byte{nil, make([]byte, 64)} {
				_, err := inst.Call(context.Background(), 0, args)
				require.ErrorIs(t, err, engine.ErrStopped, "a later call, with %d bytes of arguments", len(args))
			}
		})
	}
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

// TestAStoppedInstanceEndsItsHostFunctionsAndSleeps stops an instance from its own Host method, as closing its
// runtime does from another goroutine: that method's context ends with ErrStopped, a Host method called afterwards
// gets a context ended already, a poll_oneoff afterwards returns at once instead of sleeping out its clock, and
// each call fails with ErrStopped.
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
				inst   *engine.Instance
				causes []error
			)

			opts := &engine.InstanceOptions{
				Host: func(ctx context.Context, dst []byte, _ uint32, _ []byte) ([]byte, error) {
					if len(causes) == 0 {
						engine.Stop(inst)
					}

					causes = append(causes, context.Cause(ctx))

					return append(dst, byte(wire.StatusOK)), nil
				},
			}

			_, inst = instantiateRuntime(t, tc.guest, opts)

			start := time.Now()
			_, err := inst.Call(context.Background(), 0, tc.args)
			require.ErrorIs(t, err, engine.ErrStopped)
			assert.Less(t, time.Since(start), clock/2, "nothing waited")
			assert.Len(t, causes, tc.calls)

			for _, cause := range causes {
				require.ErrorIs(t, cause, engine.ErrStopped)
			}

			assert.True(t, inst.Broken())
		})
	}
}
