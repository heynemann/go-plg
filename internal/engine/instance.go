package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/tetratelabs/wazero/api"

	"github.com/heynemann/go-plg/internal/wire"
)

// ErrABI reports a guest that broke the plg guest ABI: missing exports, out-of-range buffers, an invalid
// status or a malformed message. The instance is broken afterwards.
var ErrABI = errors.New("engine: guest ABI violation")

// CloseError reports a plugin whose Close, run by plg_close, did not succeed: Status is [wire.StatusError] when
// it returned an error and [wire.StatusPanic] when it panicked (any other status breaks the ABI), with the
// guest's message.
type CloseError struct {
	Message string
	Status  wire.Status
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("engine: plugin Close failed: guest status %s: %s", e.Status, e.Message)
}

// Guest export names.
const (
	exportAlloc = "plg_alloc"
	exportInit  = "plg_init"
	exportCall  = "plg_call"
	exportClose = "plg_close"
)

// ptrShift is where the ABI packs a result buffer's pointer: ptr<<32 | len.
const ptrShift = 32

// FlagDeepStack is a flag a guest may set on any result's status byte: the step ran deep on the guest's stack.
// wazero then grew the native stack it ran the export on, which it keeps with the export's function, so the
// instance replaces the function to let that memory go. The flag never changes what the status means. Only the
// TypeScript guest sets it (see guestsrc/typescript/glue.c), so it is not part of internal/wire.
const FlagDeepStack = 0x80

// Instance is one guest instance speaking the plg ABI. It is NOT safe for concurrent use: one goroutine owns
// it at a time. Once [Instance.Broken] reports true it must be closed, never called again: wazero leaves a
// trapped instance callable, and it then answers with garbage.
type Instance struct {
	mod                      api.Module
	mem                      api.Memory
	alloc, init, call, close api.Function
	host                     *hostState
	hosts                    *xsync.Map[string, *hostState] // the runtime's registry host lives in
	inPtr, inCap             uint32
	stack                    [2]uint64
	broken                   bool
}

// Result is a guest's answer. Body aliases guest memory and is valid only until the next call on the instance;
// decode it first. For statuses other than [wire.StatusOK], Message holds the decoded message and Body is nil.
type Result struct {
	Message string
	Body    []byte
	Status  wire.Status
}

func newInstance(ctx context.Context, mod api.Module, host *hostState, hosts *xsync.Map[string, *hostState]) (
	*Instance, error,
) {
	inst := &Instance{
		mod:   mod,
		mem:   mod.Memory(),
		alloc: mod.ExportedFunction(exportAlloc),
		init:  mod.ExportedFunction(exportInit),
		call:  mod.ExportedFunction(exportCall),
		close: mod.ExportedFunction(exportClose),
		host:  host,
		hosts: hosts,
	}

	if inst.mem == nil || inst.alloc == nil || inst.init == nil || inst.call == nil || inst.close == nil {
		return nil, errors.Join(
			fmt.Errorf("%w: missing memory or one of the %s, %s, %s, %s exports",
				ErrABI, exportAlloc, exportInit, exportCall, exportClose),
			mod.Close(ctx))
	}

	return inst, nil
}

// Broken reports whether the instance trapped, was stopped, violated the ABI or panicked.
func (in *Instance) Broken() bool {
	return in.broken
}

// Memory returns the size of the instance's linear memory in bytes. It reads the size in whole pages (growing
// by zero pages changes nothing), which is exact up to the 4 GiB maximum, unlike the byte size wazero reports as
// a uint32.
func (in *Instance) Memory() uint64 {
	pages, _ := in.mem.Grow(0)

	return uint64(pages) * PageSize
}

// Init sends the init payload to plg_init.
func (in *Instance) Init(ctx context.Context, payload []byte) (Result, error) {
	err := in.write(ctx, payload)
	if err != nil {
		return Result{}, err
	}

	in.stack[0] = uint64(len(payload))

	return in.invoke(ctx, &in.init, in.stack[:1])
}

// Call sends the encoded arguments to plg_call for the method with the given index.
func (in *Instance) Call(ctx context.Context, method uint32, args []byte) (Result, error) {
	err := in.write(ctx, args)
	if err != nil {
		return Result{}, err
	}

	in.stack[0], in.stack[1] = uint64(method), uint64(len(args))

	return in.invoke(ctx, &in.call, in.stack[:2])
}

