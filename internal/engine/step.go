package engine

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/tetratelabs/wazero/api"

	"github.com/heynemann/go-plg/internal/httpabi"
	"github.com/heynemann/go-plg/internal/wire"
)

// Multiplexed steps: the exports and the import through which a guest serves several calls at once. A step
// runs guest code until it can make no more progress: every call it started is then finished or waiting for an
// HTTP request the guest started with http_start, whose answer a later plg_resume step delivers. Only the
// script guests (Lua, TypeScript) export them; the Go guest serves one call at a time.
const (
	// exportStart is plg_start(call, method, len u32) u64: start call number call (chosen by the host, unique
	// among the instance's calls in flight) of the method with the given index, its wire-encoded arguments in the
	// input buffer.
	exportStart = "plg_start"
	// exportResume is plg_resume(request, len u32) u64: deliver the [httpabi] answer in the input buffer to the
	// request with that number.
	exportResume = "plg_resume"
	// importHTTPStart is http_start(call, ptr, len u32) u32: during a step, the guest hands over an [httpabi]
	// request made by the given call (in flight: started and not ended, see [Instance.EndCall]); the host copies
	// it, or refuses it at once, numbers it and returns its number. Outside a step it is an ABI violation.
	importHTTPStart = "http_start"
)

// A step's result is a status-prefixed buffer like any other. With [wire.StatusOK], the body lists the calls the
// step finished: a uint32 count, then per call its number, the uint32 length of its result and that result (a
// status-prefixed buffer: OK + the encoded results, or a status and a message). Any other status fails the step
// as a whole, and the instance is broken.

// Completion is a call a step finished. Its Result's Body aliases guest memory and is valid only until the next
// step: copy it first.
type Completion struct {
	Result Result
	Call   uint32
}

// Request is an HTTP request a step started: the N-th (from 1) of call number Call, numbered ID. Body is the
// [httpabi]-encoded request, a copy the caller owns, for the caller to make; or nil when http_start refused the
// request at once (see [InstanceOptions.HTTPRefuses]): Answer is then the refusal, for the caller to deliver with
// Resume as it would the answer of a request it made.
type Request struct {
	Body   []byte
	Answer []byte
	N      int
	Call   uint32
	ID     uint32
}

// Step is what one step did. Done and Requests are reused by the next step: consume them first; the caller may
// clear Requests once consumed, so that it does not keep their bodies alive meanwhile. Failure is the zero
// Result unless the guest failed the step as a whole ([wire.StatusPanic] or [wire.StatusABI] with a message);
// the instance is then broken, and Done and Requests are empty.
type Step struct {
	Done     []Completion
	Requests []Request
	Failure  Result
}

// Multiplexes reports whether the guest serves several calls at once, i.e. exports plg_start and plg_resume.
func (in *Instance) Multiplexes() bool {
	return in.start != nil && in.resume != nil
}

// Start starts call number call of the method with the given index, with its encoded arguments, and runs the
// guest until it can make no more progress. The call is then in flight until [Instance.EndCall]. The instance
// must multiplex.
func (in *Instance) Start(ctx context.Context, call, method uint32, args []byte) (Step, error) {
	err := in.write(ctx, args)
	if err != nil {
		return Step{}, err
	}

	if in.host.calls == nil {
		in.host.calls = make(map[uint32]int)
	}

	in.host.calls[call] = 0
	in.stack[0], in.stack[1], in.stack[2] = uint64(call), uint64(method), uint64(len(args))

	return in.step(ctx, &in.start, in.stack[:3])
}

// EndCall ends call number call, which the caller no longer has in flight: the guest may start no more requests
// for it, and Start may use its number again.
func (in *Instance) EndCall(call uint32) {
	delete(in.host.calls, call)
}

// Resume delivers the [httpabi] answer of request number id and runs the guest until it can make no more
// progress. The instance must multiplex.
func (in *Instance) Resume(ctx context.Context, id uint32, answer []byte) (Step, error) {
	err := in.write(ctx, answer)
	if err != nil {
		return Step{}, err
	}

	in.stack[0], in.stack[1] = uint64(id), uint64(len(answer))

	return in.step(ctx, &in.resume, in.stack[:2])
}

