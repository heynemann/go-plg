// Package typescript links the TypeScript guest into plg. Import it for its side effect to run TypeScript (and
// JavaScript) plugins:
//
//	import _ "github.com/heynemann/go-plg/guest/typescript"
//
// The embedded typescript.wasm is QuickJS-ng with plg's glue (guestsrc/typescript), compiled to a wasip1 reactor
// by `make guests` with a pinned wasi-sdk; CI rebuilds it and fails if the committed artifact differs. Plugins
// are bundled on the host by esbuild; the guest runs the bundle with the ECMAScript built-ins, console, host and
// fetch only.
package typescript

import (
	_ "embed" // typescript.wasm

	"github.com/heynemann/go-plg/internal/guestreg"
)

// Language is the plugin language this guest runs.
const Language = "typescript"

//go:embed typescript.wasm
var artifact []byte

func init() {
	guestreg.Register(guestreg.Guest{Language: Language, Wasm: wasm})
}

// wasm returns the embedded module. Callers must not modify it.
func wasm() ([]byte, error) {
	return artifact, nil
}
