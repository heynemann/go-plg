package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// importPoll is WASI's poll_oneoff(in, out, nsubscriptions, nevents u32) errno: it waits until one of the
// nsubscriptions subscriptions at in triggers, writes an event for each one that did to out and their count to
// nevents.
const importPoll = "poll_oneoff"

// poll_oneoff's records (WASI preview 1). A subscription is 48 bytes: its userdata (u64) at 0 and its type (u8)
// at 8, then for a clock its timeout (u64, nanoseconds) at 24 and its flags (u16) at 40. An event is 32 bytes:
// the subscription's userdata at 0, an errno (u16) at 8 and the type at 10, all zero but the userdata for a
// clock that expired.
const (
	subscriptionSize    = 48
	subscriptionType    = 8
	subscriptionTimeout = 24
	subscriptionFlags   = 40
	eventSize           = 32
	countSize           = 4 // nevents is a u32
	eventTypeClock      = 0
	clockAbstime        = 1 // the clock flag of an absolute timeout
)

// errno is a WASI error number.
type errno uint32

// The errnos poll_oneoff returns.
const (
	errnoSuccess errno = 0
	errnoFault   errno = 21
	errnoIntr    errno = 27
	errnoInval   errno = 28
	errnoNotsup  errno = 58
)

// instantiateWASI adds WASI preview 1 to rt: wazero's functions, except poll_oneoff, which [pollOneoff] serves.
func instantiateWASI(ctx context.Context, rt wazero.Runtime) {
	i32 := api.ValueTypeI32

	wasi := rt.NewHostModuleBuilder(wasi_snapshot_preview1.ModuleName)
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(wasi)

	_, err := wasi.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(pollOneoff), []api.ValueType{i32, i32, i32, i32}, []api.ValueType{i32}).
		Export(importPoll). // replaces wazero's
		Instantiate(ctx)
	if err != nil { // only a programming error in the definitions above, on a fresh runtime
		panic(fmt.Sprintf("engine: instantiating %s: %v", wasi_snapshot_preview1.ModuleName, err))
	}
}

// pollOneoff serves poll_oneoff in place of wazero's, which waits out a clock with a sleep nothing interrupts:
// close-on-context-done stops running wasm, never a host function. The Go runtime waits there whenever every
// goroutine waits for a timer (time.Sleep, time.After, a timeout), so a Go plugin sleeping in a call outlived
// the call's deadline, one sleeping in Create blocked Register, and one sleeping in a refill's Create blocked
// the pool's close.
//
// pollOneoff waits until its earliest clock expires or ctx ends; ctx is the context of the guest call in progress
// (the instantiation running the guest's initialization, an ABI export or a step). When ctx ends first,
// it closes the module with the exit code close-on-context-done uses and returns EINTR: the guest stops at its
// next check exactly as when it is stopped while running, so the call fails with the [sys.ExitError] matching
// ctx's error and the instance is broken.
//
// It serves relative clocks only; wazero does not support absolute ones either. Any other subscription fails the
// poll with ENOTSUP: nothing in a plg instance can be waited for. It has no sockets or pipes, an empty stdin and
// read-only in-memory files, and the Go runtime polls a file descriptor only after an I/O on it would block,
// which never happens there; the Lua and TypeScript guests do not import poll_oneoff.
func pollOneoff(ctx context.Context, mod api.Module, stack []uint64) {
	stack[0] = uint64(poll(ctx, mod, api.DecodeU32(stack[0]), api.DecodeU32(stack[1]), api.DecodeU32(stack[2]),
		api.DecodeU32(stack[3])))
}

// poll is pollOneoff with decoded parameters: the subscriptions at inPtr, the events to write at outPtr and their
// count at neventsPtr.
func poll(ctx context.Context, mod api.Module, inPtr, outPtr, count, neventsPtr uint32) errno {
	if count == 0 {
		return errnoInval
	}

	if uint64(count)*subscriptionSize > math.MaxUint32 { // more than wasm32 memory holds
		return errnoFault
	}

	mem := mod.Memory()
	subs, okSubs := mem.Read(inPtr, count*subscriptionSize)
	events, okEvents := mem.Read(outPtr, count*eventSize)
	fired, okFired := mem.Read(neventsPtr, countSize)

	if !okSubs || !okEvents || !okFired {
		return errnoFault
	}

	timeout, code := earliest(subs)
	if code != errnoSuccess {
		return code
	}

	if !wait(ctx, timeout) {
		//nolint:errcheck // as in wazero's own stop (FailIfClosed): closing a stopped module's files reports nothing useful
		_ = mod.CloseWithExitCode(ctx, exitCode(ctx.Err()))

		return errnoIntr
	}

	binary.LittleEndian.PutUint32(fired, expired(subs, events, timeout))

	return errnoSuccess
}

// earliest returns the earliest timeout of the subscriptions, which must all be relative clocks.
func earliest(subs []byte) (uint64, errno) {
	timeout := uint64(math.MaxUint64)

	for sub := range slices.Chunk(subs, subscriptionSize) {
		if sub[subscriptionType] != eventTypeClock {
			return 0, errnoNotsup
		}

		switch binary.LittleEndian.Uint16(sub[subscriptionFlags:]) {
		case 0:
		case clockAbstime:
			return 0, errnoNotsup
		default:
			return 0, errnoInval
		}

		timeout = min(timeout, binary.LittleEndian.Uint64(sub[subscriptionTimeout:]))
	}

	return timeout, errnoSuccess
}

// wait waits for timeout nanoseconds or until ctx ends, reporting whether the timeout elapsed. A timeout beyond
// [time.Duration] lasts until ctx ends.
func wait(ctx context.Context, timeout uint64) bool {
	if timeout == 0 {
		return true
	}

	timer := time.NewTimer(time.Duration(min(timeout, math.MaxInt64)))
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// expired writes an event to events for each subscription whose timeout is the one that expired, in order, and
// returns their count.
func expired(subs, events []byte, timeout uint64) uint32 {
	var n uint32

	for sub := range slices.Chunk(subs, subscriptionSize) {
		if binary.LittleEndian.Uint64(sub[subscriptionTimeout:]) != timeout {
			continue
		}

		userdata := binary.LittleEndian.Uint64(sub) // read first: the guest may have overlapped in and out
		event := events[n*eventSize : (n+1)*eventSize]
		clear(event)
		binary.LittleEndian.PutUint64(event, userdata)

		n++
	}

	return n
}

// exitCode is the exit code close-on-context-done gives a module whose context ended with err.
func exitCode(err error) uint32 {
	if errors.Is(err, context.DeadlineExceeded) {
		return sys.ExitCodeDeadlineExceeded
	}

	return sys.ExitCodeContextCanceled
}
