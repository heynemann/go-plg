package engine_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/httpabi"
	"github.com/heynemann/go-plg/internal/wire"
)

// Fake guest memory for the host import tests: an OK status byte then three more request bytes at address
// 0 (the request is those four bytes), and room for the host's answer at answerAt.
const (
	importHTTP = 0 // function index of plg.http_roundtrip
	importTake = 1 // function index of plg.take_result
	answerAt   = 256
)

func hostData() []byte { return []byte{byte(wire.StatusOK), 'R', 'E', 'Q'} }

// roundTrip pushes answerAt, sends the request and leaves [answerAt, answer length] for take_result.
func roundTrip() []byte {
	return slices.Concat(i32Const(answerAt), i32Const(0), i32Const(4), call(importHTTP))
}

// exchangeAndReturn makes one request, takes the answer to answerAt and returns it as the call's result
// buffer, whose length the test knows.
func exchangeAndReturn(answerLen uint32) []byte {
	return slices.Concat(roundTrip(), call(importTake), packed(answerAt, answerLen))
}

func hostGuest(callBody []byte) fakeGuest {
	bodies := abiOK(0, 1)
	bodies["plg_call"] = callBody

	return fakeGuest{data: hostData(), bodies: bodies, imports: []string{"http_roundtrip", "take_result"}}
}

type ctxKey struct{}

func TestHTTPRoundTripReachesTheHandler(t *testing.T) {
	t.Parallel()

	answer := []byte{byte(wire.StatusOK), 'h', 'i'}

	var seen []int

	handler := func(ctx context.Context, dst, req []byte, n int) []byte {
		assert.Equal(t, "call ctx", ctx.Value(ctxKey{}), "the handler runs with the call's context")
		assert.Equal(t, hostData(), req)

		seen = append(seen, n)

		return append(dst, answer...)
	}

	// Two requests in one call: the first answer is never taken, the second is.
	body := slices.Concat(i32Const(0), i32Const(4), call(importHTTP), drop(), exchangeAndReturn(3))

	inst, err := instantiateWith(t, hostGuest(body), engine.InstanceOptions{HTTP: handler})
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), ctxKey{}, "call ctx")

	for range 2 {
		res, err := inst.Call(ctx, 0, nil)
		require.NoError(t, err)
		assert.Equal(t, wire.StatusOK, res.Status)
		assert.Equal(t, "hi", string(res.Body), "take_result copied the answer into guest memory")
	}

	assert.Equal(t, []int{1, 2, 1, 2}, seen, "requests are counted per call")
	require.NoError(t, inst.Close(t.Context()))
}

func TestHTTPRefusedWithoutHandler(t *testing.T) {
	t.Parallel()

	refusal := httpabi.AppendFailure(nil, engine.ErrHTTPDisabled.Error())
	inst := mustInstantiate(t, hostGuest(exchangeAndReturn(uint32(len(refusal))))) //nolint:gosec // G115: tiny

	// The refusal is an httpabi failure, which is also a well-formed error result buffer.
	res, err := inst.Call(t.Context(), 0, nil)
	require.NoError(t, err)
	assert.Equal(t, wire.StatusError, res.Status)
	assert.Equal(t, engine.ErrHTTPDisabled.Error(), res.Message)
}

func TestHostImportViolationsBreakTheInstance(t *testing.T) {
	t.Parallel()

	ok := packed(0, 1)
	answer := func(_ context.Context, dst, _ []byte, _ int) []byte { return append(dst, 1, 2, 3) }

	tests := []struct {
		name string
		body []byte
	}{
		{
			"request out of range",
			slices.Concat(i32Const(engine.PageSize-2), i32Const(10), call(importHTTP), drop(), ok),
		},
		{"take without a request", slices.Concat(i32Const(answerAt), i32Const(3), call(importTake), ok)},
		{
			"take with the wrong length",
			slices.Concat(roundTrip(), drop(), i32Const(2), call(importTake), ok),
		},
		{
			"take twice",
			slices.Concat(roundTrip(), call(importTake), i32Const(answerAt), i32Const(3), call(importTake), ok),
		},
		{
			"take beyond memory",
			slices.Concat(roundTrip(), drop(), drop(), i32Const(engine.PageSize-1), i32Const(3), call(importTake), ok),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inst, err := instantiateWith(t, hostGuest(tc.body), engine.InstanceOptions{HTTP: answer})
			require.NoError(t, err)

			_, err = inst.Call(t.Context(), 0, nil)
			require.ErrorIs(t, err, engine.ErrABI)
			assert.True(t, inst.Broken())
		})
	}
}

