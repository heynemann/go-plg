package plg

import "sync/atomic"

// ring keeps the newest values recorded in it, at most one per slot, without locks: a writer takes the next
// sequence number and stores its entry in that number's slot, unless a newer entry holds it already; a reader
// keeps only the entries whose slot still holds the sequence number it expects, so entries overwritten or not yet
// stored meanwhile are left out. The panic history ([Container.RecentPanics]) and each listed plugin's events
// ([Events]) are rings. A ring without slots keeps nothing.
type ring[T any] struct {
	slots []atomic.Pointer[ringEntry[T]]
	next  atomic.Uint64 // sequence number of the next entry
}

type ringEntry[T any] struct {
	value T
	seq   uint64
}

// newRing returns a ring keeping the newest size values (none for zero or less).
func newRing[T any](size int) ring[T] {
	return ring[T]{slots: make([]atomic.Pointer[ringEntry[T]], max(size, 0))}
}

// record keeps value as the newest entry.
func (r *ring[T]) record(value T) {
	if len(r.slots) == 0 {
		return
	}

	r.store(r.next.Add(1)-1, value)
}

// store puts value, numbered seq, in its slot. A writer delayed between taking its number and storing may find
// the slot taken by an entry recorded after its own, one ring length later: it leaves that newer entry in place
// (its own is out of the ring already), so that the ring holds the newest entries once writers are done.
func (r *ring[T]) store(seq uint64, value T) {
	slot, entry := &r.slots[seq%uint64(len(r.slots))], &ringEntry[T]{value: value, seq: seq}

	for {
		held := slot.Load()
		if held != nil && held.seq > seq {
			return
		}

		if slot.CompareAndSwap(held, entry) {
			return
		}
	}
}

// recent returns the newest entries, oldest first; nil when there are none.
func (r *ring[T]) recent() []T {
	size := uint64(len(r.slots))
	end := r.next.Load()

	if size == 0 || end == 0 {
		return nil
	}

	start := end - min(end, size)
	out := make([]T, 0, end-start)

	for seq := start; seq < end; seq++ {
		entry := r.slots[seq%size].Load()
		if entry != nil && entry.seq == seq {
			out = append(out, entry.value)
		}
	}

	return out
}
