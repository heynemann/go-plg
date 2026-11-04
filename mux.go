package plg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/httpabi"
	"github.com/heynemann/go-plg/internal/wire"
)

// muxGrace is how long, in all, a call that already ended (its answer delivered, or abandoned at its deadline)
// may go on running: the plugin code after its HTTP requests fail, such as cleanup in a catch or finally block.
// It is one deadline per call, from its end, however many steps the rest takes: a call still running then (a step
// running past it is stopped; a call that starts another step afterwards, say retrying a failed request forever,
// is failed instead) discards the instance, as any call stopped at its deadline. An abandoned call with none of
// its requests in flight, which nothing of its own can resume (a TypeScript promise awaiting what nothing or
// another call settles), does not wait for its grace to run out: the guest forgets it at once (see reclaim).
const muxGrace = 250 * time.Millisecond

var (
	errMuxDiscarded  = errors.New("the instance was discarded")
	errMuxPanic      = errors.New("the host panicked driving the instance")
	errMuxGraceSpent = fmt.Errorf("%w: the call ran on past its %s of grace after it ended", context.DeadlineExceeded,
		muxGrace)
)

// stepper is a guest instance that can serve several calls at once: a *guestInstance whose guest exports the
// multiplexed steps (Lua, TypeScript), or a fake in tests.
type stepper interface {
	instance
	// Multiplexes reports whether the guest exports the multiplexed steps.
	Multiplexes() bool
	Start(ctx context.Context, call, method uint32, args []byte) (engine.Step, error)
	Resume(ctx context.Context, request uint32, answer []byte) (engine.Step, error)
	// EndCall ends a call no longer in flight: its number may be started again.
	EndCall(call uint32)
	// Drop makes the guest forget a call in flight with none of its requests in flight, which the host gave up on.
	Drop(ctx context.Context, call uint32) error
}

// muxError fails a call on a multiplexed instance without a guest failure of its own: another call broke the
// instance ([ErrInstanceLost]; the instance is discarded), or the call's context ended while it waited for its
// instance or for its HTTP requests (the instance serves on).
type muxError struct {
	err     error
	discard bool
}

func (e *muxError) Error() string { return e.err.Error() }

func (e *muxError) Unwrap() error { return e.err }

// muxInstance serves up to len(lanes) calls at once on one guest instance. While a call waits for an HTTP
// request, the instance runs others: the guest starts the request with http_start, the host makes it on a
// goroutine of its own and a later step delivers the answer. A request the policy refuses whatever it holds
// (HTTP disabled, a limit) is refused by http_start itself, never copied: its answer waits in refused for a
// later step, with no goroutine.
//
// One goroutine, the owner (run), drives the instance: callers and request goroutines send it operations (ops),
// which it runs one at a time, so the instance and every callState are only ever touched by it. Callers take a
// lane each from the pool; a lane is an [instance] whose Call hands the call to the owner and waits for its
// answer in a mailbox, so every call has its own argument and result buffers. A caller whose context ends while
// its call waits leaves at once, even while the owner runs another call's step, and wakes the owner, which then
// abandons the call, answering its mailbox to nobody (see callWaiting and sweep), before it starts another: besides
// the calls the lanes hold, the only calls in flight are answered ones whose requests, ended with them, have not
// come back yet, and whose rest then runs within muxGrace. A step that traps, is stopped, panics or breaks the ABI
// fails its own call with a [PanicError] and every other call on the instance with [ErrInstanceLost]; the instance
// is then broken, and its lanes are discarded.
type muxInstance struct {
	inst       stepper // owned by run, then by the Close of the last lane
	pool       *pool
	ops        chan func()
	discarded  chan struct{} // closed when the pool discards a lane of a healthy instance
	stop       chan struct{} // closed by the Close of the last lane
	exited     chan struct{} // closed when run returns
	ready      chan struct{} // closed: what run selects on while refusals wait
	wake       chan struct{} // one pending wake-up of run once callers left (see left)
	flight     map[uint32]*callState
	refused    []refusal      // owner-only: the refusals to deliver, oldest first
	fetches    sync.WaitGroup // request goroutines
	memory     atomic.Uint64  // the instance's linear memory after its last step
	live       atomic.Int32   // lanes not closed
	busy       atomic.Int32   // lanes handed out to callers (the pool's count)
	broken     atomic.Bool
	discarding atomic.Bool
	leaving    atomic.Bool // a caller left while its call waited, since the owner's last sweep
	last       uint32      // the number of the last call started
}

