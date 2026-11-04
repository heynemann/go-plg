package engine_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/httpabi"
	"github.com/heynemann/go-plg/internal/wire"
)

// Fake memory of the step tests: a step result at 0, an empty one (no call finished) at emptyAt and a four-byte
// HTTP request at requestAt.
const (
	emptyAt   = 128
	requestAt = 192
)

// finished is a call a fake step finished: its number and result buffer.
type finished struct {
	result []byte
	call   uint32
}

// stepResult encodes a step result listing finished calls.
func stepResult(done ...finished) []byte {
	out := binary.LittleEndian.AppendUint32([]byte{byte(wire.StatusOK)}, uint32(len(done))) //nolint:gosec // G115: tiny

	for _, call := range done {
		out = binary.LittleEndian.AppendUint32(out, call.call)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(call.result))) //nolint:gosec // G115: tiny
		out = append(out, call.result...)
	}

	return out
}

// stepGuest is a multiplexing fake guest (http_start is function 0): plg_start answers result after running
// startBody, plg_resume answers the empty step result.
func stepGuest(result, startBody []byte) fakeGuest {
	data := make([]byte, requestAt+4)
	copy(data, result)
	copy(data[emptyAt:], stepResult())
	copy(data[requestAt:], "REQ1")

	bodies := abiOK(0, 1)
	bodies["plg_start"] = slices.Concat(startBody, packed(0, uint32(len(result)))) //nolint:gosec // G115: tiny
	bodies["plg_resume"] = packed(emptyAt, uint32(len(stepResult())))              //nolint:gosec // G115: tiny

	return fakeGuest{data: data, bodies: bodies, imports: []string{"http_start"}}
}

// startRequest starts the request at requestAt for call 9.
func startRequest() []byte {
	return slices.Concat(i32Const(9), i32Const(requestAt), i32Const(4), call(0), drop())
}

func TestStepsReportFinishedCallsAndStartedRequests(t *testing.T) {
	t.Parallel()

	result := stepResult(
		finished{call: 9, result: []byte{byte(wire.StatusOK), 42}},
		finished{call: 3, result: statusMessage(wire.StatusError, "x")},
	)
	g := stepGuest(result, startRequest())
	inst, err := instantiateWith(t, g, &engine.InstanceOptions{HTTP: refuseAll})
	require.NoError(t, err)
	require.True(t, inst.Multiplexes())

	step, err := inst.Start(t.Context(), 9, 2, []byte("args"))
	require.NoError(t, err)
	assert.Equal(t, engine.Step{
		Done: []engine.Completion{
			{Call: 9, Result: engine.Result{Status: wire.StatusOK, Body: []byte{42}}},
			{Call: 3, Result: engine.Result{Status: wire.StatusError, Message: "x"}},
		},
		Requests: []engine.Request{{Call: 9, ID: 1, N: 1, Body: []byte("REQ1")}},
	}, step)

	step, err = inst.Start(t.Context(), 10, 0, nil)
	require.NoError(t, err)
	assert.Equal(t, []engine.Request{{Call: 9, ID: 2, N: 2, Body: []byte("REQ1")}}, step.Requests,
		"requests are numbered across steps, and counted per call")

	previous := step.Requests

	step, err = inst.Resume(t.Context(), 1, []byte("answer"))
	require.NoError(t, err)
	assert.Empty(t, step.Done)
	assert.Empty(t, step.Requests, "a step reports only the requests it started")
	assert.Equal(t, engine.Request{}, previous[0], "nor keeps those of the previous step")
	assert.False(t, inst.Broken())
	require.NoError(t, inst.Close(t.Context()))
}

// refuseAll is an HTTPFunc refusing every request with a message naming its size and number in its call.
func refuseAll(_ context.Context, dst, req []byte, n int) []byte {
	return httpabi.AppendFailure(dst, fmt.Sprintf("refused %d bytes, request %d", len(req), n))
}

func TestHTTPStartRefusesAtOnceWhatHTTPRefuses(t *testing.T) {
	t.Parallel()

	twice := slices.Concat(startRequest(), startRequest())
	tests := []struct {
		refuses func(n, size int) bool
		http    engine.HTTPFunc
		name    string
		want    []engine.Request
	}{
		{
			name: "HTTP disabled",
			want: []engine.Request{
				{Call: 9, ID: 1, N: 1, Answer: httpabi.AppendFailure(nil, engine.ErrHTTPDisabled.Error())},
				{Call: 9, ID: 2, N: 2, Answer: httpabi.AppendFailure(nil, engine.ErrHTTPDisabled.Error())},
			},
		},
		{
			name: "past a count", http: refuseAll, refuses: func(n, _ int) bool { return n > 1 },
			want: []engine.Request{
				{Call: 9, ID: 1, N: 1, Body: []byte("REQ1")},
				{Call: 9, ID: 2, N: 2, Answer: httpabi.AppendFailure(nil, "refused 4 bytes, request 2")},
			},
		},
		{
			name: "over a size", http: refuseAll, refuses: func(_, size int) bool { return size > 3 },
			want: []engine.Request{
				{Call: 9, ID: 1, N: 1, Answer: httpabi.AppendFailure(nil, "refused 4 bytes, request 1")},
				{Call: 9, ID: 2, N: 2, Answer: httpabi.AppendFailure(nil, "refused 4 bytes, request 2")},
			},
		},
		{
			name: "nothing refused", http: refuseAll, refuses: func(int, int) bool { return false },
			want: []engine.Request{
				{Call: 9, ID: 1, N: 1, Body: []byte("REQ1")},
				{Call: 9, ID: 2, N: 2, Body: []byte("REQ1")},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inst, err := instantiateWith(t, stepGuest(stepResult(), twice),
				&engine.InstanceOptions{HTTP: tc.http, HTTPRefuses: tc.refuses})
			require.NoError(t, err)

			step, err := inst.Start(t.Context(), 9, 0, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, step.Requests)
		})
	}
}

