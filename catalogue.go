package plg

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"maps"
	"reflect"
	"slices"

	"github.com/heynemann/go-plg/internal/httpcap"
)

// plugin is a loaded plugin as the catalogue serves it.
type plugin struct {
	value any          // the bound adapter; it implements iface
	iface reflect.Type // the interface it was registered for
	pool  *pool
	http  *httpcap.Client // the Register call's, shared with its other plugins
	key   string          // "<name>/<descriptor key>"
	lang  Language
}

// snapshot is an immutable state of the catalogue. Register publishes a new one with compare-and-swap, so
// readers never wait and always see a consistent catalogue.
type snapshot struct {
	plugins map[string]*plugin
	byIface map[reflect.Type][]*plugin // sorted by key
	closed  bool
}

// with returns a snapshot that adds plugins, all registered for iface, to s.
func (s *snapshot) with(iface reflect.Type, added []*plugin) *snapshot {
	next := &snapshot{
		plugins: make(map[string]*plugin, len(s.plugins)+len(added)),
		byIface: maps.Clone(s.byIface),
	}

	maps.Copy(next.plugins, s.plugins)

	for _, p := range added {
		next.plugins[p.key] = p
	}

	if len(added) > 0 {
		if next.byIface == nil {
			next.byIface = map[reflect.Type][]*plugin{}
		}

		list := slices.Concat(s.byIface[iface], added)
		slices.SortFunc(list, func(a, b *plugin) int { return cmp.Compare(a.key, b.key) })

		next.byIface[iface] = list
	}

	return next
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