// Close frees the instance. First, on a healthy instance and while ctx lives, it runs plg_close, which calls the
// plugin's optional Close: one that fails or panics is reported as a [*CloseError], a guest that traps or is
// stopped by ctx meanwhile as the trap; the instance is freed all the same.
func (in *Instance) Close(ctx context.Context) error {
	var err error

	if !in.broken && ctx.Err() == nil {
		var res Result

		res, err = in.invoke(ctx, &in.close, in.stack[:1])
		if err == nil && res.Status != wire.StatusOK {
			err = &CloseError{Status: res.Status, Message: res.Message}
		}
	}

	in.broken = true
	err = errors.Join(err, in.mod.Close(ctx))
	in.hosts.Delete(in.mod.Name())

	return err
}

// write copies req into the guest's input buffer, growing it through plg_alloc when needed. A request longer
// than the 4 GiB wasm32 address space cannot fit guest memory, so mem.Write rejects it.
func (in *Instance) write(ctx context.Context, req []byte) error {
	size := uint32(len(req)) //nolint:gosec // G115: oversized requests fail the bounds-checked mem.Write below
	if size > in.inCap {
		in.stack[0] = uint64(size)

		err := in.alloc.CallWithStack(ctx, in.stack[:1])
		if err != nil {
			return in.fail(fmt.Errorf("%s: %w", exportAlloc, err))
		}

		in.inPtr, in.inCap = uint32(in.stack[0]), size //nolint:gosec // G115: a wasm32 pointer in the low 32 bits
	}

	if !in.mem.Write(in.inPtr, req) {
		return in.fail(fmt.Errorf("%w: %s returned an out-of-range buffer", ErrABI, exportAlloc))
	}

	return nil
}

// invoke calls *fn, whose packed result names a status-prefixed buffer, and validates that buffer. After a step
// flagged [FlagDeepStack], *fn becomes a fresh function of the same export: the old one holds the native
// stack wazero grew for the step (tens of MiB for a deep JS recursion) and never shrinks it.
//
//nolint:gocritic // ptrToRefParam: the pointer is the point, invoke replaces the caller's function
func (in *Instance) invoke(ctx context.Context, fn *api.Function, stack []uint64) (Result, error) {
	in.host.begin()

	err := (*fn).CallWithStack(ctx, stack)
	if err != nil {
		return Result{}, in.fail(fmt.Errorf("%s: %w", exportName(*fn), err))
	}

	ptr, size := uint32(stack[0]>>ptrShift), uint32(stack[0]) //nolint:gosec // G115: two packed uint32 halves

	view, ok := in.mem.Read(ptr, size)
	if !ok || size == 0 {
		return Result{}, in.fail(fmt.Errorf("%w: invalid result buffer (%d bytes at %#x)", ErrABI, size, ptr))
	}

	if view[0]&FlagDeepStack != 0 {
		*fn = in.mod.ExportedFunction(exportName(*fn))
	}

	res, err := parseResult(view)
	if err != nil {
		return Result{}, in.fail(err)
	}

	if res.Status == wire.StatusPanic || res.Status == wire.StatusABI {
		in.broken = true
	}

	return res, nil
}

// exportName returns the name fn is exported under.
func exportName(fn api.Function) string {
	return fn.Definition().ExportNames()[0]
}

// parseResult splits a result buffer into its status, without the deep-stack flag, and its body, decoding the
// message of non-OK statuses.
func parseResult(view []byte) (Result, error) {
	status := wire.Status(view[0] &^ FlagDeepStack)

	switch status {
	case wire.StatusOK:
		return Result{Status: status, Body: view[1:]}, nil
	case wire.StatusError, wire.StatusLoad, wire.StatusPanic, wire.StatusABI, wire.StatusConvert:
	default:
		return Result{}, fmt.Errorf("%w: invalid status %d", ErrABI, view[0])
	}

	reader := wire.NewReader(view[1:])

	msg, err := reader.Text()
	if err == nil {
		err = reader.End()
	}

	if err != nil {
		return Result{}, fmt.Errorf("%w: malformed %s message: %w", ErrABI, status, err)
	}

	return Result{Status: status, Message: msg}, nil
}

func (in *Instance) fail(err error) error {
	in.broken = true

	return err
}
