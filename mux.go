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

// muxGrace is how long each step may run for a call that already ended, its answer delivered (or abandoned at its
// deadline): the plugin code after its HTTP requests fail, such as cleanup in a catch or finally block. A step
// running longer is stopped and the instance discarded, as any call stopped at its deadline.
const muxGrace = 250 * time.Millisecond

var (
	errMuxDiscarded = errors.New("the instance was discarded")
	errMuxPanic     = errors.New("the host panicked driving the instance")
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
// answer, so every call has its own argument and result buffers. A step that traps, is stopped, panics or breaks
// the ABI fails its own call with a [PanicError] and every other call on the instance with [ErrInstanceLost];
// the instance is then broken, and its lanes are discarded.
type muxInstance struct {
	inst       stepper // owned by run, then by the Close of the last lane
	pool       *pool
	ops        chan func()
	discarded  chan struct{} // closed when the pool discards a lane of a healthy instance
	stop       chan struct{} // closed by the Close of the last lane
	exited     chan struct{} // closed when run returns
	ready      chan struct{} // closed: what run selects on while refusals wait
	flight     map[uint32]*callState
	refused    []refusal      // owner-only: the refusals to deliver, oldest first
	fetches    sync.WaitGroup // request goroutines
	memory     atomic.Uint64  // the instance's linear memory after its last step
	live       atomic.Int32   // lanes not closed
	broken     atomic.Bool
	discarding atomic.Bool
	last       uint32 // the number of the last call started
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
	mailbox  *muxCall // the waiting caller; nil once answered
	pending  int      // HTTP requests not answered yet
	id       uint32
	finished bool // the guest ended the call
}

// muxCall is a lane's mailbox: the caller fills it, the owner answers it once per call and signals done.
type muxCall struct {
	err    error
	state  *callState // owner-only: the call in flight, nil once answered
	done   chan struct{}
	guest  string // what the guest wrote to stderr during a failed step
	body   []byte // the result's body, copied out of guest memory
	result engine.Result
}

// muxLane is one of the calls a multiplexed instance serves at once, as the pool hands it out.
type muxLane struct {
	m       *muxInstance
	mailbox muxCall
	primary bool // reports the instance's memory; the other lanes report none
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
	}
	close(mux.ready)
	mux.memory.Store(inst.Memory())
	mux.live.Store(int32(lanes)) //nolint:gosec // G115: lanes are validated to at most maxConcurrentCalls

	out := make([]*muxLane, lanes)
	for i := range out {
		out[i] = &muxLane{m: mux, primary: i == 0, mailbox: muxCall{done: make(chan struct{}, 1)}}
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

// start starts a call, answering it at once when the call's context has ended or the instance is broken.
func (m *muxInstance) start(ctx context.Context, mailbox *muxCall, method uint32, args []byte) {
	if ctx.Err() != nil {
		mailbox.answer(engine.Result{}, &muxError{
			err: fmt.Errorf("waiting for its instance: %w", context.Cause(ctx)),
		})

		return
	}

	if m.broken.Load() {
		mailbox.answer(engine.Result{}, &muxError{
			err: fmt.Errorf("%w: %w", ErrInstanceLost, errMuxDiscarded), discard: true,
		})

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
}

// abandon answers a call whose caller's context ended (with cause) while it waited, and ends its requests; the
// guest then runs the rest of the call (with the failed requests) under muxGrace per step.
func (*muxInstance) abandon(mailbox *muxCall, cause error) {
	state := mailbox.state
	if state == nil { // answered meanwhile
		return
	}

	state.cancel()
	mailbox.answer(engine.Result{}, &muxError{
		err: fmt.Errorf("abandoned while waiting for its HTTP requests: %w", cause),
	})
}

// resume delivers a request's answer to the guest. When the call's context has ended meanwhile, its caller is
// abandoning it (or will find it answered): it is answered now, so that the step does not run under an ended
// context, which would stop the whole instance.
func (m *muxInstance) resume(state *callState, request uint32, answer []byte) {
	state.pending--

	defer m.forget(state)

	if m.broken.Load() {
		return
	}

	if mailbox := state.mailbox; mailbox != nil && state.ctx.Err() != nil {
		m.abandon(mailbox, context.Cause(state.ctx))
	}

	ctx := state.ctx
	if state.mailbox == nil { // the call is answered: the rest of it runs on borrowed time
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(context.WithoutCancel(state.ctx), muxGrace)
		defer cancel()
	}

	step, err := m.inst.Resume(ctx, request, answer)
	m.after(state, &step, err)
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

// finish answers a call the guest ended, copying its result out of guest memory.
func (m *muxInstance) finish(state *callState, res engine.Result) {
	state.finished = true

	if mailbox := state.mailbox; mailbox != nil {
		if res.Status == wire.StatusOK {
			mailbox.body = append(mailbox.body[:0], res.Body...)
			res.Body = mailbox.body
		}

		mailbox.answer(res, nil)
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
// culprit nobody waits for any more (abandoned at its deadline) is reported here, since no caller will.
func (m *muxInstance) fail(culprit *callState, res engine.Result, err error) {
	m.broken.Store(true)

	var cause any = err

	if err == nil {
		cause = guestFailure(res)
	}

	guest := m.inst.Guest()

	if culprit != nil && culprit.mailbox == nil {
		m.pool.panics.report(culprit.ctx, &PanicError{
			Key: m.pool.key, Language: m.pool.lang, Phase: PhaseCall, Value: cause, Guest: guest,
		})
	}

	for _, state := range m.flight {
		state.cancel()

		mailbox := state.mailbox
		if mailbox == nil {
			continue
		}

		if state == culprit {
			mailbox.guest = guest
			mailbox.answer(res, err)

			continue
		}

		mailbox.answer(engine.Result{}, &muxError{
			err: fmt.Errorf("%w: another call on its instance failed: %v", ErrInstanceLost, cause), discard: true,
		})
	}

	clear(m.flight)

	m.refused = nil
}

// answer answers the call and signals its caller.
func (c *muxCall) answer(res engine.Result, err error) {
	if c.state != nil {
		c.state.mailbox = nil
		c.state = nil
	}

	c.result, c.err = res, err
	c.done <- struct{}{}
}

// Call hands the call to the owner and waits for its answer. When ctx ends first, the call is abandoned: it
// fails with ctx's cause, and the instance serves on.
func (l *muxLane) Call(ctx context.Context, method uint32, args []byte) (engine.Result, error) {
	owner, mailbox := l.m, &l.mailbox
	mailbox.guest = ""

	select {
	case owner.ops <- func() { owner.start(ctx, mailbox, method, args) }:
	case <-ctx.Done():
		return engine.Result{}, &muxError{err: fmt.Errorf("waiting for its instance: %w", context.Cause(ctx))}
	}

	select {
	case <-mailbox.done:
	case <-ctx.Done():
		select {
		case owner.ops <- func() { owner.abandon(mailbox, context.Cause(ctx)) }:
			<-mailbox.done
		case <-mailbox.done:
		}
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
