package plg

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"maps"
	"reflect"
	"slices"
	"sync/atomic"
)

// plugin is one generation of a loaded plugin: what the catalogue serves under its key until a reload replaces
// or removes it. Each call counts itself in flight on the generation it runs on, so a retired generation is
// closed only once its last call has left.
type plugin struct {
	value    any          // the key's adapter, shared by its generations; it implements iface
	iface    reflect.Type // the interface it was registered for
	closeErr error        // closing the retired generation's instances failed; set before closed is closed
	pool     *pool
	idle     chan struct{} // closed once the generation is retired and no call is in flight
	closed   chan struct{} // closed once a retired generation's instances are closed
	key      string        // "<name>/<descriptor key>"
	desc     Descriptor    // as loaded, its Version resolved
	reserved uint64        // its worst case in the container's memory budget, given back once it is closed
	state    atomic.Int64  // calls in flight, plus retiredBit once retired
	quiet    atomic.Bool   // idle is closed
}

// retiredBit marks a retired generation in plugin.state, far above any count of calls in flight.
const retiredBit = 1 << 62

// newGeneration returns a generation of the plugin key served by instances.
func newGeneration(key string, iface reflect.Type, desc *Descriptor, instances *pool) *plugin {
	return &plugin{
		key:    key,
		iface:  iface,
		desc:   *desc,
		pool:   instances,
		idle:   make(chan struct{}),
		closed: make(chan struct{}),
	}
}

// enter counts a call in flight, unless the generation is retired: the caller must then find the generation
// that replaced it in a later snapshot.
func (p *plugin) enter() bool {
	if p.state.Add(1)&retiredBit != 0 {
		p.leave()

		return false
	}

	return true
}

// leave ends a call that entered.
func (p *plugin) leave() {
	if p.state.Add(-1) == retiredBit {
		p.quieten()
	}
}

// retire stops the generation taking calls; idle is closed once those in flight have left. The catalogue must
// no longer hold it, so that callers find its successor.
func (p *plugin) retire() {
	if p.state.Or(retiredBit) == 0 {
		p.quieten()
	}
}

// quieten closes idle, once: both the last leave after retire and a leave of a call that entered too late may
// see the count reach zero.
func (p *plugin) quieten() {
	if p.quiet.CompareAndSwap(false, true) {
		close(p.idle)
	}
}

// snapshot is an immutable state of the catalogue. Register and reloads publish a new one with
// compare-and-swap, so readers never wait and always see a consistent catalogue.
type snapshot struct {
	plugins map[string]*plugin
	byIface map[reflect.Type][]*plugin // sorted by key
	closed  bool
}

// with returns a snapshot changing the plugins of iface in s: the plugins in put are added or replace those with
// the same key, and those whose keys are in removed are gone.
func (s *snapshot) with(iface reflect.Type, put []*plugin, removed []string) *snapshot {
	next := &snapshot{
		plugins: make(map[string]*plugin, len(s.plugins)+len(put)),
		byIface: make(map[reflect.Type][]*plugin, len(s.byIface)+1),
	}

	maps.Copy(next.plugins, s.plugins)
	maps.Copy(next.byIface, s.byIface)

	for _, key := range removed {
		delete(next.plugins, key)
	}

	for _, p := range put {
		next.plugins[p.key] = p
	}

	var list []*plugin

	for _, p := range next.plugins {
		if p.iface == iface {
			list = append(list, p)
		}
	}

	slices.SortFunc(list, func(a, b *plugin) int { return cmp.Compare(a.key, b.key) })

	if len(list) == 0 {
		delete(next.byIface, iface)
	} else {
		next.byIface[iface] = list
	}

	return next
}

// serving returns the generation c serves under key, with a call entered: the caller leaves it when done. It
// fails with [ErrNotFound] for an unknown key and with [ErrClosed] once the container is closed.
func (c *Container) serving(key string) (*plugin, error) {
	for {
		snap := c.catalogue.Load()

		gen, found := snap.plugins[key]
		switch {
		case found && gen.enter():
			return gen, nil
		case found: // retired after snap was loaded, which happens only once a later snapshot is published
		case snap.closed:
			return nil, ErrClosed
		default:
			return nil, ErrNotFound
		}
	}
}

// Get returns the plugin with the given key, "<name>/<descriptor key>", as a T. It fails with [ErrNotFound]
// for an unknown key, with [ErrTypeMismatch] when the key's plugin was registered for another interface than
// T, and with [ErrClosed] once the container is closed. Finding a plugin does not allocate. It is safe for
// concurrent use.
//
//nolint:ireturn // Get returns the caller's interface type T by design
func Get[T any](_ context.Context, c *Container, key string) (T, error) {
	var zero T

	snap := c.catalogue.Load()

	plug, found := snap.plugins[key]
	if found && plug.iface == reflect.TypeFor[T]() {
		v, ok := plug.value.(T)
		if ok {
			return v, nil
		}
	}

	switch {
	case found:
		return zero, fmt.Errorf("%w: %s was registered for %s, not %s", ErrTypeMismatch, key, plug.iface,
			reflect.TypeFor[T]())
	case snap.closed:
		return zero, ErrClosed
	default:
		return zero, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
}

// All returns every plugin registered for T, with its key, in key order. Each iteration runs over the
// catalogue as it was when the iteration started: plugins registered meanwhile are not seen. It does not
// allocate and is safe for concurrent use.
func All[T any](_ context.Context, c *Container) iter.Seq2[string, T] {
	// All stays this small, and the snapshot is loaded by the iterator, so that All inlines (even with the
	// race detector) and the compiler sees the loop body cannot escape: ranging over it does not allocate.
	return func(yield func(string, T) bool) {
		for _, p := range c.catalogue.Load().byIface[reflect.TypeFor[T]()] {
			v, ok := p.value.(T)
			if ok && !yield(p.key, v) {
				return
			}
		}
	}
}
