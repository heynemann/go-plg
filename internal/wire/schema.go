package wire

import "encoding/binary"

// Kind identifies a type in a [Schema].
type Kind byte

// Schema type kinds. Their values are part of the schema format.
const (
	KindBool    Kind = iota + 1
	KindInt          // a signed integer; the payload is its size in bits
	KindUint         // an unsigned integer; the payload is its size in bits
	KindFloat        // the payload is its size in bits, 32 or 64
	KindString       //
	KindBytes        // []byte
	KindTime         // time.Time
	KindError        // error
	KindSlice        // the payload is the element's type index
	KindMap          // string keys; the payload is the value's type index
	KindPointer      // the payload is the pointee's type index
	KindStruct       // the payload is the field count, then per encoded field its name and type index
)

// Schema describes, for guests without Go types (the Lua and TypeScript glue), how an interface's methods are
// encoded and what their struct fields are called: a field's `json` tag name, else its Go name. Fields tagged
// `json:"-"` are absent, as they are from the encoding. Add the methods in method index order, from codecs of
// one [Planner], then send the bytes of [Schema.Append]. The zero value is an empty schema.
//
// Layout, where counts and type indexes are little-endian uint32 and names are strings as the wire encodes
// them:
//
//	schema = typeCount type... methodCount method...
//	type   = kind(1 byte) payload
//	method = name paramCount typeIndex... resultCount typeIndex...
//
// Numeric kinds carry one byte, their size in bits; slices, maps and pointers the type index of their element;
// structs a field count then per field its name and type index; the other kinds nothing. A type index may refer
// to any type of the table, so recursive types stay finite. The parameters exclude the leading
// context.Context and the results the final error, as on the wire.
type Schema struct {
	index   map[*node]uint32
	types   []*node
	methods []schemaMethod
}

type schemaMethod struct {
	name            string
	params, results []uint32
}

// AddMethod adds a method with the given parameter and result codecs.
func (s *Schema) AddMethod(name string, params, results []*Codec) {
	method := schemaMethod{name: name, params: s.addAll(params), results: s.addAll(results)}
	s.methods = append(s.methods, method)
}

// Append appends the encoded schema.
func (s *Schema) Append(dst []byte) []byte {
	dst = appendCount(dst, len(s.types))

	for _, n := range s.types {
		dst = s.appendType(dst, n)
	}

	dst = appendCount(dst, len(s.methods))

	for i := range s.methods {
		m := &s.methods[i]
		dst = appendIndexes(appendIndexes(appendName(dst, m.name), m.params), m.results)
	}

	return dst
}

func (s *Schema) addAll(codecs []*Codec) []uint32 {
	indexes := make([]uint32, len(codecs))
	for i, c := range codecs {
		indexes[i] = s.add(c.root)
	}

	return indexes
}

// add returns the type index of n, adding it and the types it refers to.
func (s *Schema) add(n *node) uint32 {
	if i, ok := s.index[n]; ok {
		return i
	}

	if s.index == nil {
		s.index = map[*node]uint32{}
	}

	i := uint32(len(s.types)) //nolint:gosec // G115: a contract has far fewer than 2^32 types
	s.index[n] = i
	s.types = append(s.types, n)

	if n.elem != nil {
		s.add(n.elem)
	}

	for k := range n.fields {
		s.add(n.fields[k].node)
	}

	return i
}

func (s *Schema) appendType(dst []byte, n *node) []byte {
	dst = append(dst, byte(n.kind))

	switch n.kind { //nolint:exhaustive // the other kinds carry no payload, handled by the default branch
	case KindInt, KindUint, KindFloat:
		dst = append(dst, byte(n.typ.Bits())) //nolint:gosec // G115: at most 64 bits
	case KindSlice, KindMap, KindPointer:
		dst = binary.LittleEndian.AppendUint32(dst, s.index[n.elem])
	case KindStruct:
		dst = appendCount(dst, len(n.fields))

		for k := range n.fields {
			f := &n.fields[k]
			dst = binary.LittleEndian.AppendUint32(appendName(dst, f.name), s.index[f.node])
		}
	default:
	}

	return dst
}

// appendName appends a method or field name as a string. Names come from Go identifiers and struct tags, far
// shorter than the 4 GiB a length prefix can describe.
func appendName(dst []byte, name string) []byte {
	return append(appendCount(dst, len(name)), name...)
}

func appendCount(dst []byte, n int) []byte {
	return binary.LittleEndian.AppendUint32(dst, uint32(n)) //nolint:gosec // G115: name lengths and counts are small
}

func appendIndexes(dst []byte, indexes []uint32) []byte {
	dst = appendCount(dst, len(indexes))
	for _, i := range indexes {
		dst = binary.LittleEndian.AppendUint32(dst, i)
	}

	return dst
}
