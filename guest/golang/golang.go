// Package golang links the generic Go guest into plg. Import it for its side effect to run Go plugins:
//
//	import _ "github.com/heynemann/go-plg/guest/golang"
//
// The embedded golang.wasm is yaegi compiled to wasip1 from guestsrc/golang by `make guests`; CI rebuilds it
// and fails if the committed artifact differs.
package golang

import (
	_ "embed" // golang.wasm

	"github.com/heynemann/go-plg/internal/guestreg"
)

// Language is the plugin language this guest runs.
const Language = "go"

//go:embed golang.wasm
var artifact []byte

func init() {
	guestreg.Register(guestreg.Guest{Language: Language, Wasm: wasm})
}

// wasm returns the embedded module. Callers must not modify it.
func wasm() ([]byte, error) {
	return artifact, nil
}