// refusal is a request http_start refused at once, waiting for the owner to deliver its answer.
type refusal struct {
	state  *callState
	answer []byte
	id     uint32
}

// callState is one call in flight on a multiplexed instance, owned by its owner goroutine.
type callState struct {
	// ctx is the call's context, ended once the call is answered so that its requests end too. It is kept
	// because the call outlives the step that started it: its later steps and HTTP requests run under it,
	// exactly while the call is in flight, as an http.Request carries its own.
	ctx      context.Context //nolint:containedctx // the context of one call in flight, see above
	cancel   context.CancelFunc
	grace    time.Time // once the call is answered (mailbox nil), when the rest of it must have ended
	mailbox  *muxCall  // the call's mailbox; nil once answered
	pending  int       // HTTP requests not answered yet
	id       uint32
	finished bool // the guest ended the call
}

// The stages of a muxCall: the handshake between its caller, who alone leaves, and the owner, who alone steps,
// rests and answers. While the call waits (for the owner to start it, for its HTTP requests, or for the owner,
// busy with another call's step), a caller whose context ends leaves at once; during a step of the call's own,
// which runs under that context, it waits for the step's outcome instead: the context stops the step.
const (
	callQueued   uint32 = iota // handed to the owner, not started yet
	callWaiting                // started; the owner runs no step of its own
	callStepping               // a step runs under the call's own context
	callLeaving                // that context ended during the step: the caller waits for the answer
	callAnswered               // the owner answered: the caller takes the answer
	callLeft                   // the caller left: the owner answers to nobody
)

// muxCall is a call's mailbox: the caller fills it, the owner answers it once and signals done. A lane reuses its
// mailbox from call to call, except when its caller left before the answer: the owner keeps that one, to answer
// it to nobody, and the lane takes a new one.
type muxCall struct {
	err    error
	cause  error      // why the caller left, or is leaving: its context's cause
	state  *callState // owner-only: the call in flight, nil once answered
	done   chan struct{}
	guest  string // what the guest wrote to stderr during a failed step
	body   []byte // the result's body, copied out of guest memory
	result engine.Result
	stage  atomic.Uint32 // callQueued, …
}

// muxLane is one of the calls a multiplexed instance serves at once, as the pool hands it out.
type muxLane struct {
	m       *muxInstance
	mailbox *muxCall
	primary bool // reports the instance's memory; the other lanes report none
}

func newMuxCall() *muxCall {
	return &muxCall{done: make(chan struct{}, 1)}
}

// newMux starts the owner of inst and returns its lanes. The pool owns the lanes; the owner stops when the
// last of them is closed.
func newMux(p *pool, inst stepper, lanes int) []*muxLane {
	mux := &muxInstance{
		inst:      inst,
		pool:      p,
		ops:       make(chan func()),
		discarded: make(chan struct{}),
		stop:      make(chan struct{}),
		exited:    make(chan struct{}),
		flight:    make(map[uint32]*callState),
		ready:     make(chan struct{}),
		wake:      make(chan struct{}, 1),
	}
	close(mux.ready)
	mux.memory.Store(inst.Memory())
	mux.live.Store(int32(lanes)) //nolint:gosec // G115: lanes are validated to at most maxConcurrentCalls

	out := make([]*muxLane, lanes)
	for i := range out {
		out[i] = &muxLane{m: mux, primary: i == 0, mailbox: newMuxCall()}
	}

	go mux.run()

	return out
}