// step invokes a step export with HTTP requests started asynchronously and parses what it finished.
//
//nolint:gocritic // ptrToRefParam: invoke may replace the function, see there
func (in *Instance) step(ctx context.Context, fn *api.Function, stack []uint64) (Step, error) {
	clear(in.host.started) // the previous step's requests are consumed: drop their bodies

	in.host.stepping = true
	in.host.started = in.host.started[:0]

	res, err := in.invoke(ctx, fn, stack)

	in.host.stepping = false

	if err != nil {
		return Step{}, err
	}

	if res.Status != wire.StatusOK {
		in.broken = true

		return Step{Failure: res}, nil
	}

	in.done, err = parseCompletions(in.done[:0], res.Body)
	if err != nil {
		return Step{}, in.fail(err)
	}

	return Step{Done: in.done, Requests: in.host.started}, nil
}

// errMalformedStep reports a step result whose list of finished calls does not parse.
var errMalformedStep = fmt.Errorf("%w: malformed step result", ErrABI)

// parseCompletions appends the calls a step's body lists to done.
func parseCompletions(done []Completion, body []byte) ([]Completion, error) {
	count, ok := cutUint32(&body)
	if !ok {
		return done, errMalformedStep
	}

	for range count { // each iteration consumes at least 9 bytes or fails
		var call, size uint32

		call, ok = cutUint32(&body)
		if ok {
			size, ok = cutUint32(&body)
		}

		if !ok || size == 0 || uint64(size) > uint64(len(body)) {
			return done, errMalformedStep
		}

		res, err := parseResult(body[:size])
		if err != nil {
			return done, err
		}

		done = append(done, Completion{Call: call, Result: res})
		body = body[size:]
	}

	if len(body) != 0 {
		return done, fmt.Errorf("%w: %d trailing bytes", errMalformedStep, len(body))
	}

	return done, nil
}

// cutUint32 reads a little-endian uint32 off the front of *b.
func cutUint32(b *[]byte) (uint32, bool) {
	if len(*b) < 4 { //nolint:mnd // a uint32's size
		return 0, false
	}

	v := binary.LittleEndian.Uint32(*b)
	*b = (*b)[4:]

	return v, true
}

// httpStart serves http_start: it numbers the request and records it for the step's caller, copied, or answered
// at once without copying it when HTTP is disabled or refuses it whatever it holds (a limit): as the blocking
// http_roundtrip would, with the same answer, so that a guest cannot make the host hold requests the policy
// refuses anyway. As in httpRoundTrip, ABI violations panic, which traps the step and breaks the instance.
func (r *Runtime) httpStart(ctx context.Context, mod api.Module, stack []uint64) {
	state := r.state(mod)
	call := api.DecodeU32(stack[0])

	req, ok := mod.Memory().Read(api.DecodeU32(stack[1]), api.DecodeU32(stack[2]))
	if !ok {
		panic(fmt.Errorf("%w: %s request out of range", ErrABI, importHTTPStart))
	}

	if !state.stepping {
		panic(fmt.Errorf("%w: %s outside a multiplexed step", ErrABI, importHTTPStart))
	}

	n, ok := state.calls[call]
	if !ok {
		panic(fmt.Errorf("%w: %s for call %d, not in flight", ErrABI, importHTTPStart, call))
	}

	n++
	state.calls[call] = n
	state.lastRequest++
	started := Request{Call: call, ID: state.lastRequest, N: n}

	switch {
	case state.http == nil:
		started.Answer = httpabi.AppendFailure(nil, ErrHTTPDisabled.Error())
	case state.refuses != nil && state.refuses(n, len(req)):
		started.Answer = state.http(ctx, nil, req, n)
	default:
		started.Body = append([]byte(nil), req...)
	}

	state.started = append(state.started, started)
	stack[0] = api.EncodeU32(state.lastRequest)
}
