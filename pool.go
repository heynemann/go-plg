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

// instance is a guest instance as a pool serves it: an *engine.Instance, or a fake in the pool's tests. One
// goroutine at a time owns it.
type instance interface {
	Call(ctx context.Context, method uint32, args []byte) (engine.Result, error)
	Broken() bool
	Close(ctx context.Context) error
}

// slot is an instance taken from a pool, with its argument buffer. Whoever holds the slot owns both; slots
// move through the pool's idle channel, so ownership is handed over without locks.
type slot struct {
	inst instance
	buf  []byte
	id   uint64 // unique within the pool, in start order
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

// poolConfig configures a pool.
type poolConfig struct {
	logger  *slog.Logger
	spawn   func(context.Context) (instance, error) // starts an instance, running the plugin's Create
	budget  *memoryBudget
	key     string
	limit   uint64 // memory limit of each instance, charged to the budget while it lives
	minSize int
	maxSize int
}

// pool serves a plugin's calls from minSize to maxSize instances. A caller takes an idle instance; with none
// idle it starts one while the pool is under maxSize and the budget allows, and otherwise waits on its context.
// A background refiller keeps at least minSize instances alive (more while callers wait and the pool is under
// maxSize), retrying a failing Create with exponential backoff. Broken instances are closed, never reused.
//
// size counts the places taken, by instances alive or being started; a place is reserved (size and budget)
// before an instance starts and given back (retire) when it fails to start or is closed. The idle channel has
// room for maxSize slots, so giving an instance back never blocks.
type pool struct {
	logger  *slog.Logger
	spawn   func(context.Context) (instance, error)
	budget  *memoryBudget
	stop    context.CancelFunc // ends the refiller's context
	idle    chan *slot
	wake    chan struct{} // one pending signal for the refiller (and close): an instance retired
	done    chan struct{} // closed by close
	stopped chan struct{} // closed when the refiller returns
	key     string
	limit   uint64
	ids     atomic.Uint64
	refills atomic.Uint64 // instances the refiller started
	grows   atomic.Uint64 // instances callers started
	minSize int32
	maxSize int32
	size    atomic.Int32
	waiting atomic.Int32 // callers blocked on the idle channel
	closed  atomic.Bool
}

func newPool(cfg poolConfig) *pool {
	return &pool{
		logger:  cfg.logger,
		spawn:   cfg.spawn,
		budget:  cfg.budget,
		key:     cfg.key,
		limit:   cfg.limit,
		minSize: int32(cfg.minSize), //nolint:gosec // G115: pool sizes are validated to at most maxPoolSize
		maxSize: int32(cfg.maxSize), //nolint:gosec // G115: pool sizes are validated to at most maxPoolSize
		idle:    make(chan *slot, cfg.maxSize),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// warm starts the pool's minimum instances, then its refiller. On failure it closes the instances it started
// and returns the error; the pool must not be used afterwards.
func (p *pool) warm(ctx context.Context) error {
	var err error

	for range p.minSize {
		var held *slot

		err = p.reserve()
		if err == nil {
			held, err = p.start(ctx)
		}

		if err != nil {
			break
		}

		p.idle <- held
	}

	if err != nil {
		for p.size.Load() > 0 {
			err = errors.Join(err, p.closeInstance(context.WithoutCancel(ctx), <-p.idle))
		}

		return err
	}

	refillCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	p.stop = stop

	go p.refill(refillCtx)

	return nil
}

// acquire takes an instance for one call: an idle one, a new one while the pool may grow, or the next one
// given back, waiting on ctx.
func (p *pool) acquire(ctx context.Context) (*slot, error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("waiting for an instance: %w", context.Cause(ctx))
	}

	select {
	case held := <-p.idle:
		return p.open(held)
	default:
		return p.await(ctx)
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

		held, err := p.start(ctx)
		if err != nil {
			return nil, fmt.Errorf("starting an instance: %w", err)
		}

		p.grows.Add(1)

		return p.open(held)
	}

	defer p.waiting.Add(-1)

	select {
	case held := <-p.idle:
		return p.open(held)
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for an instance: %w", context.Cause(ctx))
	case <-p.done:
		return nil, ErrClosed
	}
}

// open hands held out, unless the pool closed meanwhile: then it gives held back for close to collect.
func (p *pool) open(held *slot) (*slot, error) {
	if p.closed.Load() {
		p.idle <- held

		return nil, ErrClosed
	}

	return held, nil
}

// release gives an instance back for the next call.
func (p *pool) release(held *slot) {
	p.idle <- held
}

// discard closes a broken instance and gives its place back; the refiller replaces it.
func (p *pool) discard(ctx context.Context, held *slot) {
	err := p.closeInstance(context.WithoutCancel(ctx), held)
	if err != nil {
		p.logger.LogAttrs(ctx, slog.LevelWarn, "plg: closing a discarded instance failed",
			slog.String("key", p.key), slog.Uint64("instance", held.id), slog.String("error", err.Error()))
	}
}

// closeInstance closes an instance and gives its place back.
func (p *pool) closeInstance(ctx context.Context, held *slot) error {
	defer p.retire()

	err := held.inst.Close(ctx)
	if err != nil {
		return fmt.Errorf("closing instance %d: %w", held.id, err)
	}

	return nil
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

// start starts an instance in a reserved place, giving the place back if it fails.
func (p *pool) start(ctx context.Context) (*slot, error) {
	inst, err := p.spawn(ctx)
	if err != nil {
		p.retire()

		return nil, err
	}

	return &slot{inst: inst, id: p.ids.Add(1)}, nil
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

// refill keeps the pool at its minimum until ctx ends. A failed start is logged and retried after an
// exponentially growing delay; a success resets the delay.
func (p *pool) refill(ctx context.Context) {
	defer close(p.stopped)

	var delay time.Duration

	for {
		if !p.needsInstance() {
			select {
			case <-p.wake:
				continue
			case <-ctx.Done():
				return
			}
		}

		err := p.refillOne(ctx)

		switch {
		case err == nil:
			delay = 0
		case ctx.Err() != nil:
			return
		default:
			delay = min(max(2*delay, refillBackoffMin), refillBackoffMax)
			p.logger.LogAttrs(ctx, slog.LevelWarn, "plg: starting an instance to refill the pool failed",
				slog.String("key", p.key), slog.String("error", err.Error()), slog.Duration("retry_in", delay))

			if !sleep(ctx, delay) {
				return
			}
		}
	}
}

// refillOne starts one instance and makes it idle.
func (p *pool) refillOne(ctx context.Context) error {
	err := p.reserve()
	if errors.Is(err, errPoolFull) {
		return nil // callers filled the pool meanwhile
	}

	var held *slot

	if err == nil {
		held, err = p.start(ctx)
	}

	if err != nil {
		return err
	}

	p.refills.Add(1)

	p.idle <- held // if the pool closed meanwhile, close collects it

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
// be given back. Instances still in use when ctx ends are left to the runtime, which closes them with itself.
// It must be called once, after a successful warm.
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
