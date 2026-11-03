package plgimpl_test

import (
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/wire"
	"github.com/heynemann/go-plg/plgimpl"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type (
	small  int8
	port   uint16
	temp   float32
	octet  uint8
	octets []octet
)

func reader(b []byte) *plgimpl.Reader {
	r := wire.NewReader(b)

	return &r
}

// readsLikeReflect decodes num into T with read and requires the outcome reflect's codec has: the value when
// it fits T, else an overflow error naming the type.
func readsLikeReflect[T any](t *testing.T, body []byte, read func(r *plgimpl.Reader) (T, error)) {
	t.Helper()

	codec, err := wire.For(reflect.TypeFor[T]())
	require.NoError(t, err)

	want := reflect.New(reflect.TypeFor[T]()).Elem()
	wantErr := codec.Decode(reader(body), want)

	got, err := read(reader(body))
	if wantErr != nil {
		require.EqualError(t, err, wantErr.Error())
		assert.Zero(t, got)

		return
	}

	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%#v", want.Interface()), fmt.Sprintf("%#v", got), "NaN included")
}

// TestReadsCheckRangesAsReflectDoes: each sized read accepts exactly the values reflect's Overflow methods
// accept, around every boundary, and fails with the same error.
func TestReadsCheckRangesAsReflectDoes(t *testing.T) {
	t.Parallel()

	ints := []int64{
		0, -1, 127, 128, -128, -129, 255, 256, 32767, 32768, -32769, math.MaxInt32, math.MinInt32 - 1,
		wire.MaxSafeInt, -wire.MaxSafeInt, wire.MaxSafeInt + 1, math.MinInt64,
	}
	for _, num := range ints {
		body := wire.AppendInt(nil, num)

		readsLikeReflect(
			t,
			body,
			func(r *plgimpl.Reader) (small, error) { return plgimpl.ReadInt[small](r, "plgimpl_test.small") },
		)
		readsLikeReflect(t, body, func(r *plgimpl.Reader) (int16, error) { return plgimpl.ReadInt[int16](r, "int16") })
		readsLikeReflect(t, body, func(r *plgimpl.Reader) (int32, error) { return plgimpl.ReadInt[int32](r, "int32") })
		readsLikeReflect(t, body, func(r *plgimpl.Reader) (int, error) { return plgimpl.ReadInt[int](r, "int") })
		readsLikeReflect(t, body, func(r *plgimpl.Reader) (uint8, error) { return plgimpl.ReadUint[uint8](r, "uint8") })
		readsLikeReflect(
			t,
			body,
			func(r *plgimpl.Reader) (port, error) { return plgimpl.ReadUint[port](r, "plgimpl_test.port") },
		)
		readsLikeReflect(
			t,
			body,
			func(r *plgimpl.Reader) (uint32, error) { return plgimpl.ReadUint[uint32](r, "uint32") },
		)
		readsLikeReflect(t, body, func(r *plgimpl.Reader) (uint, error) { return plgimpl.ReadUint[uint](r, "uint") })
	}

	floats := []float64{
		0,
		-1.5,
		math.MaxFloat32,
		-math.MaxFloat32,
		math.MaxFloat32 * 2,
		-math.MaxFloat64,
		math.Nextafter(
			math.MaxFloat32,
			math.Inf(1),
		),
		math.Inf(1),
		math.Inf(-1),
		math.NaN(),
		math.SmallestNonzeroFloat64,
	}
	for _, num := range floats {
		body := wire.AppendFloat(nil, num)

		readsLikeReflect(t, body, func(r *plgimpl.Reader) (temp, error) {
			return plgimpl.ReadFloat32[temp](r, "plgimpl_test.temp")
		})
	}

	for _, short := range [][]byte{nil, {1, 2, 3}} {
		_, err := plgimpl.ReadInt[int](reader(short), "int")
		require.ErrorIs(t, err, wire.ErrTruncated)

		_, err = plgimpl.ReadUint[uint](reader(short), "uint")
		require.ErrorIs(t, err, wire.ErrTruncated)

		_, err = plgimpl.ReadFloat32[float32](reader(short), "float32")
		require.ErrorIs(t, err, wire.ErrTruncated)
	}
}

// TestByteSlicesOfNamedBytes: a slice of a named byte type crosses exactly as []byte, nil and empty apart,
// and decodes into its own type without aliasing the input.
func TestByteSlicesOfNamedBytes(t *testing.T) {
	t.Parallel()

	for _, value := range []octets{nil, {}, {0, 1, 255}} {
		got, err := plgimpl.AppendByteSlice(nil, value)
		require.NoError(t, err)

		var plain []byte

		if value != nil {
			plain = make([]byte, len(value))
			for i, c := range value {
				plain[i] = byte(c)
			}
		}

		want, err := wire.AppendBytes(nil, plain)
		require.NoError(t, err)
		assert.Equal(t, want, got)

		decoded, err := plgimpl.ReadByteSlice[octets](reader(got))
		require.NoError(t, err)
		assert.Equal(t, value, decoded)
		assert.Equal(t, value == nil, decoded == nil, "nil stays nil, empty stays empty")

		if len(got) > 4 {
			got[4] = 42

			assert.Equal(t, octet(0), decoded[0], "the decoded slice does not alias the input")
		}
	}

	_, err := plgimpl.ReadByteSlice[octets](reader([]byte{3, 0, 0, 0, 1}))
	require.ErrorIs(t, err, wire.ErrTruncated)
}
