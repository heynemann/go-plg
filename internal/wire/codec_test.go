package wire_test

import (
	"errors"
	"fmt"
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
	bigID   int64
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

func encodeValue(tb testing.TB, v any) []byte {
	tb.Helper()

	buf, err := codecFor(tb, reflect.TypeOf(v)).Append(nil, reflect.ValueOf(v))
	require.NoError(tb, err)

	return buf
}

// decodeInto decodes buf as one value of typ, requiring that it consumes buf.
func decodeInto(tb testing.TB, typ reflect.Type, buf []byte) (any, error) {
	tb.Helper()

	reader := wire.NewReader(buf)
	out := reflect.New(typ).Elem()

	err := codecFor(tb, typ).Decode(&reader, out)
	if err == nil {
		err = reader.End()
	}

	return out.Interface(), err
}

// roundTrip encodes v with the codec of its type and decodes it into a fresh value of the same type.
func roundTrip(t *testing.T, v any) any {
	t.Helper()

	typ := reflect.TypeOf(v)
	assert.Equal(t, typ, codecFor(t, typ).Type())

	out, err := decodeInto(t, typ, encodeValue(t, v))
	require.NoError(t, err)

	return out
}

func TestScalarCodecsRoundTrip(t *testing.T) {
	t.Parallel()

	values := []any{
		true,
		false,
		0,
		wire.MaxSafeInt,
		-wire.MaxSafeInt,
		int8(math.MinInt8),
		int8(math.MaxInt8),
		int16(math.MinInt16),
		int32(math.MaxInt32),
		int64(-wire.MaxSafeInt),
		uint(wire.MaxSafeInt),
		uint8(math.MaxUint8),
		uint16(math.MaxUint16),
		uint32(math.MaxUint32),
		uint64(wire.MaxSafeInt),
		float32(math.MaxFloat32),
		float32(-math.SmallestNonzeroFloat32),
		float32(math.Inf(1)),
		3.25,
		math.Inf(-1),
		math.MaxFloat64,
		"",
		"plg",
		"h\u00e9llo",
		celsius(-40),
		id("abc"),
		small(-3),
		flag(true),
		count(65535),
		bigID(-wire.MaxSafeInt),
	}

	for _, v := range values {
		assert.Equal(t, v, roundTrip(t, v), "%T(%v)", v, v)
	}
}

// A guest interpreter reports a contract's named type by its underlying kind; both must share the encoding.
func TestNamedTypesShareTheirKindsEncoding(t *testing.T) {
	t.Parallel()

	got, err := decodeInto(t, reflect.TypeFor[float64](), encodeValue(t, celsius(21.5)))
	require.NoError(t, err)
	assert.InDelta(t, 21.5, got, 0)
}

// Integers beyond ±2^53 cannot be represented exactly by Lua and JavaScript numbers: they fail on both sides
// of the wire, so no guest language ever sees a rounded value.
func TestIntegersBeyondTwoToThe53DoNotCross(t *testing.T) {
	t.Parallel()

	for _, v := range []any{
		wire.MaxSafeInt + 1, -wire.MaxSafeInt - 1, math.MaxInt, math.MinInt, int64(math.MinInt64),
		bigID(wire.MaxSafeInt + 1), uint(wire.MaxSafeInt + 1), uint64(math.MaxUint64),
	} {
		_, err := codecFor(t, reflect.TypeOf(v)).Append(nil, reflect.ValueOf(v))
		require.ErrorIs(t, err, wire.ErrPrecision, "%T(%v)", v, v)
		require.ErrorContains(t, err, fmt.Sprint(v), "the error names the value")
	}

	for _, tc := range []struct {
		into reflect.Type
		in   []byte
	}{
		{reflect.TypeFor[int](), wire.AppendInt(nil, wire.MaxSafeInt+1)},
		{reflect.TypeFor[int64](), wire.AppendInt(nil, math.MinInt64)},
		{reflect.TypeFor[uint](), wire.AppendUint(nil, wire.MaxSafeInt+1)},
		{reflect.TypeFor[uint64](), wire.AppendUint(nil, math.MaxUint64)},
	} {
		_, err := decodeInto(t, tc.into, tc.in)
		require.ErrorIs(t, err, wire.ErrPrecision, tc.into.String())
	}
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
		_, err := decodeInto(t, typ, nil)
		require.ErrorIs(t, err, wire.ErrTruncated, typ.String())
	}

	_, err := decodeInto(t, reflect.TypeFor[error](), []byte{1, 5, 0, 0, 0, 'x'})
	require.ErrorIs(t, err, wire.ErrTruncated, "an error's message is truncated")

	_, err = decodeInto(t, reflect.TypeFor[bool](), []byte{3})
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

type (
	unexported struct{ a int } //nolint:unused // the unexported field is the case under test
	clashing   struct {
		A int `json:"X"` //nolint:tagliatelle // the clash with X's Go name is the case under test
		X int
	}
	empty      struct{}
	hiddenOnly struct {
		H int `json:"-"`
	}
	withFunc struct {
		F    func()
		Name string
	}
	// selfAndFunc is recursive and unsupported: planning it fails after its slice plan referred back to it.
	selfAndFunc struct {
		F    func()
		Kids []selfAndFunc
	}
)

func TestUnsupportedTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		typ  reflect.Type
		want string
	}{
		{reflect.TypeFor[complex128](), "complex128"},
		{reflect.TypeFor[uintptr](), "uintptr"},
		{reflect.TypeFor[chan int](), "chan int"},
		{reflect.TypeFor[func()](), "func()"},
		{reflect.TypeFor[any](), "interface {}"},
		{reflect.TypeFor[[2]int](), "[2]int"},
		{reflect.TypeFor[[]any](), "interface {}"},
		{reflect.TypeFor[*chan int](), "chan int"},
		{reflect.TypeFor[map[int]string](), "map[int]string: map keys must be strings"},
		{reflect.TypeFor[map[string]func()](), "func()"},
		{reflect.TypeFor[unexported](), "unexported field a"},
		{reflect.TypeFor[clashing](), `fields A and X are both named "X"`},
		{reflect.TypeFor[[]empty](), "[]wire_test.empty: its elements encode as nothing"},
		{reflect.TypeFor[[]hiddenOnly](), "[]wire_test.hiddenOnly: its elements encode as nothing"},
		{reflect.TypeFor[withFunc](), "field wire_test.withFunc.F: wire: unsupported type: func()"},
		{reflect.TypeFor[selfAndFunc](), "field wire_test.selfAndFunc.F"},
	}

	for _, tc := range tests {
		_, err := wire.For(tc.typ)
		require.ErrorIs(t, err, wire.ErrUnsupported, tc.typ.String())
		require.ErrorContains(t, err, tc.want)
	}
}