// run is the owner goroutine: it runs operations, and delivers the refusals waiting one at a time among them,
// until the last lane is closed.
func (m *muxInstance) run() {
	defer close(m.exited)

	discarded := m.discarded

	for {
		var refusals chan struct{}

		if len(m.refused) > 0 {
			refusals = m.ready
		}

		select {
		case operation := <-m.ops:
			m.do(operation)
		case <-refusals:
			m.do(m.deliver)
		case <-m.wake:
			m.do(m.sweep)
		case <-discarded:
			discarded = nil

			m.do(func() { m.fail(nil, engine.Result{}, errMuxDiscarded) })
		case <-m.stop:
			for _, state := range m.flight {
				state.cancel()
			}

			m.fetches.Wait() // request goroutines give up sending once stop is closed

			return
		}
	}
}

// deliver delivers the oldest refusal waiting.
func (m *muxInstance) deliver() {
	next := m.refused[0]
	m.refused[0] = refusal{}
	m.refused = m.refused[1:]

	m.resume(next.state, next.id, next.answer)
}

// do runs an operation behind a panic boundary: a host panic breaks the instance, failing its calls.
func (m *muxInstance) do(operation func()) {
	defer func() {
		if v := recover(); v != nil {
			// The owner serves many calls and has no context of its own.
			m.pool.panics.recovered(context.Background(),
				&PanicError{Key: m.pool.key, Language: m.pool.lang, Phase: PhaseCall}, v)
			m.fail(nil, engine.Result{}, fmt.Errorf("%w: %v", errMuxPanic, v))
		}
	}()

	operation()
}

// start starts a call, answering it at once when the call's context has ended (its caller may have left
// already) or the instance is broken. First it abandons the calls whose callers left, so that a lane's call never
// adds to the one its last caller left.
func (m *muxInstance) start(ctx context.Context, mailbox *muxCall, method uint32, args []byte) {
	m.sweep() //nolint:contextcheck // each call left behind is dropped under its own context, within its grace

	if ctx.Err() != nil || !mailbox.begin(callQueued) {
		mailbox.answer(engine.Result{}, unstarted(context.Cause(ctx)), "")

		return
	}

	if m.broken.Load() {
		mailbox.answer(engine.Result{}, &muxError{
			err: fmt.Errorf("%w: %w", ErrInstanceLost, errMuxDiscarded), discard: true,
		}, "")

		return
	}

	m.last++
	for m.flight[m.last] != nil || m.last == 0 { // numbers wrap around after 2^32 calls
		m.last++
	}

	callCtx, cancel := context.WithCancel(ctx)
	state := &callState{ctx: callCtx, cancel: cancel, mailbox: mailbox, id: m.last}
	mailbox.state = state
	m.flight[state.id] = state

	step, err := m.inst.Start(callCtx, state.id, method, args)
	m.after(state, &step, err) //nolint:contextcheck // requests run under their own call's context
	m.settle(state)
	m.reclaim(state) //nolint:contextcheck // dropped under its own context, within its grace
}

// abandon answers a call whose caller's context ended with cause while the call waited (or ran a step of its
// own), and ends its requests; a caller that left already gets nothing. The guest then runs the rest of the call
// (with the failed requests) within muxGrace in all, or forgets it when none of its requests is in flight
// (reclaim).
func (*muxInstance) abandon(state *callState, cause error) {
	state.cancel()
	state.mailbox.answer(engine.Result{}, abandoned(cause), "")

	state.grace = time.Now().Add(muxGrace)
}

// abandoned is the error of a call whose context ended with cause while it waited for its HTTP requests.
func abandoned(cause error) *muxError {
	return &muxError{err: fmt.Errorf("abandoned while waiting for its HTTP requests: %w", cause)}
}

// unstarted is the error of a call whose context ended with cause before its instance started it.
func unstarted(cause error) *muxError {
	return &muxError{err: fmt.Errorf("waiting for its instance: %w", cause)}
}

