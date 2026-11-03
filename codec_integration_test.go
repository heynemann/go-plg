//go:build integration

package plg_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// maxSafeInt is 2^53, the largest integer magnitude that crosses the plugin boundary.
const maxSafeInt = 1 << 53

// getShaper returns the shaper test plugin, registered as "shaper/ref" in a container of its own.
//
//nolint:ireturn // the plugin is served as the contract interface under test
func getShaper(tb testing.TB) testcontract.Shaper {
	tb.Helper()

	ctr := newContainer(tb)
	err := plg.Register[testcontract.Shaper](tb.Context(), ctr, "shaper",
		plg.WithStorage(goStorage(tb, "shaper", map[string]string{"ref": "shaper"})))
	require.NoError(tb, err)

	shaper, err := plg.Get[testcontract.Shaper](tb.Context(), ctr, "shaper/ref")
	require.NoError(tb, err)

	return shaper
}

func grownTree(name string, depth int) *testcontract.Tree {
	tree := &testcontract.Tree{Name: name}
	if depth > 0 {
		tree.Kids = []*testcontract.Tree{grownTree(name+".0", depth-1), grownTree(name+".1", depth-1)}
	}

	return tree
}

// TestEveryDataTypeCrossesAGoPlugin sends every allowed data type through a real Go plugin in the guest
// artifact and back, through the typed interface.
func TestEveryDataTypeCrossesAGoPlugin(t *testing.T) {
	t.Parallel()

	shaper := getShaper(t)
	sample := testcontract.SampleOrder()

	t.Run("structs, slices, maps, bytes and time, nested", func(t *testing.T) {
		t.Parallel()

		for _, in := range []testcontract.Order{sample, {}, testcontract.EmptyOrder()} {
			got, err := shaper.Echo(t.Context(), in)
			require.NoError(t, err)
			assert.Equal(t, in, got, "nil and empty collections stay distinct")
		}
	})

	t.Run("struct pointers", func(t *testing.T) {
		t.Parallel()

		got, err := shaper.EchoPtr(t.Context(), nil)
		require.NoError(t, err)
		assert.Nil(t, got)

		got, err = shaper.EchoPtr(t.Context(), &sample)
		require.NoError(t, err)
		assert.Equal(t, &sample, got)
		assert.NotSame(t, &sample, got, "a copy crossed, not the caller's memory")
	})

	t.Run("what the plugin sees", func(t *testing.T) {
		t.Parallel()

		in := sample
		in.Internal = "never sent"
		in.When = time.Date(2026, time.March, 1, 12, 0, 0, 42, time.FixedZone("CET", 3600))

		got, err := shaper.Inspect(t.Context(), in)
		require.NoError(t, err)
		assert.Equal(t, testcontract.Inspection{
			ID: "o-1", Zone: "UTC", Unix: in.When.Unix(), Nanos: 42,
			Lines: 2, ByCode: 2, Raw: 4, Tags: 2, Notes: 3, Grid: 3, Origin: true, NilEntry: true,
		}, got, "json:\"-\" fields stay behind; the instant arrives in UTC")

		got, err = shaper.Inspect(t.Context(), testcontract.Order{})
		require.NoError(t, err)
		assert.Equal(t, testcontract.Inspection{
			Zone: "UTC", Unix: time.Time{}.Unix(), Lines: -1, ByCode: -1, Raw: -1, Tags: -1, Notes: -1, Grid: -1,
		}, got, "nil collections arrive nil")
	})

	t.Run("values built by the plugin", func(t *testing.T) {
		t.Parallel()

		tree, err := shaper.Grow(t.Context(), 4, 2)
		require.NoError(t, err)
		assert.Equal(t, grownTree("root", 4), tree, "a recursive type")

		lines := []testcontract.Line{{SKU: "a", Qty: 1}, {SKU: "b", Qty: 2}, {SKU: "a", Qty: 3}}
		index, err := shaper.Index(t.Context(), lines)
		require.NoError(t, err)
		assert.Equal(t, map[string][]testcontract.Line{"a": {lines[0], lines[2]}, "b": {lines[1]}}, index)

		index, err = shaper.Index(t.Context(), nil)
		require.NoError(t, err)
		assert.Nil(t, index)

		index, err = shaper.Index(t.Context(), []testcontract.Line{})
		require.NoError(t, err)
		assert.Equal(t, map[string][]testcontract.Line{}, index)
	})

	t.Run("bytes", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct{ in, want []byte }{{nil, nil}, {[]byte{}, []byte{}}, {[]byte("abc"), []byte("cba")}} {
			got, err := shaper.Reverse(t.Context(), tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		}
	})

	t.Run("time", func(t *testing.T) {
		t.Parallel()

		at := time.Date(1969, time.December, 31, 23, 0, 0, 1, time.FixedZone("x", -7200))
		got, err := shaper.Later(t.Context(), at, 3600)
		require.NoError(t, err)
		assert.True(t, at.Add(time.Hour).Equal(got), "%v", got)
		assert.Equal(t, time.UTC, got.Location())
	})

	t.Run("variadic", func(t *testing.T) {
		t.Parallel()

		sum, err := shaper.Sum(t.Context(), 1)
		require.NoError(t, err)
		assert.Equal(t, 1, sum)

		sum, err = shaper.Sum(t.Context(), 1, 2, 3)
		require.NoError(t, err)
		assert.Equal(t, 6, sum)
	})
}

