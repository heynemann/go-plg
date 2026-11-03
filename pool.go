package plg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/heynemann/go-plg/internal/engine"
)

// instance is a guest instance as a pool serves it: a *guestInstance, or a fake in the pool's tests. One
// goroutine at a time owns it.
type instance interface {
	Call(ctx context.Context, method uint32, args []byte) (engine.Result, error)
	Broken() bool
	Close(ctx context.Context) error
	// Guest returns what the guest wrote to stderr during its last call or close.
	Guest() string
	// Memory returns the size of the instance's linear memory in bytes.
	Memory() uint64
}

// slot is an instance taken from a pool, with its argument buffer. Whoever holds the slot owns both; slots
// move through the pool's idle channel, so ownership is handed over without locks.
type slot struct {
	inst instance
	buf  []byte
	id   uint64 // unique within the pool, in start order
	mem  uint64 // the instance's memory when last sampled, counted in the pool's memory
}

// The refiller retries a failing Create after refillBackoffMin, doubling the delay up to refillBackoffMax.
const (
	refillBackoffMin = 100 * time.Millisecond
	refillBackoffMax = 30 * time.Second
)

var (
	errPoolFull   = errors.New("the pool is at its maximum")
	errBudgetFull = fmt.Errorf("%w: no room for another instance", ErrMemoryBudget)
)

// poolConfig configures a pool. The pool logs through its panic reporter's logger.
type poolConfig struct {
	panics       *panicReporter
	spawn        func(context.Context) (instance, error) // starts an instance, running the plugin's Create
	http         engine.HTTPFunc                         // makes the requests of multiplexed instances
	budget       *memoryBudget
	meters       *pluginMeters
	key          string
	lang         Language
	limit        uint64 // memory limit of each instance, charged to the budget while it lives
	minSize      int32
	maxSize      int32
	lanes        int32         // calls each instance serves at once (see mux.go); 1 for instances that do not multiplex
	closeTimeout time.Duration // bounds the plugin's Close of each instance a failed warm started
}

// pool serves a plugin's calls from minSize to maxSize instances. A caller takes an idle instance; with none
// idle it starts one while the pool is under maxSize and the budget allows, and otherwise waits on its context.
// A background refiller keeps at least minSize instances alive (more while callers wait and the pool is under
// maxSize), retrying a failing Create with exponential backoff. Broken instances are closed, never reused. A
// panic while starting or closing an instance, or in the refiller, is recovered and reported; so is a panic in
// the logger's handler or the meter provider's instruments, which the pool measures and logs through after its
// own state is settled.
//
// An instance that multiplexes (lanes > 1, a script guest) serves up to lanes calls at once: it is handed out as
// that many slots, its lanes, which move through the pool like instances. Closing a lane closes the instance only
// when it is the last; until then the others serve on, or, when the instance broke, are discarded as callers
// take them.
//
// size counts the places taken, by instances alive or being started; a place is reserved (size and budget)
// before an instance starts and given back (retire) when it fails to start or is closed. The idle channel has
// room for maxSize × lanes slots, so giving an instance back never blocks.
type pool struct {
	logger       *slog.Logger
	panics       *panicReporter
	spawn        func(context.Context) (instance, error)
	http         engine.HTTPFunc
	budget       *memoryBudget
	meters       *pluginMeters
	stop         context.CancelFunc // ends the refiller's context
	idle         chan *slot
	wake         chan struct{} // one pending signal for the refiller (and close): an instance retired
	done         chan struct{} // closed by close
	stopped      chan struct{} // closed when the refiller returns
	key          string
	lang         Language
	limit        uint64
	closeTimeout time.Duration // bounds the plugin's Close of each instance a failed warm started
	ids          atomic.Uint64
	refills      atomic.Uint64 // instances the refiller started
	grows        atomic.Uint64 // instances callers started
	memory       atomic.Int64  // linear memory of the live instances, as last sampled: at start and on each release
	minSize      int32
	maxSize      int32
	lanes        int32
	size         atomic.Int32
	busy         atomic.Int32 // slots handed out to callers: calls in flight
	inUse        atomic.Int32 // instances with a slot handed out: serving at least one call
	waiting      atomic.Int32 // callers blocked on the idle channel
	closed       atomic.Bool
	abandoned    atomic.Bool // close gave up waiting: slots given back are closed at once
}

