// Package lua links the Lua guest into plg. Import it for its side effect to run Lua plugins:
//
//	import _ "github.com/heynemann/go-plg/guest/lua"
//
// The embedded lua.wasm is Lua 5.4 and the plg glue (guestsrc/lua) compiled to a wasip1 reactor with a pinned
// wasi-sdk by `make guests`; CI rebuilds it and fails if the committed artifact differs.
package lua

import (
	_ "embed" // lua.wasm

	"github.com/heynemann/go-plg/internal/guestreg"
)

// Language is the plugin language this guest runs.
const Language = "lua"

//go:embed lua.wasm
var artifact []byte

func init() {
	guestreg.Register(guestreg.Guest{Language: Language, Wasm: wasm})
}

// wasm returns the embedded module. Callers must not modify it.
func wasm() ([]byte, error) {
	return artifact, nil
}
