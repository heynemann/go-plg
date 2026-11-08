package testcase

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/layout"
	"github.com/heynemann/go-plg/internal/strictyaml"
)

// TestShapeOfAgreesWithTheWireKinds gives a type of each wire kind (layout's) the shape of that kind, so a new wire
// kind fails here instead of being decoded, written and compared as another shape, and panics on a type the wire
// does not carry.
func TestShapeOfAgreesWithTheWireKinds(t *testing.T) {
	t.Parallel()

	type octet uint8

	shapes := map[layout.Kind]shape{
		layout.Bool: shapeBool, layout.Int: shapeInteger, layout.Int8: shapeInteger, layout.Int16: shapeInteger,
		layout.Int32: shapeInteger, layout.Int64: shapeInteger, layout.Uint: shapeInteger,
		layout.Uint8: shapeInteger, layout.Uint16: shapeInteger, layout.Uint32: shapeInteger,
		layout.Uint64: shapeInteger, layout.Float32: shapeFloat, layout.Float64: shapeFloat,
		layout.String: shapeString, layout.Bytes: shapeBytes, layout.Time: shapeTime, layout.Slice: shapeSlice,
		layout.Map: shapeMap, layout.Pointer: shapePointer, layout.Struct: shapeStruct,
	}
	seen := map[layout.Kind]bool{}

	for _, typ := range []reflect.Type{
		reflect.TypeFor[bool](), reflect.TypeFor[int](), reflect.TypeFor[int8](), reflect.TypeFor[int16](),
		reflect.TypeFor[int32](), reflect.TypeFor[int64](), reflect.TypeFor[uint](), reflect.TypeFor[uint8](),
		reflect.TypeFor[uint16](), reflect.TypeFor[uint32](), reflect.TypeFor[uint64](), reflect.TypeFor[float32](),
		reflect.TypeFor[float64](), reflect.TypeFor[string](), reflect.TypeFor[[]byte](), reflect.TypeFor[[]octet](),
		reflect.TypeFor[time.Time](), reflect.TypeFor[[]string](), reflect.TypeFor[map[string]int](),
		reflect.TypeFor[*int](), reflect.TypeFor[struct{ A int }](),
	} {
		class, err := layout.Reflect{}.Classify(typ)
		require.NoError(t, err, typ)

		assert.Equal(t, shapes[class.Kind], shapeOf(typ), typ)

		seen[class.Kind] = true
	}

	for kind := layout.Kind(1); !strings.HasPrefix(kind.String(), "kind("); kind++ {
		assert.True(t, seen[kind], "no type of the wire kind %s", kind)
	}

	for _, typ := range []reflect.Type{
		reflect.TypeFor[chan int](), reflect.TypeFor[any](), reflect.TypeFor[[2]int](), reflect.TypeFor[uintptr](),
		reflect.TypeFor[complex128](), reflect.TypeFor[func()](),
	} {
		_, err := layout.Reflect{}.Classify(typ)
		require.ErrorIs(t, err, layout.ErrUnsupported, typ)

		assert.Panics(t, func() { shapeOf(typ) }, typ)
	}
}

// TestScalarsRoundTripEveryScalarShape: each scalar shape (those before shapeSlice) has its scalar, whose format
// writes a value that set reads back, from YAML, as one equal says is the same.
func TestScalarsRoundTripEveryScalarShape(t *testing.T) {
	t.Parallel()

	samples := map[shape][]any{
		shapeBool:    {true},
		shapeInteger: {int8(-128), uint64(1<<64 - 1)},
		shapeFloat:   {1.5, float32(0.1), 1e21},
		shapeString:  {"quote\"d\n"},
		shapeBytes:   {[]byte("hi")},
		shapeTime:    {time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("", 3600))},
	}

	require.Len(t, scalars, int(shapeSlice))

	for shp := shapeBool; shp < shapeSlice; shp++ {
		require.NotEmpty(t, samples[shp], "no sample of shape %d", shp)

		for _, sample := range samples[shp] {
			value := reflect.ValueOf(sample)
			require.Equal(t, shp, shapeOf(value.Type()), sample)

			written := scalars[shp].format(value)

			node, err := strictyaml.Parse([]byte(written), "sample")
			require.NoError(t, err, written)

			read := reflect.New(value.Type()).Elem()
			require.True(t, scalars[shp].set(node, read), written)
			assert.True(t, scalars[shp].equal(value, read), "%s read back as %s", written, scalars[shp].format(read))
		}
	}
}

// TestKindOfNamesAPointerByWhatItPointsTo: a pointer's value is written as its target's.
func TestKindOfNamesAPointerByWhatItPointsTo(t *testing.T) {
	t.Parallel()

	assert.Equal(t, kindOf(reflect.TypeFor[int8]()), kindOf(reflect.TypeFor[**int8]()))
	assert.Equal(t, kindOf(reflect.TypeFor[[]byte]()), kindOf(reflect.TypeFor[*[]byte]()))
}
