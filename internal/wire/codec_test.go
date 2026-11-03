package wire_test

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/wire"
)

type (
	celsius float64
	id      string
	small   int8
	flag    bool
	count   uint16
)

var (
	errBoom  = errors.New("boom")
	errStale = errors.New("stale")
)

func codecFor(tb testing.TB, typ reflect.Type) *wire.Codec {
	tb.Helper()

	codec, err := wire.For(typ)
	require.NoError(tb, err)

	return codec
}

// roundTrip encodes v with the codec of its type and decodes it into a fresh value of the same type.
func roundTrip(t *testing.T, v any) any {
	t.Helper()

	typ := reflect.TypeOf(v)
	codec := codecFor(t, typ)
	assert.Equal(t, typ, codec.Type())

	buf, err := codec.Append(nil, reflect.ValueOf(v))
	require.NoError(t, err)

	reader := wire.NewReader(buf)
	out := reflect.New(typ).Elem()
	require.NoError(t, codec.Decode(&reader, out))
	require.NoError(t, reader.End())

	return out.Interface()
}

func TestScalarCodecsRoundTrip(t *testing.T) {
	t.Parallel()

	values := []any{
		true,
		false,
		0,
		math.MaxInt,
		math.MinInt,
		int8(math.MinInt8),
		int8(math.MaxInt8),
		int16(math.MinInt16),
		int32(math.MaxInt32),
		int64(math.MinInt64),
		uint(
			math.MaxUint,
		),
		uint8(math.MaxUint8),
		uint16(math.MaxUint16),
		uint32(math.MaxUint32),
		uint64(math.MaxUint64),
		float32(math.MaxFloat32),
		float32(-math.SmallestNonzeroFloat32),
		float32(math.Inf(1)),
		3.25,
		math.Inf(-1),
		"",
		"plg",
		"h\u00e9llo",
		celsius(-40),
		id("abc"),
		small(-3),
		flag(true),
		count(65535),
	}

	for _, v := range values {
		assert.Equal(t, v, roundTrip(t, v), "%T(%v)", v, v)
	}
}

// A guest interpreter reports a contract's named type by its underlying kind; both must share the encoding.
func TestNamedTypesShareTheirKindsEncoding(t *testing.T) {
	t.Parallel()

	named, err := codecFor(t, reflect.TypeFor[celsius]()).Append(nil, reflect.ValueOf(celsius(21.5)))
	require.NoError(t, err)

	reader := wire.NewReader(named)
	plain := reflect.New(reflect.TypeFor[float64]()).Elem()
	require.NoError(t, codecFor(t, plain.Type()).Decode(&reader, plain))
	assert.InDelta(t, 21.5, plain.Float(), 0)
}

func TestDecodeRejectsOutOfRangeNumbers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		into reflect.Type
		name string
		in   []byte
	}{
		{reflect.TypeFor[int8](), "int8 above range", wire.AppendInt(nil, 128)},
		{reflect.TypeFor[int16](), "int16 below range", wire.AppendInt(nil, math.MinInt16-1)},
		{reflect.TypeFor[int32](), "int32 above range", wire.AppendInt(nil, math.MaxInt32+1)},
		{reflect.TypeFor[uint8](), "uint8 above range", wire.AppendUint(nil, 256)},
		{reflect.TypeFor[uint32](), "uint32 above range", wire.AppendUint(nil, math.MaxUint32+1)},
		{reflect.TypeFor[float32](), "float32 above range", wire.AppendFloat(nil, math.MaxFloat64)},
		{reflect.TypeFor[small](), "named small type", wire.AppendInt(nil, 1000)},
	}

	for _, tc := range tests {
		reader := wire.NewReader(tc.in)
		val := reflect.New(tc.into).Elem()
		err := codecFor(t, tc.into).Decode(&reader, val)
		require.ErrorIs(t, err, wire.ErrOverflow, tc.name)
		require.ErrorContains(t, err, tc.into.String(), tc.name)
		assert.True(t, val.IsZero(), "%s: the destination is untouched", tc.name)
	}
}

func TestDecodePropagatesReaderErrors(t *testing.T) {
	t.Parallel()

	types := []reflect.Type{
		reflect.TypeFor[bool](), reflect.TypeFor[int](), reflect.TypeFor[uint](), reflect.TypeFor[float64](),
		reflect.TypeFor[string](), reflect.TypeFor[error](),
	}

	for _, typ := range types {
		reader := wire.NewReader(nil)
		err := codecFor(t, typ).Decode(&reader, reflect.New(typ).Elem())
		require.ErrorIs(t, err, wire.ErrTruncated, typ.String())
	}

	errType := reflect.TypeFor[error]()
	reader := wire.NewReader([]byte{1, 5, 0, 0, 0, 'x'})
	err := codecFor(t, errType).Decode(&reader, reflect.New(errType).Elem())
	require.ErrorIs(t, err, wire.ErrTruncated, "an error's message is truncated")

	boolType := reflect.TypeFor[bool]()
	reader = wire.NewReader([]byte{3})
	err = codecFor(t, boolType).Decode(&reader, reflect.New(boolType).Elem())
	require.ErrorIs(t, err, wire.ErrInvalid)
}