func newPool(cfg *poolConfig) *pool {
	lanes := max(cfg.lanes, 1)

	return &pool{
		logger:       cfg.panics.logger,
		panics:       cfg.panics,
		spawn:        cfg.spawn,
		http:         cfg.http,
		budget:       cfg.budget,
		meters:       cfg.meters,
		key:          cfg.key,
		lang:         cfg.lang,
		limit:        cfg.limit,
		minSize:      cfg.minSize,
		maxSize:      cfg.maxSize,
		closeTimeout: cfg.closeTimeout,
		lanes:        lanes,
		idle:         make(chan *slot, cfg.maxSize*lanes),
		wake:         make(chan struct{}, 1),
		done:         make(chan struct{}),
		stopped:      make(chan struct{}),
	}
}

// warm starts the pool's minimum instances, then its refiller. On failure it closes the instances it started,
// each running the plugin's Close under the close timeout, and returns the error; the pool must not be used
// afterwards.
func (p *pool) warm(ctx context.Context) error {
	var err error

	for range p.minSize {
		var held *slot

		err = p.reserve()
		if err == nil {
			held, err = p.start(ctx, PhaseLoad)
		}

		if err != nil {
			break
		}

		p.idle <- held
	}

	if err != nil {
		for p.size.Load() > 0 {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.closeTimeout)
			err = errors.Join(err, p.closeInstance(closeCtx, <-p.idle))

			cancel()
		}

		return err
	}

	refillCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	p.stop = stop

	go p.refill(refillCtx)

	return nil
}

// acquire takes an instance for one call: an idle one, a new one while the pool may grow, or the next one
// given back, waiting on ctx. Lanes of a multiplexed instance that broke meanwhile are discarded on the way.
func (p *pool) acquire(ctx context.Context) (*slot, error) {
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("waiting for an instance: %w", context.Cause(ctx))
		}

		var (
			held *slot
			err  error
		)

		select {
		case held = <-p.idle:
			held, err = p.open(ctx, held)
		default:
			held, err = p.await(ctx)
		}

		if err != nil || !held.inst.Broken() {
			return held, err
		}

		p.discard(ctx, held)
	}
}

// await is acquire's slow path, with no instance idle. The caller announces itself as waiting before trying to
// grow the pool: a retire that the failed attempt missed then sees the waiter and wakes the refiller, which
// starts the instance the caller could not.
func (p *pool) await(ctx context.Context) (*slot, error) {
	if p.closed.Load() {
		return nil, ErrClosed
	}

	p.waiting.Add(1)

	if p.reserve() == nil {
		p.waiting.Add(-1)

		held, err := p.start(ctx, PhaseCall)
		if err != nil {
			return nil, fmt.Errorf("starting an instance: %w", err)
		}

		p.grows.Add(1)

		return p.open(ctx, held)
	}

	defer p.waiting.Add(-1)

	p.count(ctx, p.meters.inst.waits, p.meters.key)

	select {
	case held := <-p.idle:
		return p.open(ctx, held)
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for an instance: %w", context.Cause(ctx))
	case <-p.done:
		return nil, ErrClosed
	}
}

// open hands held out, unless the pool closed meanwhile: then it gives held back for close to collect (or closes
// it, once close gave up waiting).
func (p *pool) open(ctx context.Context, held *slot) (*slot, error) {
	if p.closed.Load() {
		p.idle <- held

		p.closeAbandoned(ctx)

		return nil, ErrClosed
	}

	p.engage(held)

	return held, nil
}