// resume delivers a request's answer to the guest. When the call's context has ended meanwhile (its caller left,
// or will find it answered), it is abandoned now, so that the step does not run under an ended context, which
// would stop the whole instance.
func (m *muxInstance) resume(state *callState, request uint32, answer []byte) {
	state.pending--

	defer m.forget(state)

	if m.broken.Load() {
		return
	}

	if mailbox := state.mailbox; mailbox != nil {
		switch {
		case state.ctx.Err() != nil:
			m.abandon(state, context.Cause(state.ctx))
		case !mailbox.begin(callWaiting):
			m.abandon(state, mailbox.cause) // its caller left before its context's end reached the call's
		default:
			// The step runs under the call's context, its caller waiting.
		}
	}

	ctx := state.ctx
	if state.mailbox == nil { // the call is answered: the rest of it runs on borrowed time, muxGrace in all
		var (
			cancel context.CancelFunc
			ok     bool
		)

		ctx, cancel, ok = m.borrowed(state)
		if !ok {
			return
		}

		defer cancel()
	}

	step, err := m.inst.Resume(ctx, request, answer)
	m.after(state, &step, err)
	m.settle(state)
	m.reclaim(state)
}

// borrowed returns the context the rest of an answered call runs under, ending with its grace. Once the grace has
// passed, it fails the instance instead, the call to blame (errMuxGraceSpent), and reports false.
func (m *muxInstance) borrowed(state *callState) (context.Context, context.CancelFunc, bool) {
	if !time.Now().Before(state.grace) {
		m.fail(state, engine.Result{}, errMuxGraceSpent)

		return nil, nil, false
	}

	ctx, cancel := context.WithDeadline(context.WithoutCancel(state.ctx), state.grace)

	return ctx, cancel, true
}

// left is a caller's report that it left while its call waited: it wakes the owner to abandon the call (sweep),
// which the owner would otherwise learn only when one of the call's requests comes back, never for a call with
// none in flight.
func (m *muxInstance) left() {
	m.leaving.Store(true)

	select {
	case m.wake <- struct{}{}:
	default: // a wake-up is pending already
	}
}

// sweep abandons the calls whose callers left while they waited, as their callers reported (left): those with
// none of their requests in flight are reclaimed at once, the others once their requests, ended with them, come
// back. The owner sweeps when woken and before it starts a call.
func (m *muxInstance) sweep() {
	if !m.leaving.Load() || !m.leaving.Swap(false) {
		return
	}

	for _, state := range m.flight {
		if mailbox := state.mailbox; mailbox != nil && mailbox.stage.Load() == callLeft {
			m.abandon(state, mailbox.cause)
			m.reclaim(state)
		}
	}
}

// reclaim makes the guest forget a call answered on its caller's behalf whose requests have all come back:
// nothing of its own can resume it any more (a TypeScript promise awaiting what nothing, or another call,
// settles). Rather than keep it for good in the flight table and the guest, it drops it now, within its grace;
// the drop runs none of the call's code. A drop that fails breaks the instance, the call to blame.
func (m *muxInstance) reclaim(state *callState) {
	if state.mailbox != nil || state.finished || state.pending > 0 || m.broken.Load() {
		return
	}

	ctx, cancel, ok := m.borrowed(state)
	if !ok {
		return
	}

	defer cancel()

	err := m.inst.Drop(ctx, state.id)
	if err != nil {
		m.fail(state, engine.Result{}, err)

		return
	}

	state.finished = true
	m.forget(state)
}

// settle hands a call back to waiting after a step of its own, under its context, that did not answer it; when
// that context ended meanwhile, its caller waiting for the step's outcome (see callLeaving), it abandons the call.
func (m *muxInstance) settle(state *callState) {
	if mailbox := state.mailbox; mailbox != nil && !mailbox.rest() {
		m.abandon(state, mailbox.cause)
	}
}

// after handles a step run for the call culprit: it starts the requests the step started and answers the calls
// it finished. A failed step, a guest panic in a call or a step naming calls not in flight breaks the instance,
// culprit's call failing with the cause.
func (m *muxInstance) after(culprit *callState, step *engine.Step, err error) {
	switch {
	case err != nil:
		m.fail(culprit, engine.Result{}, err)

		return
	case step.Failure.Status != 0:
		m.fail(culprit, step.Failure, nil)

		return
	}

	if !m.take(culprit, step.Requests) {
		return
	}

	for _, done := range step.Done {
		state := m.flight[done.Call]
		if state == nil || state.finished {
			m.fail(culprit, engine.Result{},
				fmt.Errorf("%w: finished call %d, not in flight", engine.ErrABI, done.Call))

			return
		}

		if done.Result.Status == wire.StatusPanic || done.Result.Status == wire.StatusABI {
			m.fail(state, done.Result, nil)

			return
		}

		m.finish(state, done.Result)
	}

	m.memory.Store(m.inst.Memory())
}

