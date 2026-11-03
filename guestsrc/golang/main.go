//go:build wasip1

// Command golang is the generic Go guest: yaegi compiled to wasip1 as a reactor module exporting the plg guest
// ABI. All logic lives in internal/goguest; these exports only translate between wasm pointers and slices.
//
// Build it with `make guests`.
package main

import (
	"os"
	"unsafe"

	"github.com/heynemann/go-plg/internal/goguest"
)

// guest is the instance's single guest. wasm exports are free functions, so it has to be package state; the
// instance is single-threaded.
//
//nolint:gochecknoglobals,unused // see above; reached only through the //go:wasmexport functions
var guest = goguest.New(os.Stdout, os.Stderr)

func main() {}

// plgAlloc returns a pointer to the guest-owned input buffer, grown to at least size bytes.
//
//go:wasmexport plg_alloc
func plgAlloc(size uint32) uint32 { //nolint:unused // called by the host through //go:wasmexport
	return ptr(guest.Alloc(size))
}

// plgInit loads the plugin from the init payload in the input buffer.
//
//go:wasmexport plg_init
func plgInit(size uint32) uint64 { //nolint:unused // called by the host through //go:wasmexport
	return pack(guest.Init(size))
}

// plgCall calls a method with the arguments in the input buffer.
//
//go:wasmexport plg_call
func plgCall(method, size uint32) uint64 { //nolint:unused // called by the host through //go:wasmexport
	return pack(guest.Call(method, size))
}

// plgClose ends the instance.
//
//go:wasmexport plg_close
func plgClose() uint64 { //nolint:unused // called by the host through //go:wasmexport
	return pack(guest.Close())
}

// ptr returns the wasm32 address of b's first byte; uintptr is 32 bits on wasm, so the conversion is exact.
func ptr(b []byte) uint32 { //nolint:unused // reached only through the //go:wasmexport functions
	return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(b)))) //nolint:gosec // G103,G115: wasm32 address, see above
}

// pack returns ptr<<32 | len, the ABI's result convention.
func pack(b []byte) uint64 { //nolint:unused // reached only through the //go:wasmexport functions
	return uint64(ptr(b))<<32 | uint64(uint32(len(b))) //nolint:gosec // G115: wasm32 lengths fit 32 bits
}