// TestOutOfRangeIntegersFailNamingTheField: integers beyond ±2^53 (which Lua and JavaScript numbers cannot
// represent exactly) fail with ErrConversion naming the parameter or result and the field, in either
// direction, and the instance keeps serving.
func TestOutOfRangeIntegersFailNamingTheField(t *testing.T) {
	t.Parallel()

	shaper := getShaper(t)

	inLine := testcontract.SampleOrder()
	inLine.Lines[1].Qty = maxSafeInt + 1

	inMap := testcontract.SampleOrder()
	inMap.ByCode["a"].Qty = -maxSafeInt - 1

	tests := []struct {
		err  error
		name string
		want string
	}{
		{
			name: "argument field", want: "shaper/ref.Echo: parameter 1: Lines[1].Qty: ",
			err: errOf(shaper.Echo(t.Context(), inLine)),
		},
		{
			name: "argument map value", want: `shaper/ref.EchoPtr: parameter 1: ByCode["a"].Qty: `,
			err: errOf(shaper.EchoPtr(t.Context(), &inMap)),
		},
		{
			name: "variadic argument", want: "shaper/ref.Sum: parameter 2: [1]: ",
			err: errOf(shaper.Sum(t.Context(), 0, 1, maxSafeInt+1)),
		},
		{
			name: "result field", want: "shaper/ref.Bump: result 0: Lines[0].Qty: ",
			err: errOf(shaper.Bump(t.Context(), testcontract.SampleOrder(), maxSafeInt-1)),
		},
	}

	for _, tc := range tests {
		require.ErrorIs(t, tc.err, plg.ErrConversion, tc.name)
		require.NotErrorIs(t, tc.err, plg.ErrCall, tc.name)
		assert.Contains(t, tc.err.Error(), tc.want, tc.name)
		assert.Contains(t, tc.err.Error(), "integer beyond ±2^53", tc.name)
	}

	// The instance survived: its state carries on across a result that could not cross.
	fooer := getFooer(t, fooerContainer(t, map[string]string{"ref": "fooer"}), "fooer/ref")

	first, err := fooer.Count(t.Context())
	require.NoError(t, err)

	//nolint:dogsled // Scalars returns fourteen values; only its error matters here
	_, _, _, _, _, _, _, _, _, _, _, _, _, _, err = fooer.Scalars(t.Context(), false, maxSafeInt, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, "")
	require.ErrorIs(t, err, plg.ErrConversion)
	assert.Contains(t, err.Error(), "fooer/ref.Scalars: result 1: ")

	second, err := fooer.Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first+1, second, "the same instance served both calls")
}

// errOf returns a call's error, dropping its result.
func errOf[T any](_ T, err error) error {
	return err
}

// BenchmarkCallGoStruct is one call into a Go plugin with a representative struct argument and result
// (SampleOrder: nested slices, maps, pointers, bytes and time), every protection on.
func BenchmarkCallGoStruct(b *testing.B) {
	shaper := getShaper(b)
	ctx := b.Context()
	order := testcontract.SampleOrder()

	b.ReportAllocs()

	for b.Loop() {
		_, err := shaper.Echo(ctx, order)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCallGoStructParallel contends for the plugin's single instance with struct calls.
func BenchmarkCallGoStructParallel(b *testing.B) {
	shaper := getShaper(b)
	ctx := b.Context()
	order := testcontract.SampleOrder()

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := shaper.Echo(ctx, order)
			if err != nil {
				b.Error(err)

				return
			}
		}
	})
}
