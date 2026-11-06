package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

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
	// importHost is host_call(method, ptr, len u32) u32: the guest calls the Host method with the given index
	// with the wire-encoded arguments at ptr; the host keeps the status-prefixed answer as the pending result and
	// returns its length.
	importHost = "host_call"
	// importTake is take_result(ptr, len u32): the guest has the pending result, exactly len bytes, copied to
	// ptr. Any mismatch breaks the instance.
	importTake = "take_result"
)

// keepPending bounds the pending-result buffer an instance keeps between host calls; larger ones are dropped
// once taken.
const keepPending = 64 << 10

// ErrHTTPDisabled is the refusal instances created without an [HTTPFunc] answer every request with.
var ErrHTTPDisabled = errors.New("plg: HTTP is not enabled for this plugin")

// ErrStopped fails what an instance does once its runtime closes ([Runtime.Close]): the initialization, call or
// step in progress (its context, which the host function it waits in has, ends with ErrStopped as the cause) and
// every later one. The instance is broken.
var ErrStopped = errors.New("engine: instance stopped: its runtime closed")

// HTTPFunc serves a guest's http_roundtrip: it makes (or refuses) the [httpabi]-encoded request req and appends
// the encoded answer to dst. req aliases guest memory and is valid only until it returns. ctx is the context of
// the plg_init or plg_call in progress, which the instance's stop ends too, and n counts that call's requests,
// from 1. One HTTPFunc serves every instance created with it, so it must be safe for concurrent use.
type HTTPFunc func(ctx context.Context, dst, req []byte, n int) []byte

// HostFunc serves a guest's host_call: it calls the Host method with the given index with the wire-encoded
// arguments args and appends the status-prefixed answer to dst (the encoded results for [wire.StatusOK],
// otherwise a message). args aliases guest memory and is valid only until it returns. ctx is the context of the
// plg_init or plg_call in progress, which the instance's stop ends too. An error means the request itself is
// invalid (an unknown method, malformed arguments): the guest broke the ABI and the instance breaks. One HostFunc
// serves every instance created with it, so it must be safe for concurrent use.
type HostFunc func(ctx context.Context, dst []byte, method uint32, args []byte) ([]byte, error)

// hostState is what the host imports know about one instance. Only the goroutine calling the instance touches
// it, a guest calling its imports synchronously, except for its stop (stage, stopped and the cancel the stage
// hands over), which [Runtime.Close] triggers from another goroutine.
type hostState struct {
	http        HTTPFunc
	refuses     func(n, size int) bool // see InstanceOptions.HTTPRefuses
	host        HostFunc
	runtime     *Runtime                // the instance's, told when a guest run its stop found leaves
	cancel      context.CancelCauseFunc // ends the context of the guest run in progress (stage hostBusy)
	calls       map[uint32]int          // the multiplexed calls in flight: the requests each started with http_start
	stopped     chan struct{}           // closed once the instance is stopped
	pending     []byte                  // the result take_result hands over
	started     []Request               // the requests http_start took during the current step
	requests    int                     // http_roundtrip calls in the current plg_init, plg_call or step
	stage       atomic.Int32            // hostIdle, hostBusy or hostStopped
	lastRequest uint32                  // the number of the last request http_start took
	ready       bool                    // pending holds a result not taken yet
	stepping    bool                    // a multiplexed step is running: http_start is allowed
}

// The stages of a hostState: whether the guest runs (its initialization, an ABI export or a step, host functions
// included), and whether the instance was stopped. Only the calling goroutine moves it between hostIdle and
// hostBusy; only stop moves it to hostStopped, for good.
const (
	hostIdle int32 = iota
	hostBusy
	hostStopped
)

func newHostState(opts *InstanceOptions, rt *Runtime) *hostState {
	return &hostState{
		http: opts.HTTP, refuses: opts.HTTPRefuses, host: opts.Host, runtime: rt, stopped: make(chan struct{}),
	}
}

// begin resets the per-call state before a plg_init or plg_call.
func (st *hostState) begin() {
	st.requests, st.ready = 0, false
}

