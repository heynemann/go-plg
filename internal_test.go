package plg

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/wire"
)

// FuzzCheckData builds a type from the fuzz input and checks the validator against an oracle computed while
// building it: the type is data exactly when every part of it is.
func FuzzCheckData(f *testing.F) {
	for _, seed := range [][]byte{
		{},
		{5, 0},
		{6, 8, 2, 0, 3},
		{9, 8, 1, 4},
		{9, 1},
		{7, 0},
		{8, 2, 10, 0, 0},
		{5, 13, 0},
		{12},
		{11, 2},
		{8, 2, 0, 9, 8, 0},
		{6, 5, 14},
		{15},
		{16},
		{17},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, program []byte) {
		gen := typeGen{program: program}
		typ, isData := gen.next(0)

		err := checkData(typ, map[reflect.Type]bool{})
		if isData {
			assert.NoError(t, err, "%s", typ)
		} else {
			require.ErrorIs(t, err, errNotData, "%s", typ)
		}
	})
}

// typeGen builds a type from a byte program, one byte per decision.
type typeGen struct {
	program []byte
	pos     int
}

const maxGenDepth = 6

func (g *typeGen) op() byte {
	if g.pos >= len(g.program) {
		return 0
	}

	g.pos++

	return g.program[g.pos-1]
}

//nolint:gocyclo // one case per type constructor keeps the oracle readable
func (g *typeGen) next(depth int) (reflect.Type, bool) {
	op := g.op()
	if depth >= maxGenDepth {
		op = 0
	}

	switch op % 18 {
	case 0:
		return reflect.TypeFor[string](), true
	case 1:
		return reflect.TypeFor[int8](), true
	case 2:
		return reflect.TypeFor[float64](), true
	case 3:
		return reflect.TypeFor[time.Time](), true
	case 4:
		return reflect.TypeFor[uint64](), true
	case 5:
		elem, ok := g.next(depth + 1)

		return reflect.SliceOf(elem), ok
	case 6:
		elem, ok := g.next(depth + 1)

		return reflect.MapOf(reflect.TypeFor[string](), elem), ok
	case 7:
		elem, _ := g.next(depth + 1)

		return reflect.MapOf(reflect.TypeFor[int](), elem), false
	case 8:
		return g.structOf(depth)
	case 9:
		elem, ok := g.next(depth + 1)

		return reflect.PointerTo(elem), ok && elem.Kind() == reflect.Struct
	case 10:
		elem, _ := g.next(depth + 1)

		return reflect.FuncOf([]reflect.Type{elem}, nil, false), false
	case 11:
		elem, _ := g.next(depth + 1)

		return reflect.ChanOf(reflect.BothDir, elem), false
	case 12:
		return reflect.TypeFor[any](), false
	case 13:
		elem, _ := g.next(depth + 1)

		return reflect.StructOf([]reflect.StructField{{Name: "f", PkgPath: "p", Type: elem}}), false
	case 14:
		return reflect.TypeFor[complex64](), false
	case 15:
		elem, _ := g.next(depth + 1)

		return reflect.ArrayOf(2, elem), false
	case 16:
		return reflect.TypeFor[uintptr](), false
	default:
		return reflect.TypeFor[error](), false
	}
}

func (g *typeGen) structOf(depth int) (reflect.Type, bool) {
	n := int(g.op() % 3)
	fields := make([]reflect.StructField, n)
	isData := true

	for i := range fields {
		typ, ok := g.next(depth + 1)
		fields[i] = reflect.StructField{Name: "F" + string(rune('0'+i)), Type: typ}
		isData = isData && ok
	}

	return reflect.StructOf(fields), isData
}

type pair interface {
	M(ctx context.Context) (string, int8, error)
}

// TestDecodeResultsRejectsMalformedBodies covers the guest answering garbage with an OK status: the call
// fails (and its instance is discarded) instead of returning made-up values.
func TestDecodeResultsRejectsMalformedBodies(t *testing.T) {
	t.Parallel()

	meth, err := newMethod(reflect.TypeFor[pair](), 0, 0)
	require.NoError(t, err)

	valid, err := wire.AppendString(nil, "x")
	require.NoError(t, err)

	valid = wire.AppendInt(valid, -7)

	out, err := meth.decodeResults(valid)
	require.NoError(t, err)
	require.Len(t, out, 3)
	assert.Equal(t, "x", out[0].String())
	assert.Equal(t, int64(-7), out[1].Int())
	assert.True(t, out[2].IsNil(), "a nil error")

	for name, tc := range map[string]struct {
		want error
		body []byte
	}{
		"empty":     {wire.ErrTruncated, nil},
		"truncated": {wire.ErrTruncated, valid[:len(valid)-1]},
		"trailing":  {wire.ErrTrailing, append(slices.Clip(valid), 0)},
		"overflow":  {wire.ErrOverflow, wire.AppendInt(slices.Clip(valid[:len(valid)-8]), 300)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := meth.decodeResults(tc.body)
			require.ErrorIs(t, err, tc.want)
		})
	}
}