// release gives an instance back for the next call, sampling its memory first: memory grows during calls. Once
// close gave up waiting for it, the instance is closed instead.
func (p *pool) release(ctx context.Context, held *slot) {
	if mem := held.inst.Memory(); mem != held.mem {
		p.memory.Add(int64(mem) - int64(held.mem)) //nolint:gosec // G115: wasm32 memory is at most 4 GiB

		held.mem = mem
	}

	p.disengage(held)

	p.idle <- held

	p.closeAbandoned(ctx)
}

// discard closes a broken instance and gives its place back; the refiller replaces it. A lane of a multiplexed
// instance breaks the instance, failing the other calls on it; the instance closes with its last lane. A broken
// instance closes under an ended context, so without the plugin's Close: whatever broke it (a trap, running out
// of memory, a result the host could not decode) may leave Close unable to end.
func (p *pool) discard(ctx context.Context, held *slot) {
	p.disengage(held)

	if lane, ok := held.inst.(*muxLane); ok && !lane.Broken() {
		lane.discard()
	}

	ended, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cancel()

	err := p.closeInstance(ended, held)
	if err != nil {
		p.guard(ctx, logMethod, func() {
			p.logger.LogAttrs(ctx, slog.LevelWarn, "plg: closing a discarded instance failed",
				slog.String("key", p.key), slog.Uint64("instance", held.id), slog.String("error", err.Error()))
		})
	}
}

// closeInstance closes an instance and gives its place back; a lane of a multiplexed instance closes it only
// when it is the last. A panic meanwhile is reported as a [PanicError] in [PhaseClose] and returned.
func (p *pool) closeInstance(ctx context.Context, held *slot) (err error) {
	p.memory.Add(-int64(held.mem)) //nolint:gosec // G115: wasm32 memory is at most 4 GiB

	if lane, ok := held.inst.(*muxLane); ok && !lane.release() {
		return nil
	}

	defer p.retire()
	defer func() {
		if v := recover(); v != nil {
			failure := &PanicError{Key: p.key, Language: p.lang, Phase: PhaseClose}
			p.panics.recovered(ctx, failure, v)

			err = failure
		}
	}()

	err = held.inst.Close(ctx)
	if err != nil {
		return fmt.Errorf("closing instance %d: %w", held.id, err)
	}

	return nil
}

// closeIdle closes the instances idle now.
func (p *pool) closeIdle(ctx context.Context) {
	for {
		select {
		case held := <-p.idle:
			err := p.closeInstance(ctx, held)
			if err != nil {
				p.guard(ctx, logMethod, func() {
					p.logger.LogAttrs(ctx, slog.LevelWarn, "plg: closing an abandoned instance failed",
						slog.String("key", p.key), slog.Uint64("instance", held.id), slog.String("error", err.Error()))
				})
			}
		default:
			return
		}
	}
}

// closeAbandoned closes the instances idle now once close gave up waiting for them. They close under an ended
// context, so without the plugin's Close, which nobody waits for any more.
func (p *pool) closeAbandoned(ctx context.Context) {
	if !p.abandoned.Load() {
		return
	}

	ended, cancel := context.WithCancel(ctx)
	cancel()
	p.closeIdle(ended)
}

// engage counts held as handed out to a caller, and its instance as in use unless another of its lanes is.
func (p *pool) engage(held *slot) {
	p.busy.Add(1)

	if lane, ok := held.inst.(*muxLane); !ok || lane.m.busy.Add(1) == 1 {
		p.inUse.Add(1)
	}
}

// disengage counts held as given back, and its instance as no longer in use unless another of its lanes is.
func (p *pool) disengage(held *slot) {
	p.busy.Add(-1)

	if lane, ok := held.inst.(*muxLane); !ok || lane.m.busy.Add(-1) == 0 {
		p.inUse.Add(-1)
	}
}

