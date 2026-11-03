// Package scriptabi is the init payload of the script guests (TypeScript and Lua): what their C glue needs to
// load a plugin and to convert values without Go types.
//
// Layout, where strings are as the wire encodes them (a little-endian uint32 byte length, then the bytes) and
// the count is a little-endian uint32:
//
//	payload = name source methodCount schema
//
// name is the plugin's file name as the guest reports it (TypeScript: the bundle's name; Lua: the entrypoint
// inside the /plugin mount) and source its code (TypeScript: the esbuild bundle; Lua: empty, the guest reads the
// mount). schema is a [wire.Schema] holding the interface's methods first, in method index order (the index a
// plg_call names), then the Host's, in Host method order (the index a host_call names is a schema method index
// minus methodCount); without a Host there are none.
package scriptabi

import "encoding/binary"

// Payload is a script guest's init payload.
type Payload struct {
	// Name and Source are far shorter than 4 GiB: a guest holds the source, and much more, in its wasm32 memory.
	Name   string
	Source string
	// Schema is the encoded [wire.Schema] of the interface's methods followed by the Host's.
	Schema []byte
	// Methods is the number of interface methods at the start of Schema.
	Methods uint32
}

// Append appends the encoded payload.
func (p *Payload) Append(dst []byte) []byte {
	dst = appendString(appendString(dst, p.Name), p.Source)
	dst = binary.LittleEndian.AppendUint32(dst, p.Methods)

	return append(dst, p.Schema...)
}

// appendString appends s as the wire encodes strings.
func appendString(dst []byte, s string) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(s))) //nolint:gosec // G115: see above

	return append(dst, s...)
}