// take takes the requests a step started for culprit: it launches those to make and queues the refusals. Then it
// clears requests: the request goroutines own the bodies, so the instance keeps none until its next step. A
// request of a call not in flight breaks the instance, as after does; take then reports false.
func (m *muxInstance) take(culprit *callState, requests []engine.Request) bool {
	defer clear(requests)

	for _, req := range requests {
		state := m.flight[req.Call]
		if state == nil {
			m.fail(culprit, engine.Result{},
				fmt.Errorf("%w: a request of call %d, not in flight", engine.ErrABI, req.Call))

			return false
		}

		state.pending++

		if req.Answer != nil {
			m.refused = append(m.refused, refusal{state: state, answer: req.Answer, id: req.ID})

			continue
		}

		m.launch(state, req)
	}

	return true
}

// launch makes a request a step started on a goroutine of its own, under its call's context; the answer comes
// back to the owner as a resume operation.
func (m *muxInstance) launch(state *callState, req engine.Request) {
	ctx, id, body, n := state.ctx, req.ID, req.Body, req.N

	m.fetches.Go(func() {
		answer := m.roundTrip(ctx, body, n)
		body = nil // the request is made: while its answer waits for the owner, its body need not

		select {
		case m.ops <- func() { m.resume(state, id, answer) }: //nolint:contextcheck // runs under its call's context
		case <-m.stop:
		}
	})
}

// roundTrip makes one request through the plugin's HTTP capability.
func (m *muxInstance) roundTrip(ctx context.Context, req []byte, n int) []byte {
	if m.pool.http == nil {
		return httpabi.AppendFailure(nil, engine.ErrHTTPDisabled.Error())
	}

	return m.pool.http(ctx, nil, req, n)
}

// finish answers a call the guest ended. Its requests still in flight resume it within muxGrace.
func (m *muxInstance) finish(state *callState, res engine.Result) {
	state.finished = true

	if mailbox := state.mailbox; mailbox != nil {
		mailbox.answer(res, nil, "")

		if state.pending > 0 {
			state.grace = time.Now().Add(muxGrace)
		}
	}

	m.forget(state)
}

// forget drops a call the guest ended once none of its requests is in flight (or waits refused); until then its
// context stays alive for them (they end with it).
func (m *muxInstance) forget(state *callState) {
	if !state.finished {
		return
	}

	state.cancel()

	if state.pending == 0 && m.flight[state.id] == state {
		delete(m.flight, state.id)
		m.inst.EndCall(state.id)
	}
}

// fail breaks the instance: culprit, the call whose step failed (nil when no call is to blame), gets res and
// err, and every other call in flight [ErrInstanceLost]. Their requests end; the refusals waiting are dropped. A
// culprit nobody waits for any more (abandoned at its deadline, or its caller left) is reported here, since no
// caller will.
func (m *muxInstance) fail(culprit *callState, res engine.Result, err error) {
	m.broken.Store(true)

	var cause any = err

	if err == nil {
		cause = guestFailure(res)
	}

	guest := m.inst.Guest()

	if culprit != nil && (culprit.mailbox == nil || !culprit.mailbox.answer(res, err, guest)) {
		m.pool.panics.report(culprit.ctx, &PanicError{
			Key: m.pool.key, Language: m.pool.lang, Phase: PhaseCall, Value: cause, Guest: guest,
		})
	}

	for _, state := range m.flight {
		state.cancel()

		if mailbox := state.mailbox; mailbox != nil {
			mailbox.answer(engine.Result{}, &muxError{
				err: fmt.Errorf("%w: another call on its instance failed: %v", ErrInstanceLost, cause), discard: true,
			}, "")
		}
	}

	clear(m.flight)

	m.refused = nil
}