// guard runs fn, the user's code (named method) the pool calls: the logger's handler or the meter provider's
// instruments. A panic in it is reported as a [PanicError] in [PhaseCallback].
func (p *pool) guard(ctx context.Context, method string, fn func()) {
	p.panics.callback(ctx, p.key, p.lang, method, fn)
}

// reserve takes a place for a new instance: [errPoolFull] at the pool's maximum, [errBudgetFull] when the
// container's memory budget has no room for one more instance.
func (p *pool) reserve() error {
	for {
		n := p.size.Load()
		if n >= p.maxSize {
			return errPoolFull
		}

		if p.size.CompareAndSwap(n, n+1) {
			break
		}
	}

	if !p.budget.acquire(p.limit) {
		p.size.Add(-1)
		p.signal() // a caller may have seen the pool full because of this place

		return errBudgetFull
	}

	return nil
}

// start starts an instance in a reserved place, giving the place back if it fails. A panic while starting it
// is reported as a [PanicError] in the given phase and returned.
func (p *pool) start(ctx context.Context, phase Phase) (*slot, error) {
	inst, err := p.spawnGuarded(ctx, phase)
	if err != nil {
		p.retire()

		return nil, err
	}

	held := &slot{inst: inst, id: p.ids.Add(1), mem: inst.Memory()}
	p.memory.Add(int64(held.mem)) //nolint:gosec // G115: wasm32 memory is at most 4 GiB

	if st, ok := inst.(stepper); ok && p.lanes > 1 && st.Multiplexes() {
		lanes := newMux(p, st, int(p.lanes)) //nolint:contextcheck // the owner outlives this call; it serves many
		held.inst = lanes[0]

		for _, lane := range lanes[1:] {
			p.idle <- &slot{inst: lane, id: held.id}
		}
	}

	return held, nil
}

//nolint:ireturn // it returns what spawn returns: the pool's instance interface
func (p *pool) spawnGuarded(ctx context.Context, phase Phase) (instance, error) {
	var (
		inst instance
		err  error
	)

	func() {
		defer func() {
			if v := recover(); v != nil {
				failure := &PanicError{Key: p.key, Language: p.lang, Phase: phase}
				p.panics.recovered(ctx, failure, v)

				inst, err = nil, failure
			}
		}()

		inst, err = p.spawn(ctx)
	}()

	return inst, err
}

// retire gives back the place of an instance that was closed or failed to start.
func (p *pool) retire() {
	p.size.Add(-1)
	p.budget.release(p.limit)
	p.signal()
}

func (p *pool) signal() {
	select {
	case p.wake <- struct{}{}:
	default: // a signal is already pending
	}
}

// needsInstance reports whether the refiller should start an instance: the pool is under its minimum, or
// callers are waiting while it is under its maximum.
func (p *pool) needsInstance() bool {
	n := p.size.Load()

	return n < p.minSize || (n < p.maxSize && p.waiting.Load() > 0)
}

// refill keeps the pool at its minimum until ctx ends.
func (p *pool) refill(ctx context.Context) {
	defer close(p.stopped)

	var delay time.Duration

	for p.refillStep(ctx, &delay) {
	}
}

// refillStep waits until the pool needs an instance and starts one. A failed start is logged and followed by
// an exponentially growing delay; a success resets the delay. A panic is reported as a [PanicError] in
// [PhaseRefill] and treated as a failure. It reports whether the refiller goes on.
func (p *pool) refillStep(ctx context.Context, delay *time.Duration) bool {
	more, panicked := true, true

	func() {
		defer func() {
			if v := recover(); v != nil {
				p.panics.recovered(ctx, &PanicError{Key: p.key, Language: p.lang, Phase: PhaseRefill}, v)
			}
		}()

		more = p.refillOnce(ctx, delay)
		panicked = false
	}()

	if panicked {
		*delay = backoff(*delay)

		return sleep(ctx, *delay)
	}

	return more
}