func TestPendingAnswersDoNotOutliveTheirCall(t *testing.T) {
	t.Parallel()

	answer := func(_ context.Context, dst, _ []byte, _ int) []byte { return append(dst, 1, 2, 3) }
	g := hostGuest(slices.Concat(i32Const(answerAt), i32Const(3), call(importTake), packed(0, 1)))
	g.bodies["plg_init"] = slices.Concat(i32Const(0), i32Const(4), call(importHTTP), drop(), packed(0, 1))

	inst, err := instantiateWith(t, g, engine.InstanceOptions{HTTP: answer})
	require.NoError(t, err)

	_, err = inst.Init(t.Context(), nil)
	require.NoError(t, err, "init made a request and left its answer")

	_, err = inst.Call(t.Context(), 0, nil)
	require.ErrorIs(t, err, engine.ErrABI, "the next call cannot take it")
}

// hostCallGuest imports host_call (function 0) and take_result (function 1); its plg_call runs body.
func hostCallGuest(body []byte) fakeGuest {
	bodies := abiOK(0, 1)
	bodies["plg_call"] = body

	return fakeGuest{data: hostData(), bodies: bodies, imports: []string{"host_call", "take_result"}}
}

// hostCall calls Host method 7 with the four request bytes as arguments, leaving the answer length.
func hostCall() []byte {
	return slices.Concat(i32Const(7), i32Const(0), i32Const(4), call(0))
}

func TestHostCallReachesTheHostFunc(t *testing.T) {
	t.Parallel()

	var calls int

	host := func(ctx context.Context, dst []byte, method uint32, args []byte) ([]byte, error) {
		assert.Equal(t, "call ctx", ctx.Value(ctxKey{}), "the host runs with the call's context")
		assert.Equal(t, uint32(7), method)
		assert.Equal(t, hostData(), args)

		calls++

		return append(dst, byte(wire.StatusOK), 'o', 'k'), nil
	}

	// Push take_result's destination, call the host, take its answer and return it as the call's result.
	body := slices.Concat(i32Const(answerAt), hostCall(), call(1), packed(answerAt, 3))

	inst, err := instantiateWith(t, hostCallGuest(body), engine.InstanceOptions{Host: host})
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), ctxKey{}, "call ctx")

	for range 2 {
		res, err := inst.Call(ctx, 0, nil)
		require.NoError(t, err)
		assert.Equal(t, wire.StatusOK, res.Status)
		assert.Equal(t, "ok", string(res.Body), "take_result copied the host's answer into guest memory")
	}

	assert.Equal(t, 2, calls)
	assert.False(t, inst.Broken())
	require.NoError(t, inst.Close(t.Context()))
}

func TestHostCallViolationsBreakTheInstance(t *testing.T) {
	t.Parallel()

	ok := packed(0, 1)
	answer := func(_ context.Context, dst []byte, _ uint32, _ []byte) ([]byte, error) {
		return append(dst, byte(wire.StatusOK)), nil
	}
	refuse := func(_ context.Context, dst []byte, _ uint32, _ []byte) ([]byte, error) {
		return dst, errRefused
	}

	tests := []struct {
		host engine.HostFunc
		name string
		msg  string
		body []byte
	}{
		{nil, "no host", "host_call from an instance without a host", slices.Concat(hostCall(), drop(), ok)},
		{refuse, "host refuses the request", "host_call: refused", slices.Concat(hostCall(), drop(), ok)},
		{
			answer, "arguments out of range", "host_call arguments out of range",
			slices.Concat(i32Const(0), i32Const(engine.PageSize-2), i32Const(10), call(0), drop(), ok),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inst, err := instantiateWith(t, hostCallGuest(tc.body), engine.InstanceOptions{Host: tc.host})
			require.NoError(t, err)

			_, err = inst.Call(t.Context(), 0, nil)
			require.ErrorIs(t, err, engine.ErrABI)
			require.ErrorContains(t, err, tc.msg)
			assert.True(t, inst.Broken())
		})
	}
}

var errRefused = errors.New("refused")
