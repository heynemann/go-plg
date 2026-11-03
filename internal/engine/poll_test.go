package engine_test

import (
	"context"
	"encoding/binary"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/wire"
)

// Fake memory of the poll tests: plg_call stores poll_oneoff's errno at errnoAt and answers with the buffer from 0
// to pollEnd: an OK status, the errno, the event count at neventsAt and room for maxEvents events at eventsAt.
// The subscriptions are the call's arguments, which plg_alloc places at argsAt.
const (
	errnoAt   = 1
	neventsAt = 5
	eventsAt  = 9
	maxEvents = 3
	pollEnd   = eventsAt + maxEvents*32
	argsAt    = 1024 // abiOK's plg_alloc
	memEnd    = engine.PageSize
)

// WASI errnos.
const (
	errnoFault  = 21
	errnoInval  = 28
	errnoNotsup = 58
)

// pollArgs are the arguments a poll test guest passes poll_oneoff.
type pollArgs struct {
	in, out, count, nevents int32
}

// pollOf polls count subscriptions in the test guest's memory layout.
func pollOf(count int32) pollArgs {
	return pollArgs{in: argsAt, out: eventsAt, count: count, nevents: neventsAt}
}

// pollGuest returns a fake guest whose plg_call calls poll_oneoff with args and answers with the errno, the event
// count and the events.
func pollGuest(args pollArgs) fakeGuest {
	return pollingGuest(args, nil)
}

// pollAfterHostCall is pollGuest calling host_call first: the test's HostFunc then ends the call's context while
// the guest runs, right before it polls, with no loop between them where the guest could be stopped.
func pollAfterHostCall(args pollArgs) fakeGuest {
	return pollingGuest(args, []string{"host_call"})
}

// pollingGuest is a poll test guest calling each host import of first, with zero arguments, before it polls.
func pollingGuest(args pollArgs, first []string) fakeGuest {
	body := i32Const(errnoAt)
	for i := range first {
		body = slices.Concat(body, i32Const(0), i32Const(0), i32Const(0), call(i), drop())
	}

	bodies := abiOK(0, 1)
	bodies["plg_call"] = slices.Concat(body,
		i32Const(args.in), i32Const(args.out), i32Const(args.count), i32Const(args.nevents), call(len(first)),
		store32(), packed(0, pollEnd))

	return fakeGuest{data: []byte{byte(wire.StatusOK)}, bodies: bodies, imports: append(first, "poll_oneoff")}
}

// clockSub encodes a clock subscription.
func clockSub(userdata, timeout uint64, flags uint16) []byte {
	sub := make([]byte, 48)
	binary.LittleEndian.PutUint64(sub, userdata)
	binary.LittleEndian.PutUint64(sub[24:], timeout)
	binary.LittleEndian.PutUint16(sub[40:], flags)

	return sub
}

// fdReadSub encodes a subscription to stdin being readable.
func fdReadSub(userdata uint64) []byte {
	sub := make([]byte, 48)
	binary.LittleEndian.PutUint64(sub, userdata)

	sub[8] = 1 // fd_read; the descriptor at 16 is 0

	return sub
}

// event is a decoded poll_oneoff event.
type event struct {
	userdata uint64
	errno    uint16
	kind     byte
}

// polled is what poll_oneoff answered.
type polled struct {
	events  []event
	errno   uint32
	nevents uint32
}

func pollResult(t *testing.T, res engine.Result) polled {
	t.Helper()
	require.Equal(t, wire.StatusOK, res.Status, res.Message)

	body := res.Body
	got := polled{errno: binary.LittleEndian.Uint32(body), nevents: binary.LittleEndian.Uint32(body[4:])}

	for ev := range slices.Chunk(body[eventsAt-1:], 32) {
		if len(got.events) == int(min(got.nevents, maxEvents)) {
			break
		}

		got.events = append(got.events, event{
			userdata: binary.LittleEndian.Uint64(ev), errno: binary.LittleEndian.Uint16(ev[8:]), kind: ev[10],
		})
	}

	return got
}

func TestPollOneoffWaitsForTheEarliestClock(t *testing.T) {
	t.Parallel()

	const tick = 20 * time.Millisecond

	tests := []struct {
		name  string
		subs  [][]byte
		fired []uint64 // userdata of the clocks reported, in order
		wait  time.Duration
	}{
		{name: "one clock", subs: [][]byte{clockSub(7, uint64(tick), 0)}, fired: []uint64{7}, wait: tick},
		{
			name: "the earliest of several",
			subs: [][]byte{
				clockSub(1, uint64(time.Hour), 0), clockSub(2, uint64(tick), 0), clockSub(3, uint64(tick), 0),
			},
			fired: []uint64{2, 3},
			wait:  tick,
		},
		{
			name:  "a zero timeout",
			subs:  [][]byte{clockSub(4, uint64(time.Hour), 0), clockSub(5, 0, 0)},
			fired: []uint64{5},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inst := mustInstantiate(t, pollGuest(pollOf(int32(len(tc.subs))))) //nolint:gosec // G115: a few subs

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second) // ends a wait for the wrong clock
			defer cancel()

			start := time.Now()
			res, err := inst.Call(ctx, 0, slices.Concat(tc.subs...))
			elapsed := time.Since(start)

			require.NoError(t, err)

			want := polled{nevents: uint32(len(tc.fired))} //nolint:gosec // G115: a few events
			for _, userdata := range tc.fired {
				want.events = append(want.events, event{userdata: userdata})
			}

			assert.Equal(t, want, pollResult(t, res), "events for the clocks that expired, nothing else")
			assert.GreaterOrEqual(t, elapsed, tc.wait, "the poll waited for its earliest clock")
			assert.False(t, inst.Broken())
		})
	}
}