// refillOnce is refillStep without its panic boundary.
func (p *pool) refillOnce(ctx context.Context, delay *time.Duration) bool {
	if !p.needsInstance() {
		select {
		case <-p.wake:
			return true
		case <-ctx.Done():
			return false
		}
	}

	err := p.refillOne(ctx)

	switch {
	case err == nil:
		*delay = 0

		return true
	case ctx.Err() != nil:
		return false
	default:
		p.count(ctx, p.meters.inst.refills, p.meters.refillFailed)

		*delay = backoff(*delay)

		p.guard(ctx, logMethod, func() {
			p.logger.LogAttrs(ctx, slog.LevelWarn, "plg: starting an instance to refill the pool failed",
				slog.String("key", p.key), slog.String("error", err.Error()), slog.Duration("retry_in", *delay))
		})

		return sleep(ctx, *delay)
	}
}

// backoff returns the delay after a failure that followed one delay.
func backoff(delay time.Duration) time.Duration {
	return min(max(2*delay, refillBackoffMin), refillBackoffMax)
}

// refillOne starts one instance and makes it idle.
func (p *pool) refillOne(ctx context.Context) error {
	err := p.reserve()
	if errors.Is(err, errPoolFull) {
		return nil // callers filled the pool meanwhile
	}

	var held *slot

	if err == nil {
		held, err = p.start(ctx, PhaseRefill)
	}

	if err != nil {
		return err
	}

	p.refills.Add(1)

	p.idle <- held // if the pool closed meanwhile, close collects it

	p.count(ctx, p.meters.inst.refills, p.meters.refillOK) // once idle: a panicking instrument cannot keep it

	return nil
}

// sleep waits for d or for ctx to end, reporting whether d elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// close stops serving calls and the refiller, then closes every instance, waiting on ctx for those in use to
// be given back. When ctx ends first, the instances still in use are closed as they are given back, without
// the plugin's Close (the runtime frees those never given back with itself). It must be called once, after a
// successful warm.
func (p *pool) close(ctx context.Context) error {
	p.closed.Store(true)
	close(p.done)
	p.stop()
	<-p.stopped // the refiller's context is done, which also stops an instance it is starting

	var errs []error

	for p.size.Load() > 0 {
		select {
		case held := <-p.idle:
			errs = append(errs, p.closeInstance(ctx, held))
		case <-p.wake: // an instance in use was discarded
		case <-ctx.Done():
			p.abandoned.Store(true)
			p.closeIdle(ctx) // what release gave back before it could see abandoned

			return fmt.Errorf("plg: draining %s: %w", p.key, context.Cause(ctx))
		}
	}

	err := errors.Join(errs...)
	if err != nil {
		return fmt.Errorf("plg: closing %s: %w", p.key, err)
	}

	return nil
}

// memoryBudget accounts the container's memory budget in bytes. Register reserves the worst case of every
// plugin it loads; pools charge each instance's limit while it lives. A zero total means no budget: everything
// is counted and nothing refused.
type memoryBudget struct {
	total    uint64
	reserved atomic.Uint64
	used     atomic.Uint64
}

// reserve reserves a plugin's worst case, reporting whether the budget had room for it.
func (b *memoryBudget) reserve(n uint64) bool {
	return take(&b.reserved, n, b.total)
}

// unreserve gives a reservation back.
func (b *memoryBudget) unreserve(n uint64) {
	b.reserved.Add(-n)
}

// acquire charges an instance's limit, reporting whether the budget had room for it.
func (b *memoryBudget) acquire(n uint64) bool {
	return take(&b.used, n, b.total)
}

// release gives an instance's charge back.
func (b *memoryBudget) release(n uint64) {
	b.used.Add(-n)
}

// take adds n to counter unless that would exceed a non-zero total.
func take(counter *atomic.Uint64, n, total uint64) bool {
	if total == 0 {
		counter.Add(n)

		return true
	}

	for {
		cur := counter.Load()
		if n > total-cur {
			return false
		}

		if counter.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}
