// Package redact scrubs a plugin version's resolved secrets from what plg reports of the plugin: its output, the
// errors and panic values it produces and the log records about its requests. Each value is replaced by a mask
// naming its secret, [secret <name>], so a reader sees which secret was there without its value.
//
// Scrubbing guards against accidental leaks (a token printed while debugging, a URL in an error). It matches the
// values exactly: a plugin that encodes, splits or transforms a secret before writing it is not caught.
package redact

import (
	"bytes"
	"slices"
	"strings"
	"sync/atomic"
)

// MaxKept bounds the values kept per secret: a secret that rotates keeps its newest MaxKept values, so that a value
// an instance still holds after a rotation is scrubbed too, without the set growing with every rotation.
const MaxKept = 8

// Redactor holds the values of one plugin version's secrets. The zero value holds none; a nil *Redactor scrubs
// nothing. It is safe for concurrent use: [Redactor.Add] publishes a new immutable set, which scrubbing reads.
type Redactor struct {
	set atomic.Pointer[set]
}

// set is an immutable set of values and the replacer scrubbing them.
type set struct {
	replacer *strings.Replacer
	entries  []entry // in the order they were added
	longest  int     // the length of the longest value
}

// entry is one value of the secret name.
type entry struct {
	name, value string
}

// Mask returns what a value of the secret name is replaced with.
func Mask(name string) string {
	return "[secret " + name + "]"
}

// Add records value as the newest value of the secret name. An empty value is not recorded (it would match
// everywhere). A value the name had already becomes its newest again, so that resolving a value seen before keeps
// it; one recorded under another name stays recorded under both, so that either secret rotating away from it does
// not stop it being scrubbed. The name's oldest value goes once it has more than [MaxKept].
func (r *Redactor) Add(name, value string) {
	if value == "" {
		return
	}

	for {
		old := r.set.Load()
		if old.newest(name) == value {
			return
		}

		if r.set.CompareAndSwap(old, old.with(name, value)) {
			return
		}
	}
}

// newest returns name's newest value in s, "" when it has none.
func (s *set) newest(name string) string {
	if s == nil {
		return ""
	}

	for _, e := range slices.Backward(s.entries) {
		if e.name == name {
			return e.value
		}
	}

	return ""
}

// with returns a new set holding s's entries and value as name's newest value, without name's oldest value when it
// then has more than MaxKept.
func (s *set) with(name, value string) *set {
	added := entry{name: name, value: value}

	var entries []entry

	if s != nil {
		entries = slices.DeleteFunc(slices.Clone(s.entries), func(e entry) bool { return e == added })
	}

	entries = append(entries, added)

	if kept := countOf(entries, name); kept > MaxKept {
		oldest := slices.IndexFunc(entries, func(e entry) bool { return e.name == name })
		entries = slices.Delete(entries, oldest, oldest+1)
	}

	// The replacer tries its pairs in order at each position: longer values first, so that a value containing
	// another is replaced whole. A value two secrets hold is masked as the one that recorded it longest ago.
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(a, b entry) int { return len(b.value) - len(a.value) })

	pairs := make([]string, 0, 2*len(sorted))
	for _, e := range sorted {
		pairs = append(pairs, e.value, Mask(e.name))
	}

	return &set{replacer: strings.NewReplacer(pairs...), entries: entries, longest: len(sorted[0].value)}
}

// countOf returns how many entries are values of name.
func countOf(entries []entry, name string) int {
	n := 0

	for _, e := range entries {
		if e.name == name {
			n++
		}
	}

	return n
}

// String returns text with every value r holds replaced by its secret's mask. A text holding no value is returned
// as it is, without allocating: it runs on every line a guest writes and every request a plugin makes.
func (r *Redactor) String(text string) string {
	cur := r.current()
	if cur == nil {
		return text
	}

	// The replacer allocates as it scans for several values, even when none matches; Contains does not.
	for _, e := range cur.entries {
		if strings.Contains(text, e.value) {
			return cur.replacer.Replace(text)
		}
	}

	return text
}

// Span returns how many bytes a piece of text must hold, at least size, for [Redactor.Cut] and [Redactor.Trim] to
// protect every value r holds: twice the longest value when that is more than size.
func (r *Redactor) Span(size int) int {
	if cur := r.current(); cur != nil {
		return max(size, 2*cur.longest)
	}

	return size
}

// Cut returns where to cut text, the start of an output too long to report at once, so that what precedes the cut
// can be reported (scrubbed) now and the rest kept for later: it keeps the last bytes, which may be the start of a
// value the output goes on with, and never cuts a whole value text holds in two. It protects each value at most
// half as long as text (plus one); see [Redactor.Span]. It returns len(text) when there is nothing to keep.
func (r *Redactor) Cut(text []byte) int {
	cur := r.current()

	keep := cur.reach(len(text)) - 1
	if keep <= 0 {
		return len(text)
	}

	cut := len(text) - keep

	for _, e := range cur.entries {
		if len(e.value)-1 > keep {
			continue // too long for text: not protected
		}

		if s := straddling(text, e.value, cut); s >= 0 {
			cut = max(cut, s+len(e.value))
		}
	}

	return cut
}

// Trim returns how many bytes to drop from the start of text, the end of a longer one, so that it does not start
// with the end of a value whose start was cut away, and no whole value it holds is cut in two. Like [Redactor.Cut]
// it protects each value at most half as long as text (plus one). It returns 0 when there is nothing to drop.
func (r *Redactor) Trim(text []byte) int {
	cur := r.current()

	keep := cur.reach(len(text)) - 1
	if keep <= 0 {
		return 0
	}

	cut := keep

	for _, e := range cur.entries {
		if len(e.value)-1 > keep {
			continue // too long for text: not protected
		}

		if s := straddling(text, e.value, cut); s >= 0 {
			cut = min(cut, s)
		}
	}

	return cut
}

// reach returns the length of the longest value a text of size bytes can protect, one at most size/2+1 bytes long
// (0: none).
func (s *set) reach(size int) int {
	longest := 0

	if s != nil {
		for _, e := range s.entries {
			if len(e.value)-1 <= size/2 {
				longest = max(longest, len(e.value))
			}
		}
	}

	return longest
}

// current returns r's set, nil when it holds none.
func (r *Redactor) current() *set {
	if r == nil {
		return nil
	}

	return r.set.Load()
}

// straddling returns the start of an occurrence of value in b that a cut at position cut splits, or -1. Only an
// occurrence starting in (cut-len(value), cut) can, and the first one found there does.
func straddling(b []byte, value string, cut int) int {
	from := max(0, cut-len(value)+1)
	to := min(len(b), cut+len(value)-1)

	if to-from < len(value) {
		return -1
	}

	if i := bytes.Index(b[from:to], []byte(value)); i >= 0 && from+i < cut {
		return from + i
	}

	return -1
}