// expiring is a context whose deadline passes when the test closes it.
type expiring chan struct{}

func (expiring) Deadline() (time.Time, bool) { return time.Time{}, false }

func (e expiring) Done() <-chan struct{} { return e }

func (e expiring) Err() error {
	select {
	case <-e:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (expiring) Value(any) any { return nil }

// TestPollOneoffEndsWithTheCall: a guest waiting in poll_oneoff is stopped when its call's context ends, as a
// running guest is, instead of sleeping out its clock.
func TestPollOneoffEndsWithTheCall(t *testing.T) {
	t.Parallel()

	const clock = 20 * time.Second // the stop must come long before

	tests := []struct {
		ctx     func(t *testing.T) (context.Context, func())
		want    error
		name    string
		timeout uint64
	}{
		{
			name: "deadline", timeout: uint64(clock), want: context.DeadlineExceeded,
			ctx: func(*testing.T) (context.Context, func()) {
				ctx := make(expiring)

				return ctx, func() { close(ctx) }
			},
		},
		{
			name: "cancellation", timeout: uint64(clock), want: context.Canceled,
			ctx: func(t *testing.T) (context.Context, func()) {
				t.Helper()

				return context.WithCancel(t.Context())
			},
		},
		{
			name: "a clock beyond time.Duration", timeout: math.MaxUint64, want: context.DeadlineExceeded,
			ctx: func(t *testing.T) (context.Context, func()) {
				t.Helper()

				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				t.Cleanup(cancel)

				return ctx, func() {} // its deadline passes while the guest waits
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, end := tc.ctx(t)
			opts := engine.InstanceOptions{
				Host: func(_ context.Context, dst []byte, _ uint32, _ []byte) ([]byte, error) {
					end()

					return append(dst, byte(wire.StatusOK)), nil
				},
			}

			inst, err := instantiateWith(t, pollAfterHostCall(pollOf(1)), opts)
			require.NoError(t, err)

			start := time.Now()
			_, err = inst.Call(ctx, 0, clockSub(1, tc.timeout, 0))
			require.ErrorIs(t, err, tc.want)
			assert.Less(t, time.Since(start), clock/2, "the wait ended with the call, not at its clock")
			assert.True(t, inst.Broken(), "a stopped instance is never reused")
		})
	}
}

func TestPollOneoffRefusesWhatItCannotServe(t *testing.T) {
	t.Parallel()

	clock := clockSub(1, uint64(time.Hour), 0) // a valid clock: every refusal comes without waiting for it

	tests := []struct {
		name  string
		subs  []byte
		args  pollArgs
		errno uint32
	}{
		{name: "no subscriptions", args: pollOf(0), errno: errnoInval},
		{name: "an absolute clock", args: pollOf(1), subs: clockSub(1, 1, 1), errno: errnoNotsup},
		{name: "unknown clock flags", args: pollOf(1), subs: clockSub(1, 1, 2), errno: errnoInval},
		{name: "a file descriptor", args: pollOf(2), subs: slices.Concat(clock, fdReadSub(2)), errno: errnoNotsup},
		{
			name: "subscriptions out of memory", subs: clock, errno: errnoFault,
			args: pollArgs{in: memEnd - 47, out: eventsAt, count: 1, nevents: neventsAt},
		},
		{
			name: "events out of memory", subs: clock, errno: errnoFault,
			args: pollArgs{in: argsAt, out: memEnd - 31, count: 1, nevents: neventsAt},
		},
		{
			name: "event count out of memory", subs: clock, errno: errnoFault,
			args: pollArgs{in: argsAt, out: eventsAt, count: 1, nevents: memEnd - 3},
		},
		{
			// 2^28 subscriptions take 3×2^32 bytes and their events 2×2^32: none at all once wrapped to 32 bits.
			name: "more subscriptions than wasm32 memory holds", subs: clock, errno: errnoFault,
			args: pollArgs{in: argsAt, out: eventsAt, count: 1 << 28, nevents: neventsAt},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inst := mustInstantiate(t, pollGuest(tc.args))

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second) // ends a wait that should not happen
			defer cancel()

			res, err := inst.Call(ctx, 0, tc.subs)
			require.NoError(t, err)
			assert.Equal(t, polled{errno: tc.errno}, pollResult(t, res), "an errno, no events")
			assert.False(t, inst.Broken(), "a refused poll is an answer, not a trap")
		})
	}
}