func TestErrorCodec(t *testing.T) {
	t.Parallel()

	codec := codecFor(t, reflect.TypeFor[error]())
	encode := func(err error) []byte {
		buf, encErr := codec.Append(nil, reflect.ValueOf(&err).Elem())
		require.NoError(t, encErr)

		return buf
	}

	assert.Equal(t, []byte{0}, encode(nil))
	assert.Equal(t, []byte{1, 4, 0, 0, 0, 'b', 'o', 'o', 'm'}, encode(errBoom))

	decode := func(buf []byte) error {
		reader := wire.NewReader(buf)

		var err error

		require.NoError(t, codec.Decode(&reader, reflect.ValueOf(&err).Elem()))
		require.NoError(t, reader.End())

		return err
	}

	require.NoError(t, decode(encode(nil)))

	got := decode(encode(errBoom))

	var remote *wire.RemoteError

	require.ErrorAs(t, got, &remote)
	assert.Equal(t, "boom", remote.Message)
	require.EqualError(t, got, "boom")

	reader := wire.NewReader(encode(nil))
	existing := errStale
	require.NoError(t, codec.Decode(&reader, reflect.ValueOf(&existing).Elem()))
	require.NoError(t, existing, "decoding nil clears the destination")
}

func TestUnsupportedTypes(t *testing.T) {
	t.Parallel()

	types := []reflect.Type{
		reflect.TypeFor[complex128](), reflect.TypeFor[uintptr](), reflect.TypeFor[chan int](),
		reflect.TypeFor[func()](), reflect.TypeFor[any](), reflect.TypeFor[[2]int](),
		// Structured types arrive with the full codec (ticket 03).
		reflect.TypeFor[[]byte](), reflect.TypeFor[[]string](), reflect.TypeFor[map[string]int](),
		reflect.TypeFor[struct{ A int }](), reflect.TypeFor[*int](), reflect.TypeFor[time.Time](),
	}

	for _, typ := range types {
		_, err := wire.For(typ)
		require.ErrorIs(t, err, wire.ErrUnsupported, typ.String())
		require.ErrorContains(t, err, typ.String())
	}
}

// signature is a scalar method signature, as the host and guest codecs see it.
func signature() []reflect.Type {
	return []reflect.Type{
		reflect.TypeFor[bool](), reflect.TypeFor[int](), reflect.TypeFor[int8](), reflect.TypeFor[uint16](),
		reflect.TypeFor[uint64](), reflect.TypeFor[float64](), reflect.TypeFor[string](), reflect.TypeFor[error](),
	}
}

// FuzzDecode feeds arbitrary bytes to the codecs of a scalar signature: decoding untrusted guest output must
// never panic, and whatever decodes cleanly re-encodes to the same bytes (the encoding is canonical).
func FuzzDecode(f *testing.F) {
	types := signature()

	codecs := make([]*wire.Codec, len(types))
	for i, typ := range types {
		codecs[i] = codecFor(f, typ)
	}

	seed := wire.AppendBool(nil, true)
	seed = wire.AppendInt(seed, -7)
	seed = wire.AppendInt(seed, 100)
	seed = wire.AppendUint(seed, 65535)
	seed = wire.AppendUint(seed, math.MaxUint64)
	seed = wire.AppendFloat(seed, math.Pi)
	seed = append(seed, 4, 0, 0, 0, 's', 'e', 'e', 'd', 1, 3, 0, 0, 0, 'e', 'r', 'r')

	f.Add(seed)
	f.Add(seed[:len(seed)-1])
	f.Add([]byte{})
	f.Add([]byte{2})

	f.Fuzz(func(t *testing.T, input []byte) {
		reader := wire.NewReader(input)
		values := make([]reflect.Value, len(codecs))

		for i, codec := range codecs {
			values[i] = reflect.New(types[i]).Elem()

			err := codec.Decode(&reader, values[i])
			if err != nil {
				return
			}
		}

		if reader.End() != nil {
			return
		}

		var out []byte

		for i, codec := range codecs {
			next, err := codec.Append(out, values[i])
			require.NoError(t, err)

			out = next
		}

		assert.Equal(t, input, out)
	})
}

func BenchmarkScalarCall(b *testing.B) {
	types := signature()
	codecs := make([]*wire.Codec, len(types))
	values := make([]reflect.Value, len(types))
	outs := make([]reflect.Value, len(types))

	for i, typ := range types {
		codecs[i] = codecFor(b, typ)
		values[i] = reflect.New(typ).Elem()
		outs[i] = reflect.New(typ).Elem()
	}

	values[6].SetString("a typical argument string")

	buf := make([]byte, 0, 256)

	b.ReportAllocs()

	for b.Loop() {
		buf = buf[:0]
		for i, codec := range codecs {
			next, err := codec.Append(buf, values[i])
			if err != nil {
				b.Fatal(err)
			}

			buf = next
		}

		reader := wire.NewReader(buf)
		for i, codec := range codecs {
			err := codec.Decode(&reader, outs[i])
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}
