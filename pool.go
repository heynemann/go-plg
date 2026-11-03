package plg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/heynemann/go-plg/internal/engine"
)

// slot is one place for an instance in a pool. Whoever holds the slot owns its instance (nil until spawned
// again after a discard) and its argument buffer. Slots move through the pool's channel, so ownership is
// handed over without locks.
type slot struct {
	inst *engine.Instance
	buf  []byte
}

// poolSize is the number of instances per plugin: one for now, so a plugin serves one call at a time.
const poolSize = 1

// pool serves a plugin's calls from a fixed number of slots: a caller takes a free slot, waiting on its
// context while all are taken, spawns an instance into it if it is empty, and gives it back. A broken
// instance is closed when given back and its slot left empty, so the next caller spawns a fresh one.
type pool struct {
	logger *slog.Logger
	spawn  func(context.Context) (*engine.Instance, error)
	slots  chan *slot
	done   chan struct{} // closed by close
	key    string
	closed atomic.Bool
}

// newPool returns a pool whose first slot holds first; spawn creates the instances of later slots and the
// replacements of discarded ones.
func newPool(key string, first *engine.Instance, spawn func(context.Context) (*engine.Instance, error),
	logger *slog.Logger,
) *pool {
	created := &pool{
		key:    key,
		spawn:  spawn,
		logger: logger,
		slots:  make(chan *slot, poolSize),
		done:   make(chan struct{}),
	}

	created.slots <- &slot{inst: first}

	return created
}

// acquire takes a slot holding a ready instance.
func (p *pool) acquire(ctx context.Context) (*slot, error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("waiting for an instance: %w", context.Cause(ctx))
	}

	var held *slot

	select {
	case held = <-p.slots:
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for an instance: %w", context.Cause(ctx))
	case <-p.done:
		return nil, ErrClosed
	}

	if p.closed.Load() {
		p.slots <- held

		return nil, ErrClosed
	}

	if held.inst == nil {
		inst, err := p.spawn(ctx)
		if err != nil {
			p.slots <- held

			return nil, fmt.Errorf("replacing a discarded instance: %w", err)
		}

		held.inst = inst
	}

	return held, nil
}

// release gives a slot back with its instance, for the next call.
func (p *pool) release(held *slot) {
	p.slots <- held
}

// discard closes the slot's instance and gives the slot back empty.
func (p *pool) discard(ctx context.Context, held *slot) {
	err := held.inst.Close(context.WithoutCancel(ctx))
	if err != nil {
		p.logger.LogAttrs(ctx, slog.LevelWarn, "plg: closing a discarded instance failed",
			slog.String("key", p.key), slog.String("error", err.Error()))
	}

	held.inst = nil
	p.slots <- held
}

// close stops serving calls and closes every instance, waiting on ctx for the slots in use to come back.
// Instances still in use when ctx ends are left to the runtime, which closes them with itself. It must be
// called once.
func (p *pool) close(ctx context.Context) error {
	p.closed.Store(true)
	close(p.done)

	var errs []error

	for range cap(p.slots) {
		select {
		case held := <-p.slots:
			if held.inst != nil {
				errs = append(errs, held.inst.Close(ctx))
			}
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
