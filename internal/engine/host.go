package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/heynemann/go-plg/internal/httpabi"
)

// Host imports: the wasm module and function names guests import.
const (
	hostModule = "plg"
	// importHTTP is http_roundtrip(ptr, len u32) u32: the guest hands over an [httpabi] request; the host makes
	// it (or refuses it), keeps the encoded answer as the instance's pending result and returns its length.
	importHTTP = "http_roundtrip"
	// importTake is take_result(ptr, len u32): the guest has the pending result, exactly len bytes, copied to
	// ptr. Any mismatch breaks the instance.
	importTake = "take_result"
)

// keepPending bounds the pending-result buffer an instance keeps between host calls; larger ones are dropped
// once taken.
const keepPending = 64 << 10

// ErrHTTPDisabled is the refusal instances created without an [HTTPFunc] answer every request with.
var ErrHTTPDisabled = errors.New("plg: HTTP is not enabled for this plugin")

// HTTPFunc serves a guest's http_roundtrip: it makes (or refuses) the [httpabi]-encoded request req and appends
// the encoded answer to dst. req aliases guest memory and is valid only until it returns. ctx is the context of
// the plg_init or plg_call in progress and n counts that call's requests, from 1. One HTTPFunc serves every
// instance created with it, so it must be safe for concurrent use.
type HTTPFunc func(ctx context.Context, dst, req []byte, n int) []byte

// hostState is what the host imports know about one instance. Only the goroutine calling the instance touches
// it: a guest calls its imports synchronously.
type hostState struct {
	http     HTTPFunc
	pending  []byte // the result take_result hands over
	requests int    // http_roundtrip calls in the current plg_init or plg_call
	ready    bool   // pending holds a result not taken yet
}

// begin resets the per-call state before a plg_init or plg_call.
func (st *hostState) begin() {
	st.requests, st.ready = 0, false
}

// instantiateHostModule adds the plg host module to rt.
func (r *Runtime) instantiateHostModule(ctx context.Context, rt wazero.Runtime) {
	i32 := api.ValueTypeI32

	_, err := rt.NewHostModuleBuilder(hostModule).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(r.httpRoundTrip), []api.ValueType{i32, i32}, []api.ValueType{i32}).
		Export(importHTTP).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(r.takeResult), []api.ValueType{i32, i32}, nil).
		Export(importTake).
		Instantiate(ctx)
	if err != nil { // only a programming error in the definitions above, on a fresh runtime
		panic(fmt.Sprintf("engine: instantiating the %s host module: %v", hostModule, err))
	}
}

// state returns the calling instance's state. It is registered before the module is instantiated and removed
// after it is closed, so every caller has one.
func (r *Runtime) state(mod api.Module) *hostState {
	st, _ := r.hosts.Load(mod.Name())

	return st
}

// httpRoundTrip serves http_roundtrip. A host function panic traps the call, so ABI violations panic: the
// instance is then broken and discarded.
func (r *Runtime) httpRoundTrip(ctx context.Context, mod api.Module, stack []uint64) {
	state := r.state(mod)

	req, ok := mod.Memory().Read(api.DecodeU32(stack[0]), api.DecodeU32(stack[1]))
	if !ok {
		panic(fmt.Errorf("%w: %s request out of range", ErrABI, importHTTP))
	}

	state.requests++

	if state.http == nil {
		state.pending = httpabi.AppendFailure(state.pending[:0], ErrHTTPDisabled.Error())
	} else {
		state.pending = state.http(ctx, state.pending[:0], req, state.requests)
	}

	state.ready = true
	stack[0] = api.EncodeU32(uint32(len(state.pending))) //nolint:gosec // G115: answers are bounded far below 4 GiB
}

// takeResult serves take_result.
func (r *Runtime) takeResult(_ context.Context, mod api.Module, stack []uint64) {
	state := r.state(mod)
	ptr, size := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])

	if !state.ready || int(size) != len(state.pending) {
		panic(fmt.Errorf("%w: %s of %d bytes without a pending result of that length", ErrABI, importTake, size))
	}

	if !mod.Memory().Write(ptr, state.pending) {
		panic(fmt.Errorf("%w: %s destination out of range", ErrABI, importTake))
	}

	state.ready = false
	if cap(state.pending) > keepPending {
		state.pending = nil
	}
}
