package engine_test

import (
	"bytes"
	"encoding/binary"
	"slices"
)

// fakeGuest assembles a tiny wasm module that exports one page of memory and the plg ABI functions with the
// given bodies, so engine behavior can be tested against exact guest misbehavior without the real guest. It
// may import plg host functions: import i is function index i, called with call(i).
type fakeGuest struct {
	bodies  map[string][]byte // export name -> function body (instructions, without the final end)
	data    []byte            // placed at address 0
	imports []string          // plg host functions, in function index order
}

const (
	i32 = 0x7f
	i64 = 0x7e
)

// signatureOf returns the parameter and result types of an ABI export.
func signatureOf(export string) [2][]byte {
	switch export {
	case "plg_alloc":
		return [2][]byte{{i32}, {i32}}
	case "plg_init":
		return [2][]byte{{i32}, {i64}}
	case "plg_call":
		return [2][]byte{{i32, i32}, {i64}}
	case "plg_close":
		return [2][]byte{nil, {i64}}
	case "http_roundtrip":
		return [2][]byte{{i32, i32}, {i32}}
	case "take_result":
		return [2][]byte{{i32, i32}, nil}
	case "host_call":
		return [2][]byte{{i32, i32, i32}, {i32}}
	default: // _initialize
		return [2][]byte{}
	}
}

// Instruction helpers.
func i32Const(v int32) []byte { return append([]byte{0x41}, sleb(int64(v))...) }

func i64Const(v int64) []byte { return append([]byte{0x42}, sleb(v)...) }

func packed(ptr, size uint32) []byte { return i64Const(int64(ptr)<<32 | int64(size)) }

func unreachable() []byte { return []byte{0x00} }

// call calls function index fn.
func call(fn int) []byte { return append([]byte{0x10}, uleb(fn)...) }

// drop drops the top of the stack.
func drop() []byte { return []byte{0x1a} }

// spin is `loop br 0 end unreachable`.
func spin() []byte { return []byte{0x03, 0x40, 0x0c, 0x00, 0x0b, 0x00} }

// abiOK returns bodies for a well-behaved guest whose every call answers with the buffer at ptr/size.
func abiOK(ptr, size uint32) map[string][]byte {
	return map[string][]byte{
		"plg_alloc": i32Const(1024),
		"plg_init":  packed(ptr, size),
		"plg_call":  packed(ptr, size),
		"plg_close": packed(ptr, size),
	}
}

func (g fakeGuest) wasm() []byte {
	names := make([]string, 0, len(g.bodies))
	for name := range g.bodies {
		names = append(names, name)
	}

	slices.Sort(names)

	var types, imports, funcs, exports, code bytes.Buffer

	exports.Write(uleb(len(names) + 1))
	exports.Write(name("memory"))
	exports.Write([]byte{0x02, 0x00})

	for i, n := range slices.Concat(g.imports, names) {
		sig := signatureOf(n)
		params, results := sig[0], sig[1]

		types.WriteByte(0x60)
		types.Write(uleb(len(params)))
		types.Write(params)
		types.Write(uleb(len(results)))
		types.Write(results)

		if i < len(g.imports) {
			imports.Write(name("plg"))
			imports.Write(name(n))
			imports.WriteByte(0x00) // a function
			imports.Write(uleb(i))

			continue
		}

		funcs.Write(uleb(i))

		exports.Write(name(n))
		exports.WriteByte(0x00)
		exports.Write(uleb(i))

		body := append(append([]byte{0x00}, g.bodies[n]...), 0x0b) // no locals; end
		code.Write(uleb(len(body)))
		code.Write(body)
	}

	out := []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00}
	out = section(out, 1, vec(len(g.imports)+len(names), types.Bytes()))

	if len(g.imports) > 0 {
		out = section(out, 2, vec(len(g.imports), imports.Bytes()))
	}

	out = section(out, 3, vec(len(names), funcs.Bytes()))
	out = section(out, 5, []byte{0x01, 0x00, 0x01}) // one memory, min 1 page
	out = section(out, 7, exports.Bytes())
	out = section(out, 10, vec(len(names), code.Bytes()))

	if len(g.data) > 0 {
		// One active segment for memory 0 at i32.const 0.
		seg := slices.Concat([]byte{0x01, 0x00, 0x41, 0x00, 0x0b}, uleb(len(g.data)), g.data)
		out = section(out, 11, seg)
	}

	return out
}

func section(out []byte, id byte, payload []byte) []byte {
	out = append(out, id)
	out = append(out, uleb(len(payload))...)

	return append(out, payload...)
}

func vec(n int, items []byte) []byte {
	return append(uleb(n), items...)
}

func name(s string) []byte {
	return append(uleb(len(s)), s...)
}

func uleb(v int) []byte {
	return binary.AppendUvarint(nil, uint64(v)) //nolint:gosec // G115: sizes and indices are never negative
}

func sleb(v int64) []byte {
	var out []byte

	for {
		b := byte(v & 0x7f)
		v >>= 7

		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(out, b)
		}

		out = append(out, b|0x80)
	}
}
