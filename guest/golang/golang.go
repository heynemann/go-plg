// Package golang links the generic Go guest into plg. Import it for its side effect to run Go plugins:
//
//	import _ "github.com/heynemann/go-plg/guest/golang"
//
// The embedded golang.wasm.gz is yaegi compiled to wasip1 from guestsrc/golang by `make guests` and compressed
// (the module is over 20 MB); CI rebuilds it and fails if the committed artifact differs.
package golang

import (
	"bytes"
	"compress/gzip"
	_ "embed" // golang.wasm.gz
	"fmt"
	"io"
	"sync"

	"github.com/heynemann/go-plg/internal/guestreg"
)

// Language is the plugin language this guest runs.
const Language = "go"

//go:embed golang.wasm.gz
var artifact []byte

// wasm returns the embedded module, inflated on first use and kept for the life of the process. Callers must
// not modify it.
//
//nolint:gochecknoglobals // the once-per-process inflation the guest registry hands out
var wasm = sync.OnceValues(func() ([]byte, error) { return inflate(artifact) })

func init() {
	guestreg.Register(guestreg.Guest{Language: Language, Wasm: wasm})
}

// inflate decompresses a gzip stream.
func inflate(compressed []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("guest/golang: inflating the artifact: %w", err)
	}

	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("guest/golang: inflating the artifact: %w", err)
	}

	return raw, nil
}
