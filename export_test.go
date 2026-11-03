package plg

import (
	"context"
	"fmt"
	"io/fs"
	"reflect"
	"time"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/plgimpl"
)

// MuxGrace is how long, in all, a multiplexed call that already ended may run on.
const MuxGrace = muxGrace

// Hold takes an instance of the plugin with the given key, as a call in flight would, until the returned
// release function is called. It lets tests make a plugin busy without a slow plugin method.
func Hold(ctx context.Context, c *Container, key string) (func(), error) {
	held, err := HoldInstance(ctx, c, key)

	return held.Release, err
}

// Held is an instance taken by [HoldInstance]. Call exactly one of Release and Discard.
type Held struct {
	Release func() // gives the instance back
	Discard func() // closes the instance as a broken one; the pool's refiller replaces it
	ID      uint64 // instances of a plugin are numbered from 1 in the order they started
}

// HoldInstance is [Hold], also returning the identity of the instance taken. Like a call, it counts as in
// flight on the generation of the plugin it holds until it is released or discarded.
func HoldInstance(ctx context.Context, c *Container, key string) (Held, error) {
	gen, err := c.serving(key)
	if err != nil {
		return Held{}, err
	}

	held, err := gen.pool.acquire(ctx)
	if err != nil {
		gen.leave()

		return Held{}, err
	}

	return Held{
		ID: held.id,
		Release: func() {
			gen.pool.release(context.WithoutCancel(ctx), held)
			gen.leave()
		},
		Discard: func() {
			gen.pool.discard(context.WithoutCancel(ctx), held)
			gen.leave()
		},
	}, nil
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

// MemoryReserved returns the worst cases of every plugin version the container holds, added up.
func MemoryReserved(c *Container) uint64 {
	return c.budget.reserved.Load()
}

// InstanceMemory returns the linear memory of the live instances of the plugin with the given key, as last
// sampled by its pool.
func InstanceMemory(c *Container, key string) uint64 {
	return uint64(c.catalogue.Load().plugins[key].pool.memory.Load()) //nolint:gosec // G115: never negative
}

// Generation identifies the version of the plugin c serves under key; nil when there is none.
func Generation(c *Container, key string) any {
	gen, ok := c.catalogue.Load().plugins[key]
	if !ok {
		return nil
	}

	return gen
}

// Version returns the version of the plugin c serves under key.
func Version(c *Container, key string) string {
	return c.catalogue.Load().plugins[key].desc.Version
}

// LoadedVersion opens the plugin desc of store and returns the version Register gives it when desc has none:
// the content hash of the snapshot it loads.
func LoadedVersion(ctx context.Context, store Storage, desc Descriptor) (string, error) {
	files, err := store.Open(ctx, desc)
	if err != nil {
		return "", fmt.Errorf("opening: %w", err)
	}

	frozen, err := snapshotFiles(ctx, files)
	if err != nil {
		return "", err
	}

	return contentVersion(frozen, ".")
}

// OpenSwapping opens the plugin directory desc names as s.Open does, calling beforeOpen with the
// plugin-relative name of each directory and file right before the copy opens it, once the walk listed them.
func OpenSwapping(ctx context.Context, s *FSStorage, desc Descriptor, beforeOpen func(name string)) (fs.FS, error) {
	return s.copyPlugin(ctx, desc, beforeOpen)
}

// Retired returns how many replaced or removed plugin versions are not closed yet.
func Retired(c *Container) int {
	return c.retired.Size()
}

// AwaitRetired waits on ctx until every replaced or removed plugin version is closed.
func AwaitRetired(ctx context.Context, c *Container) error {
	for gen := range c.retired.All() {
		select {
		case <-gen.closed:
		case <-ctx.Done():
			return fmt.Errorf("awaiting retired versions: %w", context.Cause(ctx))
		}
	}

	return nil
}

// SetCloseTimeout replaces the time a retired version's instances get to run their Close.
func SetCloseTimeout(c *Container, d time.Duration) {
	c.closeTimeout = d
}

func (p *pool) stats() PoolStats {
	return PoolStats{
		Size:    int(p.size.Load()),
		Idle:    len(p.idle),
		Refills: p.refills.Load(),
		Grows:   p.grows.Load(),
	}
}

// HostHandler returns the host_call handler Register gives the instances of the Go plugin key when it registers
// iface with the WithHost option opt in the container ctr, which reports its panics.
func HostHandler(iface reflect.Type, opt RegisterOption, key string, ctr *Container) (engine.HostFunc, error) {
	contr, err := lookupContract(iface)
	if err != nil {
		return nil, err
	}

	cfg := &registerConfig{}
	opt(cfg)

	hst, err := newHost(contr, cfg.hosts)
	if err != nil {
		return nil, err
	}

	return hst.handler(key, LanguageGo, ctr.panics), nil
}

// UseGenericBinder makes c serve every contract with the generic binder, ignoring the fast paths plg gen
// generated, so tests and benchmarks can hold both binders to the same behavior. Call it before Register.
func UseGenericBinder(c *Container) {
	c.genericBinder.Store(true)
}

// FastAdapter returns the adapter plg gen generated for iface's registered contract, calling inv, or nil when
// the contract has none.
func FastAdapter(iface reflect.Type, inv plgimpl.Invoker) any {
	ct, err := lookupContract(iface)
	if err != nil || ct.fast == nil {
		return nil
	}

	return ct.fast(inv)
}

// GenericCall is the generic binder's call of iface's method index with args (ctx first).
//
//nolint:ireturn // binders hand calls around as plgimpl.Call
func GenericCall(iface reflect.Type, index int, args []reflect.Value) (plgimpl.Call, error) {
	ct, err := lookupContract(iface)
	if err != nil {
		return nil, err
	}

	return &reflectCall{meth: ct.methods[index], args: args}, nil
}

// GenericResults returns the results a call from GenericCall decoded, ending with a nil error.
func GenericResults(call plgimpl.Call) []reflect.Value {
	generic, ok := call.(*reflectCall)
	if !ok {
		return nil
	}

	return generic.out
}

// DecodeAnswer decodes the results of a plugin's successful answer body into call, as every binder does.
func DecodeAnswer(body []byte, call plgimpl.Call) error {
	return decodeResults(body, call)
}
