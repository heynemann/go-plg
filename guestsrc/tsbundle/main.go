//go:build wasip1

// Command tsbundle is the TypeScript bundler guest: esbuild (internal/tsbundle) compiled to a wasip1 reactor
// module exporting the plg guest ABI, so that the host bundles TypeScript plugins inside a sandbox it stops at its
// time and memory limits. The host mounts the plugin's files at tsbundle.PluginPath. All logic lives in
// internal/tsbundle; these exports only translate between wasm pointers and slices.
//
// Build it with `make guests`.
package main

import (
	"os"
	"unsafe"

	"github.com/heynemann/go-plg/internal/tsbundle"
)

// guest is the instance's single guest. wasm exports are free functions, so it has to be package state; the
// instance is single-threaded.
//
//nolint:gochecknoglobals,unused // see above; reached only through the //go:wasmexport functions
var guest = tsbundle.NewGuest(os.DirFS(tsbundle.PluginPath))

func main() {}

// plgAlloc returns a pointer to the guest-owned input buffer, grown to at least size bytes.
//
//go:wasmexport plg_alloc
func plgAlloc(size uint32) uint32 { //nolint:unused // called by the host through //go:wasmexport
	return ptr(guest.Alloc(size))
}

// plgInit bundles the plugin whose entrypoint is in the input buffer.
//
//go:wasmexport plg_init
func plgInit(size uint32) uint64 { //nolint:unused // called by the host through //go:wasmexport
	return pack(guest.Init(size))
}

// plgCall refuses every call: the bundler has no methods.
//
//go:wasmexport plg_call
func plgCall(_, _ uint32) uint64 { //nolint:unused // called by the host through //go:wasmexport
	return pack(guest.Call())
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
