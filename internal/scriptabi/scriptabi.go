// Package scriptabi is the init payload of the script guests (TypeScript and Lua): what their C glue needs to
// load a plugin and to convert values without Go types.
//
// Layout, where strings are as the wire encodes them (a little-endian uint32 byte length, then the bytes), the
// count is a little-endian uint32 and validate one byte, 0 or 1:
//
//	payload = name source methodCount validate schema
//
// name is the plugin's file name as the guest reports it (TypeScript: the bundle's name; Lua: the entrypoint
// inside the /plugin mount) and source its code (TypeScript: the esbuild bundle; Lua: empty, the guest reads the
// mount). schema is a [wire.Schema] holding the interface's methods first, in method index order (the index a
// plg_call names); then, when validate is 1, the plugin instance's Validate, which plg_call names by the index
// methodCount; then the Host's, in Host method order (the index a host_call names is a schema method index minus
// methodCount and validate); without a Host there are none.
package scriptabi

import "encoding/binary"

// Payload is a script guest's init payload.
type Payload struct {
	// Name and Source are far shorter than 4 GiB: a guest holds the source, and much more, in its wasm32 memory.
	Name   string
	Source string
	// Schema is the encoded [wire.Schema] of the interface's methods, then Validate when Validate is set, then the
	// Host's.
	Schema []byte
	// Methods is the number of interface methods at the start of Schema.
	Methods uint32
	// Validate tells the guest that the plugins validate: the schema method after the interface's is Validate.
	Validate bool
}

// Append appends the encoded payload.
func (p *Payload) Append(dst []byte) []byte {
	dst = appendString(appendString(dst, p.Name), p.Source)
	dst = binary.LittleEndian.AppendUint32(dst, p.Methods)

	validate := byte(0)
	if p.Validate {
		validate = 1
	}

	dst = append(dst, validate)

	return append(dst, p.Schema...)
}

// appendString appends s as the wire encodes strings.
func appendString(dst []byte, s string) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(s))) //nolint:gosec // G115: see above

	return append(dst, s...)
}
