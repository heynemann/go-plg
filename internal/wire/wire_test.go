package wire_test

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/wire"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// The byte layout is the contract every guest implements, so it is pinned exactly.
func TestEncodingLayout(t *testing.T) {
	t.Parallel()

	str, err := wire.AppendString(nil, "ab")
	require.NoError(t, err)

	tests := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"false", wire.AppendBool(nil, false), []byte{0}},
		{"true", wire.AppendBool(nil, true), []byte{1}},
		{"int is 8 bytes little endian", wire.AppendInt(nil, 0x0102), []byte{2, 1, 0, 0, 0, 0, 0, 0}},
		{"negative int is two's complement", wire.AppendInt(nil, -1), bytes.Repeat([]byte{0xff}, 8)},
		{"uint", wire.AppendUint(nil, math.MaxUint64), bytes.Repeat([]byte{0xff}, 8)},
		{"float is binary64", wire.AppendFloat(nil, 1), []byte{0, 0, 0, 0, 0, 0, 0xf0, 0x3f}},
		{"string is u32 length then bytes", str, []byte{2, 0, 0, 0, 'a', 'b'}},
		{"append keeps the prefix", wire.AppendBool([]byte{9}, true), []byte{9, 1}},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.want, tc.got, tc.name)
	}
}

func TestPrimitivesRoundTrip(t *testing.T) {
	t.Parallel()

	ints := []int64{0, 1, -1, math.MaxInt64, math.MinInt64}
	uints := []uint64{0, 1, math.MaxUint64}
	floats := []float64{
		0, math.Copysign(0, -1), 1.5, -2.25, math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1),
	}
	strs := []string{"", "x", "h\u00e9llo, \u4e16\u754c", strings.Repeat("z", 1<<16), "\x00\xff"}

	buf := wire.AppendBool(wire.AppendBool(nil, true), false)
	for _, v := range ints {
		buf = wire.AppendInt(buf, v)
	}

	for _, v := range uints {
		buf = wire.AppendUint(buf, v)
	}

	for _, v := range floats {
		buf = wire.AppendFloat(buf, v)
	}

	buf = wire.AppendFloat(buf, math.NaN())

	for _, v := range strs {
		next, err := wire.AppendString(buf, v)
		require.NoError(t, err)

		buf = next
	}

	reader := wire.NewReader(buf)

	for _, want := range []bool{true, false} {
		got, err := reader.Bool()
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}

	for _, want := range ints {
		got, err := reader.Int64()
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}

	for _, want := range uints {
		got, err := reader.Uint64()
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}

	for _, want := range floats {
		got, err := reader.Float64()
		require.NoError(t, err)
		assert.Equal(t, math.Float64bits(want), math.Float64bits(got), "bit-exact, including signed zero")
	}

	nan, err := reader.Float64()
	require.NoError(t, err)
	assert.True(t, math.IsNaN(nan))

	for _, want := range strs {
		got, err := reader.Text()
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}

	assert.Zero(t, reader.Len())
	require.NoError(t, reader.End())
}

func TestTextDoesNotAliasTheBuffer(t *testing.T) {
	t.Parallel()

	buf, err := wire.AppendString(nil, "abc")
	require.NoError(t, err)

	reader := wire.NewReader(buf)
	s, err := reader.Text()
	require.NoError(t, err)

	buf[4] = 'X'

	assert.Equal(t, "abc", s, "the guest reuses its buffers; decoded strings must own their bytes")
}

func TestReaderErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want error
		name string
		kind string
		in   []byte
	}{
		{wire.ErrTruncated, "bool from nothing", "bool", nil},
		{wire.ErrInvalid, "bool other than 0 or 1", "bool", []byte{2}},
		{wire.ErrTruncated, "int short by a byte", "int", make([]byte, 7)},
		{wire.ErrTruncated, "uint from nothing", "uint", nil},
		{wire.ErrTruncated, "float short", "float", []byte{1}},
		{wire.ErrTruncated, "string without full length", "text", []byte{1, 0, 0}},
		{wire.ErrTruncated, "string longer than the input", "text", []byte{3, 0, 0, 0, 'a', 'b'}},
		{wire.ErrTruncated, "string length near 4 GiB", "text", []byte{0xff, 0xff, 0xff, 0xff, 'a'}},
		{wire.ErrTrailing, "trailing byte", "end", []byte{0}},
	}

	for _, tc := range tests {
		reader := wire.NewReader(tc.in)

		var err error

		switch tc.kind {
		case "bool":
			_, err = reader.Bool()
		case "int":
			_, err = reader.Int64()
		case "uint":
			_, err = reader.Uint64()
		case "float":
			_, err = reader.Float64()
		case "text":
			_, err = reader.Text()
		default:
			err = reader.End()
		}

		require.ErrorIs(t, err, tc.want, tc.name)
	}
}

func TestFailedReadsDoNotConsume(t *testing.T) {
	t.Parallel()

	reader := wire.NewReader([]byte{1, 2, 3})
	_, err := reader.Int64()
	require.ErrorIs(t, err, wire.ErrTruncated)
	assert.Equal(t, 3, reader.Len())
}

func TestAppendMessage(t *testing.T) {
	t.Parallel()

	decode := func(buf []byte) string {
		reader := wire.NewReader(buf)
		s, err := reader.Text()
		require.NoError(t, err)
		require.NoError(t, reader.End())

		return s
	}

	assert.Equal(t, "short", decode(wire.AppendMessage(nil, "short")))

	exact := strings.Repeat("a", wire.MaxMessage)
	assert.Equal(t, exact, decode(wire.AppendMessage(nil, exact)))

	long := decode(wire.AppendMessage(nil, strings.Repeat("a", wire.MaxMessage+10)))
	assert.Len(t, long, wire.MaxMessage)

	// A 3-byte rune straddling the limit is dropped whole.
	straddling := strings.Repeat("a", wire.MaxMessage-1) + "\u4e16"
	got := decode(wire.AppendMessage(nil, straddling))
	assert.Len(t, got, wire.MaxMessage-1)
	assert.True(t, utf8.ValidString(got))
}

func TestStatusString(t *testing.T) {
	t.Parallel()

	tests := map[wire.Status]string{
		wire.StatusOK: "ok", wire.StatusError: "error", wire.StatusPanic: "panic", wire.StatusLoad: "load",
		wire.StatusABI: "abi", 0: "invalid", 200: "invalid",
	}
	for s, want := range tests {
		assert.Equal(t, want, s.String())
	}
}