// begin is the owner's side of a step of the call's own starting, under its context, the call waiting at stage
// from (callQueued for its first step); false when its caller left.
func (c *muxCall) begin(from uint32) bool {
	return c.stage.CompareAndSwap(from, callStepping)
}

// rest is the owner's side of a step of the call's own ending without answering it; false when the call's
// context ended meanwhile, its caller waiting for the answer.
func (c *muxCall) rest() bool {
	return c.stage.CompareAndSwap(callStepping, callWaiting)
}

// answer answers the call with res (its body copied out of guest memory), err and guest, what the guest wrote to
// stderr during the step that failed it, and signals its caller. It reports false, answering nobody, when the
// caller left.
func (c *muxCall) answer(res engine.Result, err error, guest string) bool {
	if c.state != nil {
		c.state.mailbox = nil
		c.state = nil
	}

	if c.stage.Swap(callAnswered) == callLeft {
		return false
	}

	if res.Status == wire.StatusOK {
		c.body = append(c.body[:0], res.Body...)
		res.Body = c.body
	}

	c.result, c.err, c.guest = res, err, guest
	c.done <- struct{}{}

	return true
}

// leave is the caller's side of its context ending with cause. When the call waits, the caller leaves at once (the
// owner answering to nobody), and leave returns the error the call fails with; it returns nil when the caller
// must take the answer, sent or about to be (the call's own step, which the context stops, is running).
func (c *muxCall) leave(cause error) error {
	c.cause = cause // read by the owner once it sees the call left or leaving

	for {
		switch stage := c.stage.Load(); stage {
		case callAnswered:
			return nil
		case callStepping:
			if c.stage.CompareAndSwap(callStepping, callLeaving) {
				return nil
			}
		case callQueued:
			if c.stage.CompareAndSwap(callQueued, callLeft) {
				return unstarted(cause)
			}
		default:
			if c.stage.CompareAndSwap(callWaiting, callLeft) { // the caller sees no other stage
				return abandoned(cause)
			}
		}
	}
}

// Call hands the call to the owner and waits for its answer. When ctx ends first, the call is abandoned: it fails
// with ctx's cause, at once (or once a step of its own, which ctx stops, ends), and the instance serves on.
func (l *muxLane) Call(ctx context.Context, method uint32, args []byte) (engine.Result, error) {
	owner, mailbox := l.m, l.mailbox
	mailbox.guest = ""
	mailbox.stage.Store(callQueued)

	select {
	case owner.ops <- func() { owner.start(ctx, mailbox, method, args) }:
	case <-ctx.Done():
		return engine.Result{}, unstarted(context.Cause(ctx))
	}

	select {
	case <-mailbox.done:
	case <-ctx.Done():
		err := mailbox.leave(context.Cause(ctx))
		if err != nil {
			l.mailbox = newMuxCall() // the owner keeps this one, to answer it to nobody

			owner.left()

			return engine.Result{}, err
		}

		<-mailbox.done
	}

	return mailbox.result, mailbox.err
}

// Broken reports whether a call broke the instance.
func (l *muxLane) Broken() bool {
	return l.m.broken.Load()
}

// Memory returns the instance's linear memory after its last step, for one lane only: the pool adds up its
// lanes.
func (l *muxLane) Memory() uint64 {
	if l.primary {
		return l.m.memory.Load()
	}

	return 0
}

// Guest returns what the guest wrote to stderr during the step that failed the lane's last call.
func (l *muxLane) Guest() string {
	return l.mailbox.guest
}

// Close stops the owner and closes the instance; the pool calls it for the last lane only.
func (l *muxLane) Close(ctx context.Context) error {
	close(l.m.stop)
	<-l.m.exited

	return l.m.inst.Close(ctx) //nolint:wrapcheck // the pool wraps close errors with the instance
}

// release closes the lane, reporting whether it was the instance's last: only then does the pool close the
// instance (Close) and give its place back.
func (l *muxLane) release() bool {
	return l.m.live.Add(-1) == 0
}

// discard breaks the instance on the pool's behalf (a result the host could not decode), failing the other
// calls on it.
func (l *muxLane) discard() {
	if l.m.discarding.CompareAndSwap(false, true) {
		close(l.m.discarded)
	}
}