// enter starts a run of the guest under ctx, returning the context it runs under, ended too with [ErrStopped] as
// the cause when the instance is stopped before leave, and the cancel to hand leave; or false when the instance
// is stopped already: the guest must not run. The calling goroutine hands the cancel over to stop through the
// stage: it writes st.cancel only while the stage is not hostStopped, then moves the stage to hostBusy, and stop
// reads st.cancel only when it finds hostBusy, so the write happens before the read; once the stage is
// hostStopped, st.cancel is never written again.
//
// The context is how a stop reaches a running guest safely: wazero's close-on-context-done marks the module
// closed, so the guest stops at its next check, and leaves freeing its resources to the goroutine running it,
// whereas closing the module from another goroutine frees its WASI state under the guest.
func (st *hostState) enter(ctx context.Context) (context.Context, context.CancelCauseFunc, bool) {
	ctx, cancel := context.WithCancelCause(ctx)

	if st.stage.Load() != hostStopped {
		st.cancel = cancel

		if st.stage.CompareAndSwap(hostIdle, hostBusy) {
			return ctx, cancel, true
		}
	}

	cancel(nil)

	return nil, nil, false
}

// leave ends the guest run enter started under ctx, releasing the run's context. A run stop found is one its
// runtime's close waits for: leave tells the runtime it has ended.
func (st *hostState) leave(ctx context.Context, cancel context.CancelCauseFunc) {
	cancel(nil) // no effect once stop cancelled it

	if !st.stage.CompareAndSwap(hostBusy, hostIdle) { // stopped during the run
		st.runtime.left(ctx)
	}
}

// stop stops the instance, reporting whether its guest was running: the run's context then ends with
// [ErrStopped], and with it the host function the guest waits in, if any, and a sleep in poll_oneoff; later runs
// are refused; stopped is closed. It is safe for concurrent use.
func (st *hostState) stop() bool {
	previous := st.stage.Swap(hostStopped)
	if previous == hostStopped {
		return false
	}

	if previous == hostBusy {
		st.cancel(ErrStopped)
	}

	close(st.stopped)

	return previous == hostBusy
}

// isStopped reports whether the instance was stopped.
func (st *hostState) isStopped() bool {
	return st.stage.Load() == hostStopped
}

// instantiateHostModule adds the plg host module to rt.
func (r *Runtime) instantiateHostModule(ctx context.Context, rt wazero.Runtime) {
	i32 := api.ValueTypeI32

	_, err := rt.NewHostModuleBuilder(hostModule).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(r.httpRoundTrip), []api.ValueType{i32, i32}, []api.ValueType{i32}).
		Export(importHTTP).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(r.hostCall), []api.ValueType{i32, i32, i32}, []api.ValueType{i32}).
		Export(importHost).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(r.takeResult), []api.ValueType{i32, i32}, nil).
		Export(importTake).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(r.httpStart), []api.ValueType{i32, i32, i32}, []api.ValueType{i32}).
		Export(importHTTPStart).
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
// instance is then broken and discarded. The request runs under the call's context, which the instance's stop
// ends.
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

// hostCall serves host_call. As in httpRoundTrip, ABI violations panic, which traps the call and breaks the
// instance; so does a host_call from an instance created without a [HostFunc], whose guest has no Host to call.
// The Host method runs under the call's context, which the instance's stop ends.
func (r *Runtime) hostCall(ctx context.Context, mod api.Module, stack []uint64) {
	state := r.state(mod)

	args, ok := mod.Memory().Read(api.DecodeU32(stack[1]), api.DecodeU32(stack[2]))
	if !ok {
		panic(fmt.Errorf("%w: %s arguments out of range", ErrABI, importHost))
	}

	if state.host == nil {
		panic(fmt.Errorf("%w: %s from an instance without a host", ErrABI, importHost))
	}

	answer, err := state.host(ctx, state.pending[:0], api.DecodeU32(stack[0]), args)
	if err != nil {
		panic(fmt.Errorf("%w: %s: %w", ErrABI, importHost, err))
	}

	state.pending, state.ready = answer, true
	stack[0] = api.EncodeU32(uint32(len(answer))) //nolint:gosec // G115: answers are bounded far below 4 GiB
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
