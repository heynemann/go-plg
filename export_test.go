package plg

import "context"

// Hold takes an instance of the plugin with the given key, as a call in flight would, until the returned
// release function is called. It lets tests make a plugin busy without a slow plugin method.
func Hold(ctx context.Context, c *Container, key string) (func(), error) {
	held, err := HoldInstance(ctx, c, key)

	return held.Release, err
}

// Held is an instance taken by [HoldInstance].
type Held struct {
	Release func() // gives the instance back
	ID      uint64 // instances of a plugin are numbered from 1 in the order they started
}

// HoldInstance is [Hold], also returning the identity of the instance taken.
func HoldInstance(ctx context.Context, c *Container, key string) (Held, error) {
	plug, ok := c.catalogue.Load().plugins[key]
	if !ok {
		return Held{}, ErrNotFound
	}

	held, err := plug.pool.acquire(ctx)
	if err != nil {
		return Held{}, err
	}

	return Held{ID: held.id, Release: func() { plug.pool.release(held) }}, nil
}

// PoolStats describes a plugin's pool.
type PoolStats struct {
	Size    int    // instances alive or starting
	Idle    int    // instances waiting for a call
	Refills uint64 // instances started by the refiller
	Grows   uint64 // instances started by callers
}

// Stats returns the pool of the plugin with the given key.
func Stats(c *Container, key string) PoolStats {
	return c.catalogue.Load().plugins[key].pool.stats()
}

// MemoryInUse returns the memory limits of every instance alive in the container, added up.
func MemoryInUse(c *Container) uint64 {
	return c.budget.used.Load()
}

func (p *pool) stats() PoolStats {
	return PoolStats{
		Size:    int(p.size.Load()),
		Idle:    len(p.idle),
		Refills: p.refills.Load(),
		Grows:   p.grows.Load(),
	}
}
