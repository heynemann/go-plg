package wire_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/traefik/yaegi/interp"

	"github.com/heynemann/go-plg/internal/wire"
)

type (
	// parent and child are mutually recursive: a child's minimum size is its parent's slice length prefix.
	parent struct{ Children []child }
	child  struct{ Parent parent }
)

// TestMutuallyRecursiveStructsRoundTrip: a struct reached again while it is planned (planning parent reaches
// child, which holds the unfinished parent) gets its size once the plans are complete, so a slice of it is
// not rejected as encoding as nothing, whichever of the two is planned first.
func TestMutuallyRecursiveStructsRoundTrip(t *testing.T) {
	t.Parallel()

	want := parent{Children: []child{{Parent: parent{Children: []child{{}}}}, {Parent: parent{Children: []child{}}}}}

	for _, first := range []reflect.Type{reflect.TypeFor[parent](), reflect.TypeFor[child]()} {
		var planner wire.Planner

		_, err := planner.For(first)
		require.NoError(t, err, first.String())

		codec, err := planner.For(reflect.TypeFor[parent]())
		require.NoError(t, err)

		b, err := codec.Append(nil, reflect.ValueOf(want))
		require.NoError(t, err)

		var got parent

		reader := wire.NewReader(b)
		require.NoError(t, codec.Decode(&reader, reflect.ValueOf(&got).Elem()))
		assert.Equal(t, want, got)

		// A count of children the remaining input cannot hold at 4 bytes each is truncated.
		reader = wire.NewReader(append([]byte{2, 0, 0, 0}, 0xff, 0xff, 0xff, 0xff))
		require.ErrorIs(t, codec.Decode(&reader, reflect.ValueOf(&got).Elem()), wire.ErrTruncated)
	}
}

// TestRejectsStructsTheInterpreterLaidOutTooSmall: yaegi builds a struct holding, by value, a struct still
// being declared around a placeholder of size 0, then swaps the field's real type in, leaving the struct
// smaller than its field. The Go guest plans the interpreted types; storing into such a struct would write
// beyond it, so the plan rejects it. Declared the other way round, the interpreted types are sound.
func TestRejectsStructsTheInterpreterLaidOutTooSmall(t *testing.T) {
	t.Parallel()

	const types = "package main\ntype Parent struct{ Children []Child }\ntype Child struct{ P Parent }\n"

	interpreted := func(t *testing.T, first string) reflect.Type {
		t.Helper()

		i := interp.New(interp.Options{})
		_, err := i.Eval(types + "var First = " + first + "{}\nvar P = Parent{}\n")
		require.NoError(t, err)

		v, err := i.Eval("main.P")
		require.NoError(t, err)

		return v.Type()
	}

	_, err := wire.For(interpreted(t, "Parent"))
	require.ErrorIs(t, err, wire.ErrUnsupported)
	require.ErrorContains(t, err, "field P does not fit in the struct as the Go interpreter laid it out")

	_, err = wire.For(interpreted(t, "Child"))
	require.NoError(t, err, "reached through Child first, the interpreted types are sound")
}
