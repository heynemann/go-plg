//go:build wasip1

// Package reactor is the generic Go guest's wasm side: the //go:wasmexport functions of the plg guest ABI and
// the host imports, for yaegi compiled to wasip1 as a reactor module. All logic lives in internal/goguest; these
// exports only translate between wasm pointers and slices.
//
// A guest's main package links it with a blank import: guestsrc/golang (the default guest, built by
// `make guests`) and the custom guests `plg build-guest` generates, which add symbol tables for extra packages.
package reactor

import (
	"net/http"
	"os"
	"runtime"
	"slices"
	"unsafe"

	"github.com/heynemann/go-plg/internal/goguest"
)

// guest is the instance's single guest. wasm exports are free functions, so it has to be package state; the
// instance is single-threaded.
//
//nolint:gochecknoglobals,unused // see above; reached only through the //go:wasmexport functions
var guest = goguest.New(os.Stdout, os.Stderr, callHost)

// init makes the standard net/http client go through the host: plugins call http.Get and friends as usual.
func init() { //nolint:gochecknoinits // a reactor has no other hook that runs before the plugin is loaded
	http.DefaultTransport = goguest.Transport{Exchange: exchange}
}

// httpRoundTrip hands the encoded request at ptr to the host, which makes the request and keeps its encoded
// answer for takeResult; it returns the answer's length.
//
//go:wasmimport plg http_roundtrip
func httpRoundTrip(ptr, size uint32) uint32

// takeResult copies the host's pending answer, exactly size bytes, to ptr.
//
//go:wasmimport plg take_result
func takeResult(ptr, size uint32)

// exchange sends an encoded request to the host and returns the host's answer in a fresh buffer.
func exchange(req []byte) []byte {
	size := httpRoundTrip(ptr(req), uint32(len(req))) //nolint:gosec // G115: goguest bounds requests below 2 GiB
	runtime.KeepAlive(req)

	resp := make([]byte, size)
	takeResult(ptr(resp), size)

	return resp
}

// hostCall hands the encoded arguments at ptr of a call to the Host method with the given index to the host,
// which calls it and keeps the status-prefixed answer for takeResult; it returns the answer's length.
//
//go:wasmimport plg host_call
func hostCall(method, ptr, size uint32) uint32 //nolint:unused // called by callHost, see there

// callHost calls a Host method and returns the host's answer, reusing dst's storage.
func callHost(method uint32, args, dst []byte) []byte { //nolint:unused // reached through guest, see there
	size := hostCall(method, ptr(args), uint32(len(args))) //nolint:gosec // G115: wasm32 lengths fit 32 bits
	runtime.KeepAlive(args)

	answer := slices.Grow(dst[:0], int(size))[:size]
	takeResult(ptr(answer), size)

	return answer
}

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
func ptr(b []byte) uint32 {
	return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(b)))) //nolint:gosec // G103,G115: wasm32 address, see above
}

// pack returns ptr<<32 | len, the ABI's result convention.
func pack(b []byte) uint64 { //nolint:unused // reached only through the //go:wasmexport functions
	return uint64(ptr(b))<<32 | uint64(uint32(len(b))) //nolint:gosec // G115: wasm32 lengths fit 32 bits
}