func TestHTTPStartOnlyForCallsInFlight(t *testing.T) {
	t.Parallel()

	inst := mustInstantiate(t, stepGuest(stepResult(), startRequest()))

	_, err := inst.Start(t.Context(), 9, 0, nil)
	require.NoError(t, err)

	inst.EndCall(9)

	_, err = inst.Start(t.Context(), 10, 0, nil) // its step starts a request of call 9, ended
	require.ErrorIs(t, err, engine.ErrABI)
	require.ErrorContains(t, err, "http_start for call 9, not in flight")
	assert.True(t, inst.Broken())
}

func TestGuestsWithoutStepsDoNotMultiplex(t *testing.T) {
	t.Parallel()

	inst := mustInstantiate(t, fakeGuest{data: []byte{byte(wire.StatusOK)}, bodies: abiOK(0, 1)})
	assert.False(t, inst.Multiplexes())
}

func TestDropForgetsACall(t *testing.T) {
	t.Parallel()

	g := stepGuest(stepResult(), nil)
	g.bodies["plg_drop"] = packed(emptyAt, 1) // OK, with no body
	inst := mustInstantiate(t, g)

	require.NoError(t, inst.Drop(t.Context(), 9))
	assert.False(t, inst.Broken())
	require.NoError(t, inst.Close(t.Context()))
}

func TestDropFailures(t *testing.T) {
	t.Parallel()

	refusal := statusMessage(wire.StatusABI, "no such call waits")
	tests := []struct {
		drop   []byte // plg_drop's body; nil: no plg_drop export
		name   string
		want   string
		result []byte
		abi    bool // an ABI violation, not the guest's own failure
	}{
		{name: "no export", want: "cannot drop call 9", abi: true},
		{
			name: "refused", want: "no such call waits", abi: true,
			result: refusal, drop: packed(0, uint32(len(refusal))), //nolint:gosec // G115: tiny
		},
		{name: "answer with a body", want: "plg_drop of call 9", drop: packed(emptyAt, 5), abi: true},
		{name: "trap", want: "plg_drop", drop: unreachable()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			g := stepGuest(tc.result, nil)
			if tc.drop != nil {
				g.bodies["plg_drop"] = tc.drop
			}

			inst := mustInstantiate(t, g)

			err := inst.Drop(t.Context(), 9)
			require.ErrorContains(t, err, tc.want)

			if tc.abi {
				require.ErrorIs(t, err, engine.ErrABI)
			} else {
				require.NotErrorIs(t, err, engine.ErrABI)
			}

			assert.True(t, inst.Broken(), "an instance whose drop failed is never reused")
		})
	}
}

func TestStepFailures(t *testing.T) {
	t.Parallel()

	ok := []byte{byte(wire.StatusOK)}
	header := func(count uint32) []byte {
		return binary.LittleEndian.AppendUint32(slices.Clone(ok), count)
	}

	tests := []struct {
		name   string
		want   string
		result []byte
		body   []byte
	}{
		{name: "truncated count", want: "malformed step result", result: append(slices.Clone(ok), 0, 0)},
		{name: "count beyond the body", want: "malformed step result", result: header(1)},
		{
			name: "empty call result", want: "malformed step result",
			result: binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(header(1), 9), 0),
		},
		{
			name: "call result beyond the body", want: "malformed step result",
			result: append(binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(header(1), 9), 5), 1),
		},
		{
			name: "invalid call status", want: "invalid status 99",
			result: stepResult(finished{call: 9, result: []byte{99}}),
		},
		{name: "trailing bytes", want: "1 trailing bytes", result: append(header(0), 7)},
		{name: "trap", want: "plg_start", result: header(0), body: unreachable()},
		{
			name: "request beyond memory", want: "http_start request out of range", result: header(0),
			body: slices.Concat(i32Const(9), i32Const(engine.PageSize-2), i32Const(4), call(0), drop()),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inst := mustInstantiate(t, stepGuest(tc.result, tc.body))

			_, err := inst.Start(t.Context(), 9, 0, nil)
			require.ErrorContains(t, err, tc.want)
			assert.True(t, inst.Broken(), "an instance that broke the step protocol is never reused")
		})
	}
}

func TestAFailedStepBreaksTheInstance(t *testing.T) {
	t.Parallel()

	inst := mustInstantiate(t, stepGuest(statusMessage(wire.StatusPanic, "out of memory"), nil))

	step, err := inst.Start(t.Context(), 9, 0, nil)
	require.NoError(t, err)
	assert.Equal(t, engine.Step{Failure: engine.Result{Status: wire.StatusPanic, Message: "out of memory"}}, step)
	assert.True(t, inst.Broken())
}

func TestHTTPStartOnlyDuringSteps(t *testing.T) {
	t.Parallel()

	g := stepGuest(stepResult(), nil)
	g.bodies["plg_call"] = slices.Concat(startRequest(), packed(0, 1))
	inst := mustInstantiate(t, g)

	_, err := inst.Call(t.Context(), 0, nil)
	require.ErrorIs(t, err, engine.ErrABI)
	require.ErrorContains(t, err, "http_start outside a multiplexed step")
	assert.True(t, inst.Broken())
}
