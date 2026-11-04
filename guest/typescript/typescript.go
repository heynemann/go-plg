// Package typescript links the TypeScript guest into plg. Import it for its side effect to run TypeScript (and
// JavaScript) plugins:
//
//	import _ "github.com/heynemann/go-plg/guest/typescript"
//
// It embeds two modules, built by `make guests`; CI rebuilds them and fails if the committed artifacts differ.
// typescript.wasm is QuickJS-ng with plg's glue (guestsrc/typescript), compiled to a wasip1 reactor with a pinned
// wasi-sdk: it runs the plugin's bundle with the ECMAScript built-ins, console, host and fetch only.
// bundler.wasm.gz is esbuild (guestsrc/tsbundle) compiled to wasip1 and compressed (the module is about 14 MB):
// the host runs it to bundle each plugin, inside a sandbox it stops at its time and memory limits.
package typescript

import (
	_ "embed" // typescript.wasm, bundler.wasm.gz
	"sync"

	"github.com/heynemann/go-plg/internal/guestreg"
)

// Language is the plugin language this guest runs.
const Language = "typescript"

//go:embed typescript.wasm
var artifact []byte

//go:embed bundler.wasm.gz
var bundlerArtifact []byte

func init() {
	guestreg.Register(guestreg.Guest{Language: Language, Wasm: wasm, Bundler: bundler})
}

// wasm returns the embedded module. Callers must not modify it.
func wasm() ([]byte, error) {
	return artifact, nil
}

// bundler returns the embedded bundler module, inflated on first use and kept for the life of the process.
// Callers must not modify it.
//
//nolint:gochecknoglobals // the once-per-process inflation the guest registry hands out
var bundler = sync.OnceValues(func() ([]byte, error) { return guestreg.Inflate(bundlerArtifact) })
