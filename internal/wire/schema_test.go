package wire_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/wire"
)

type schemaNode struct {
	Attrs map[string]float32
	Name  string `json:"name"`
	At    time.Time
	Kids  []*schemaNode `json:"kids,omitempty"`
	Data  []byte
	Skip  int  `json:"-"`
	Small int8 `json:",omitempty"` //nolint:tagliatelle // the Go name fallback is the case under test
	U     uint16
	Flag  bool
}

// schemaBytes builds expected schema bytes.
type schemaBytes []byte

func (s schemaBytes) u32(n uint32) schemaBytes { return schemaBytes(appendLen(s, n)) }
func (s schemaBytes) str(v string) schemaBytes { return append(s.u32(uint32(len(v))), v...) } //nolint:gosec // short

func (s schemaBytes) kind(k wire.Kind, payload ...byte) schemaBytes {
	return append(append(s, byte(k)), payload...)
}

func (s schemaBytes) field(name string, index uint32) schemaBytes { return s.str(name).u32(index) }

func (s schemaBytes) indexes(ix ...uint32) schemaBytes {
	s = s.u32(uint32(len(ix))) //nolint:gosec // a handful
	for _, i := range ix {
		s = s.u32(i)
	}

	return s
}

// The schema is what the Lua and TypeScript glue read to name struct fields, so its layout is pinned: types in
// first-use order, shared and recursive types referred to by index, json tag names (else Go names), skipped
// fields absent.
func TestSchemaLayout(t *testing.T) {
	t.Parallel()

	var planner wire.Planner

	codec := func(typ reflect.Type) *wire.Codec {
		c, err := planner.For(typ)
		require.NoError(t, err)

		return c
	}

	var schema wire.Schema

	schema.AddMethod("Walk",
		[]*wire.Codec{codec(reflect.TypeFor[*schemaNode]()), codec(reflect.TypeFor[string]())},
		[]*wire.Codec{codec(reflect.TypeFor[[]int64]())})
	schema.AddMethod("Get",
		[]*wire.Codec{codec(reflect.TypeFor[schemaNode]())},
		[]*wire.Codec{codec(reflect.TypeFor[*schemaNode]()), codec(reflect.TypeFor[error]())})
	schema.AddMethod("Nothing", nil, nil)

	want := schemaBytes(nil).u32(14).
		kind(wire.KindPointer).u32(1). // 0: *schemaNode
		kind(wire.KindStruct).u32(8).  // 1: schemaNode
		field("Attrs", 2).field("name", 4).field("At", 5).field("kids", 6).field("Data", 7).
		field("Small", 8).field("U", 9).field("Flag", 10).
		kind(wire.KindMap).u32(3).    // 2: map[string]float32
		kind(wire.KindFloat, 32).     // 3
		kind(wire.KindString).        // 4
		kind(wire.KindTime).          // 5
		kind(wire.KindSlice).u32(0).  // 6: []*schemaNode
		kind(wire.KindBytes).         // 7
		kind(wire.KindInt, 8).        // 8
		kind(wire.KindUint, 16).      // 9
		kind(wire.KindBool).          // 10
		kind(wire.KindSlice).u32(12). // 11: []int64
		kind(wire.KindInt, 64).       // 12
		kind(wire.KindError).         // 13
		u32(3).
		str("Walk").indexes(0, 4).indexes(11).
		str("Get").indexes(1).indexes(0, 13).
		str("Nothing").indexes().indexes()

	assert.Equal(t, []byte(want), schema.Append(nil))
	assert.Equal(t, []byte{0, 0, 0, 0, 0, 0, 0, 0}, new(wire.Schema).Append(nil), "an empty schema")
}
