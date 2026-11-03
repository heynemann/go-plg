// Package guestreg is the registry the guest packages (guest/golang, ...) add their artifact to from init, so
// the plg core links no guest it was not asked for and finds the linked ones by language.
package guestreg

import (
	"fmt"
	"slices"
	"strings"

	"github.com/puzpuzpuz/xsync/v4"
)

// Guest is a guest artifact: a linked one, or a custom one a Register brings (plg.WithGoGuest).
type Guest struct {
	// Wasm returns the guest module. It is called at most once per container runtime that needs the guest.
	Wasm func() ([]byte, error)
	// Language is the plugin language the guest runs ("go", "lua", "typescript").
	Language string
	// ID tells a custom guest apart from the linked guest of its language (empty) and from other custom
	// guests: its content digest, so that one module is compiled once per container.
	ID string
}

// guests holds every linked guest, keyed by language.
//
//nolint:gochecknoglobals // init-time registry filled by blank-imported guest packages
var guests = xsync.NewMap[string, Guest]()

// Register adds a guest. Guest packages call it from init; registering a language twice, or a guest without
// a language or artifact, is a programming error and panics.
func Register(g Guest) {
	if g.Language == "" || g.Wasm == nil {
		panic("guestreg: guest without a language or artifact")
	}

	if _, dup := guests.LoadOrStore(g.Language, g); dup {
		panic(fmt.Sprintf("guestreg: guest for language %q registered twice", g.Language))
	}
}

// Lookup returns the guest registered for language.
func Lookup(language string) (Guest, bool) {
	return guests.Load(language)
}

// All returns every registered guest, sorted by language.
func All() []Guest {
	all := make([]Guest, 0, guests.Size())
	for _, g := range guests.All() {
		all = append(all, g)
	}

	slices.SortFunc(all, func(a, b Guest) int { return strings.Compare(a.Language, b.Language) })

	return all
}