// A Planner forgets everything a failed For planned: a later codec must not pick up a plan that refers to a
// type left half planned.
func TestPlannerForgetsFailedPlans(t *testing.T) {
	t.Parallel()

	var planner wire.Planner

	_, err := planner.For(reflect.TypeFor[selfAndFunc]())
	require.ErrorIs(t, err, wire.ErrUnsupported)

	_, err = planner.For(reflect.TypeFor[[]selfAndFunc]())
	require.ErrorIs(t, err, wire.ErrUnsupported, "its element is still unsupported, not half planned")

	codec, err := planner.For(reflect.TypeFor[[]string]())
	require.NoError(t, err, "the planner stays usable")
	assert.Equal(t, reflect.TypeFor[[]string](), codec.Type())
}

// signature is a scalar method signature, as the host and guest codecs see it.
func signature() []reflect.Type {
	return []reflect.Type{
		reflect.TypeFor[bool](), reflect.TypeFor[int](), reflect.TypeFor[int8](), reflect.TypeFor[uint16](),
		reflect.TypeFor[uint64](), reflect.TypeFor[float64](), reflect.TypeFor[string](), reflect.TypeFor[error](),
	}
}

// FuzzDecode feeds arbitrary bytes to the codecs of a scalar signature: decoding untrusted guest output must
// never panic, and whatever decodes cleanly re-encodes to the same bytes (the scalar encoding is canonical).
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
	seed = wire.AppendUint(seed, wire.MaxSafeInt)
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

// TestTimeUsesUnixSecondsAndNanoseconds pins time.Time's layout, which other guests implement.
func TestTimeUsesUnixSecondsAndNanoseconds(t *testing.T) {
	t.Parallel()

	assert.Equal(t, wire.AppendInt(wire.AppendInt(nil, -2), 999999999),
		encodeValue(t, time.Unix(-2, 999999999)), "before 1970: seconds round down, nanoseconds stay positive")
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
