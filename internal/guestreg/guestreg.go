// Package guestreg is the registry the guest packages (guest/golang, ...) add their artifact to from init, so
// the plg core links no guest it was not asked for and finds the linked ones by language.
package guestreg

import (
	"fmt"

	"github.com/puzpuzpuz/xsync/v4"
)

// Guest is a linked guest artifact.
type Guest struct {
	// Wasm returns the guest module. It is called at most once per container that needs the guest.
	Wasm func() ([]byte, error)
	// Language is the plugin language the guest runs ("go", "lua", "typescript").
	Language string
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
