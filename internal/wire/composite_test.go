package wire_test

import (
	"encoding/binary"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/wire"
)

type (
	Base struct {
		ID id `json:"id"`
	}

	point struct{ X, Y int }

	line struct {
		Note  string
		Qty   int     `json:"qty"`
		Price float64 `json:"price,omitempty"`
	}

	tags []string

	order struct {
		Base

		Secret string `json:"-"`
		When   time.Time
		Lines  []line
		ByName map[string]*line
		Raw    []byte
		Ref    *point
		Tags   tags
		Nested map[string][]map[string]int
		Matrix [][]int
	}

	// holder keeps nil and empty collections away from testify's []byte special case.
	holder struct {
		M map[string]int
		P *point
		B []byte
		S []int
	}

	tree struct {
		Index map[string]*tree
		Name  string
		Kids  []tree
	}

	list struct {
		Next *list
		V    int
	}

	forest map[string]forest
	nest   []nest
)

func sampleOrder() order {
	return order{
		Base:   Base{ID: "o-1"}, //nolint:modernize // embedlit: the embedded struct is part of the case
		When:   time.Date(2026, time.November, 3, 10, 30, 15, 123456789, time.UTC),
		Lines:  []line{{Qty: 1, Price: 2.5, Note: "first"}, {Qty: -3}},
		ByName: map[string]*line{"a": {Qty: 7}, "nil": nil},
		Raw:    []byte{0, 255, 7},
		Ref:    &point{X: -1, Y: 1},
		Tags:   tags{"x", ""},
		Matrix: [][]int{{1, 2}, nil, {}},
		Nested: map[string][]map[string]int{"k": {{"z": 1}, nil, {}}, "nil": nil},
	}
}

func TestCompositeCodecsRoundTrip(t *testing.T) {
	t.Parallel()

	values := []any{
		sampleOrder(),
		order{},
		order{Lines: []line{}, ByName: map[string]*line{}, Raw: []byte{}, Tags: tags{}, Matrix: [][]int{}},
		holder{},
		holder{B: []byte{}, S: []int{}, M: map[string]int{}, P: &point{}},
		holder{B: []byte("data"), S: []int{-1, 0, 1}, M: map[string]int{"": 0, "é": 9}},
		&point{X: 3},
		(*point)(nil),
		[]*point{nil, {X: 1}},
		map[string]*point{"nil": nil, "p": {Y: 2}},
		tree{
			Name: "root", Kids: []tree{{Name: "a", Kids: []tree{{Name: "a.a"}}}, {Name: "b"}},
			Index: map[string]*tree{"b": {Name: "b"}},
		},
		list{V: 1, Next: &list{V: 2, Next: &list{V: 3}}},
		forest{"a": {"b": nil, "c": {}}, "d": nil},
		nest{nil, {}, {nil}},
		[]time.Time{{}, time.Date(1969, time.December, 31, 23, 59, 59, 1, time.UTC)},
		[][]byte{nil, {}, {1}},
	}

	for _, v := range values {
		assert.Equal(t, v, roundTrip(t, v), "%T: %+v", v, v)
	}
}

// The byte layout is the contract the Lua and TypeScript glue implement, so it is pinned exactly.
func TestCompositeLayout(t *testing.T) {
	t.Parallel()

	nilLength := []byte{0xff, 0xff, 0xff, 0xff}
	u32 := func(n byte) []byte { return []byte{n, 0, 0, 0} }
	join := func(parts ...[]byte) []byte {
		var out []byte

		for _, p := range parts {
			out = append(out, p...)
		}

		return out
	}

	type skipping struct {
		A int
		B int `json:"-"`
		C bool
	}

	tests := []struct {
		value any
		name  string
		want  []byte
	}{
		{[]int(nil), "nil slice", nilLength},
		{[]int{}, "empty slice", u32(0)},
		{[]int{1}, "slice", join(u32(1), wire.AppendInt(nil, 1))},
		{[]byte(nil), "nil bytes", nilLength},
		{[]byte{}, "empty bytes", u32(0)},
		{[]byte("ab"), "bytes are raw, not words", join(u32(2), []byte("ab"))},
		{map[string]bool(nil), "nil map", nilLength},
		{map[string]bool{}, "empty map", u32(0)},
		{map[string]bool{"k": true}, "map entry", join(u32(1), u32(1), []byte("k"), []byte{1})},
		{(*point)(nil), "nil pointer", []byte{0}},
		{&point{X: 1, Y: 2}, "pointer", join([]byte{1}, wire.AppendInt(nil, 1), wire.AppendInt(nil, 2))},
		{
			skipping{A: 1, B: 2, C: true},
			"fields in order, json:\"-\" left out",
			join(wire.AppendInt(nil, 1), []byte{1}),
		},
		{time.Unix(5, 7), "time", join(wire.AppendInt(nil, 5), wire.AppendInt(nil, 7))},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.want, encodeValue(t, tc.value), tc.name)
	}
}

func TestSkippedFieldsDoNotCross(t *testing.T) {
	t.Parallel()

	plain := sampleOrder()
	plain.ByName, plain.Nested = nil, nil // map order would make the bytes differ
	withSecret := plain
	withSecret.Secret = "do not send"
	assert.Equal(t, encodeValue(t, plain), encodeValue(t, withSecret))

	codec := codecFor(t, reflect.TypeFor[order]())
	reader := wire.NewReader(encodeValue(t, withSecret))
	stale := order{Secret: "stale"}
	require.NoError(t, codec.Decode(&reader, reflect.ValueOf(&stale).Elem()))
	assert.Empty(t, stale.Secret, "decoding overwrites the whole value, skipped fields included")
}

func TestTimeCrossesAsAnInstantInUTC(t *testing.T) {
	t.Parallel()

	zoned := time.Date(2026, time.March, 29, 2, 30, 0, 999999999, time.FixedZone("CEST", 2*3600))
	now := time.Now()

	for _, in := range []time.Time{zoned, now, {}, time.Unix(-1, 1)} {
		got, ok := roundTrip(t, in).(time.Time)
		require.True(t, ok)
		assert.True(t, in.Equal(got), "%v: same instant, got %v", in, got)
		assert.Equal(t, time.UTC, got.Location(), "%v", in)
		assert.Equal(t, got, got.Round(0), "%v: no monotonic reading", in)
	}

	got, ok := roundTrip(t, time.Time{}).(time.Time)
	require.True(t, ok)
	assert.True(t, got.IsZero())
}

func appendLen(b []byte, n uint32) []byte {
	return binary.LittleEndian.AppendUint32(b, n)
}

func appendStr(b []byte, s string) []byte {
	b, _ = wire.AppendString(b, s) //nolint:errcheck // short test strings
	return b
}

// A conversion failure names where it happened, encoding (a host argument, a guest result) or decoding.
func TestConversionErrorsNameTheField(t *testing.T) {
	t.Parallel()

	tooBig := wire.MaxSafeInt + 1

	inLine := sampleOrder()
	inLine.Lines[1].Qty = tooBig

	inMap := sampleOrder()
	inMap.ByName["a"].Qty = -tooBig

	inNested := sampleOrder()
	inNested.Nested["k"][0]["z"] = tooBig

	inList := list{Next: &list{Next: &list{V: tooBig}}}

	tests := []struct {
		value any
		path  string
	}{
		{inLine, "Lines[1].Qty: "},
		{inMap, `ByName["a"].Qty: `},
		{inNested, `Nested["k"][0]["z"]: `},
		{inList, "Next.Next.V: "},
		{[]int{0, tooBig}, "[1]: "},
	}

	for _, tc := range tests {
		_, err := codecFor(t, reflect.TypeOf(tc.value)).Append(nil, reflect.ValueOf(tc.value))
		require.ErrorIs(t, err, wire.ErrPrecision, tc.path)
		assert.True(t, strings.HasPrefix(err.Error(), tc.path), "%q starts with %q", err, tc.path)
	}

	// Decoding: a line whose quantity is beyond 2^53, as a guest of another language could send it.
	body := appendLen(nil, 2)
	body = wire.AppendFloat(wire.AppendInt(appendStr(body, ""), 1), 0)
	body = wire.AppendFloat(wire.AppendInt(appendStr(body, ""), int64(tooBig)), 0)

	_, err := decodeInto(t, reflect.TypeFor[[]line](), body)
	require.ErrorIs(t, err, wire.ErrPrecision)
	assert.True(t, strings.HasPrefix(err.Error(), "[1].Qty: "), err.Error())

	entry := appendStr(appendLen(nil, 1), "key")
	entry = wire.AppendInt(entry, 300)
	_, err = decodeInto(t, reflect.TypeFor[map[string]small](), entry)
	require.ErrorIs(t, err, wire.ErrOverflow)
	assert.True(t, strings.HasPrefix(err.Error(), `["key"]: `), err.Error())
}

func TestDecodeRejectsMalformedComposites(t *testing.T) {
	t.Parallel()

	huge := uint32(0xfffffffe)

	tests := []struct {
		into reflect.Type
		want error
		name string
		in   []byte
	}{
		{reflect.TypeFor[[]int](), wire.ErrTruncated, "no length", []byte{1, 0}},
		{reflect.TypeFor[[]int](), wire.ErrTruncated, "more elements than input", wire.AppendInt(appendLen(nil, 2), 1)},
		{reflect.TypeFor[[]int](), wire.ErrTruncated, "a count near 4 Gi", appendLen(nil, huge)},
		{reflect.TypeFor[[]bool](), wire.ErrInvalid, "a bad element", append(appendLen(nil, 1), 2)},
		{reflect.TypeFor[[]byte](), wire.ErrTruncated, "bytes longer than input", append(appendLen(nil, 3), 1)},
		{reflect.TypeFor[[]byte](), wire.ErrTruncated, "bytes without length", []byte{1}},
		{reflect.TypeFor[map[string]int](), wire.ErrTruncated, "map without length", nil},
		{reflect.TypeFor[map[string]int](), wire.ErrTruncated, "more entries than input", appendLen(nil, huge)},
		{
			reflect.TypeFor[map[string]int](),
			wire.ErrTruncated,
			"truncated key",
			append(appendLen(nil, 1), 9, 0, 0, 0, 'k', 1, 2, 3, 4, 5, 6, 7),
		},
		{reflect.TypeFor[map[string]int](), wire.ErrTruncated, "truncated value", appendStr(appendLen(nil, 1), "k12")},
		{
			reflect.TypeFor[map[string]bool](), wire.ErrInvalid, "duplicate keys",
			append(appendStr(append(appendStr(appendLen(nil, 2), "k"), 1), "k"), 0),
		},
		{reflect.TypeFor[*point](), wire.ErrInvalid, "presence byte other than 0 or 1", []byte{2}},
		{reflect.TypeFor[*point](), wire.ErrTruncated, "no presence byte", nil},
		{reflect.TypeFor[*point](), wire.ErrTruncated, "truncated pointee", []byte{1, 0}},
		{reflect.TypeFor[point](), wire.ErrTruncated, "missing field", wire.AppendInt(nil, 1)},
		{reflect.TypeFor[time.Time](), wire.ErrTruncated, "time without nanoseconds", wire.AppendInt(nil, 1)},
		{reflect.TypeFor[time.Time](), wire.ErrTruncated, "time without seconds", nil},
		{
			reflect.TypeFor[time.Time](),
			wire.ErrInvalid,
			"a second of nanoseconds",
			wire.AppendInt(wire.AppendInt(nil, 0), 1e9),
		},
		{
			reflect.TypeFor[time.Time](),
			wire.ErrInvalid,
			"negative nanoseconds",
			wire.AppendInt(wire.AppendInt(nil, 0), -1),
		},
		{
			reflect.TypeFor[time.Time](), wire.ErrPrecision, "seconds beyond 2^53",
			wire.AppendInt(wire.AppendInt(nil, wire.MaxSafeInt+1), 0),
		},
	}

	for _, tc := range tests {
		_, err := decodeInto(t, tc.into, tc.in)
		require.ErrorIs(t, err, tc.want, tc.name)
	}
}

func chain(n int) *list {
	var head *list

	for i := range n {
		head = &list{V: i, Next: head}
	}

	return head
}

// Nesting is bounded: a pointer cycle fails instead of recursing forever, and a hostile encoding cannot nest
// deeper than MaxDepth.
func TestNestingIsBounded(t *testing.T) {
	t.Parallel()

	cycle := &list{V: 1}
	cycle.Next = cycle

	_, err := codecFor(t, reflect.TypeFor[*list]()).Append(nil, reflect.ValueOf(cycle))
	require.ErrorIs(t, err, wire.ErrTooDeep)

	// Each list node is a pointer and a struct: two levels.
	deep := chain(wire.MaxDepth/2 - 1)
	assert.Equal(t, deep, roundTrip(t, deep), "nesting up to MaxDepth crosses")

	_, err = codecFor(t, reflect.TypeFor[*list]()).Append(nil, reflect.ValueOf(chain(wire.MaxDepth/2+1)))
	require.ErrorIs(t, err, wire.ErrTooDeep)

	for _, tc := range []struct {
		into reflect.Type
		node []byte
		end  []byte
	}{
		{reflect.TypeFor[nest](), appendLen(nil, 1), appendLen(nil, 0)},
		{reflect.TypeFor[forest](), appendStr(appendLen(nil, 1), "k"), appendLen(nil, 0)},
		{reflect.TypeFor[*list](), []byte{1}, []byte{0}},
	} {
		hostile := make([]byte, 0, len(tc.node)*(wire.MaxDepth+1)+len(tc.end))
		for range wire.MaxDepth + 1 {
			hostile = append(hostile, tc.node...)
		}

		_, err = decodeInto(t, tc.into, append(hostile, tc.end...))
		require.ErrorIs(t, err, wire.ErrTooDeep, tc.into.String())
	}
}

// Decoded values never alias the input: the host decodes straight from guest memory, which the next call
// overwrites.
func TestDecodedValuesDoNotAliasTheInput(t *testing.T) {
	t.Parallel()

	value := holder{B: []byte("abc"), S: []int{1}, M: map[string]int{"k": 1}}
	buf := encodeValue(t, value)
	got, err := decodeInto(t, reflect.TypeFor[holder](), buf)
	require.NoError(t, err)

	for i := range buf {
		buf[i] = 0xee
	}

	assert.Equal(t, value, got)
}

// TestCodecsAreSafeForConcurrentUse shares one codec between goroutines, as the binder of a contract does.
func TestCodecsAreSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	codec := codecFor(t, reflect.TypeFor[order]())

	var wg sync.WaitGroup

	errs := make(chan error, 8)

	for g := range 8 {
		wg.Go(func() {
			for i := range 50 {
				value := sampleOrder()
				value.Lines[0].Qty = g*1000 + i

				buf, err := codec.Append(nil, reflect.ValueOf(value))
				if err == nil {
					reader := wire.NewReader(buf)
					out := reflect.New(codec.Type()).Elem()
					err = codec.Decode(&reader, out)

					if err == nil && !reflect.DeepEqual(value, out.Interface()) {
						err = assert.AnError
					}
				}

				if err != nil {
					errs <- err

					return
				}
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
}

// TestEncodingDoesNotAllocate shows that calls reuse the plans built once per type: encoding a struct without
// maps into a buffer with room, and decoding one without strings or collections, allocate nothing (`make
// allocs`; coverage instrumentation would make the values escape).
//
//nolint:paralleltest // testing.AllocsPerRun panics in parallel tests
func TestEncodingAndFixedSizeDecodingDoNotAllocate(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("allocation counts are only meaningful without coverage instrumentation")
	}

	in := sampleOrder()
	in.ByName, in.Nested = nil, nil
	codec := codecFor(t, reflect.TypeFor[order]())
	val := reflect.ValueOf(in)
	buf := make([]byte, 0, 1024)

	allocs := testing.AllocsPerRun(100, func() {
		_, err := codec.Append(buf, val)
		require.NoError(t, err)
	})
	assert.Zero(t, allocs, "encoding")

	type fixed struct {
		At   time.Time
		P    point
		F    float32
		Flag bool
	}

	fixedCodec := codecFor(t, reflect.TypeFor[fixed]())
	encoded := encodeValue(t, fixed{At: time.Unix(1, 2), P: point{3, 4}, F: 0.5, Flag: true})
	out := reflect.New(fixedCodec.Type()).Elem()

	var reader wire.Reader // escapes into the codec's function values once, not per call

	allocs = testing.AllocsPerRun(100, func() {
		reader = wire.NewReader(encoded)
		require.NoError(t, fixedCodec.Decode(&reader, out))
	})
	assert.Zero(t, allocs, "decoding")
}

// FuzzDecodeComposites decodes arbitrary bytes as every composite shape, as a hostile guest could send them:
// decoding must never panic or over-allocate, and whatever decodes re-encodes to the same length and decodes
// again (map order and float32 NaN payloads may differ, so bytes are not compared).
func FuzzDecodeComposites(f *testing.F) {
	types := []reflect.Type{
		reflect.TypeFor[order](), reflect.TypeFor[tree](), reflect.TypeFor[*list](), reflect.TypeFor[forest](),
		reflect.TypeFor[nest](), reflect.TypeFor[holder](), reflect.TypeFor[[]float32](),
	}

	codecs := make([]*wire.Codec, len(types))
	for i, typ := range types {
		codecs[i] = codecFor(f, typ)
	}

	seed := encodeValue(f, sampleOrder())
	for _, v := range []any{
		tree{Name: "t", Kids: []tree{{Name: "k"}}, Index: map[string]*tree{"x": nil}},
		chain(3),
		forest{"a": {"b": nil}},
		nest{{}, nil},
		holder{B: []byte{1}, M: map[string]int{"m": 1}},
		[]float32{1, 2},
	} {
		seed = append(seed, encodeValue(f, v)...)
	}

	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, input []byte) {
		reader := wire.NewReader(input)
		values := make([]reflect.Value, len(codecs))

		for i, codec := range codecs {
			values[i] = reflect.New(types[i]).Elem()

			if codec.Decode(&reader, values[i]) != nil {
				return
			}
		}

		if reader.End() != nil {
			return
		}

		var out []byte

		for i, codec := range codecs {
			next, err := codec.Append(out, values[i])
			require.NoError(t, err, "whatever decodes encodes")

			out = next
		}

		require.Len(t, out, len(input), "the encoding has a canonical length")

		again := wire.NewReader(out)
		for i, codec := range codecs {
			require.NoError(t, codec.Decode(&again, reflect.New(types[i]).Elem()))
		}
	})
}

func BenchmarkCodec(b *testing.B) {
	shapes := []struct {
		value any
		name  string
	}{
		{sampleOrder(), "order"},
		{[]line{{Qty: 1, Price: 2, Note: "a"}, {Qty: 2}, {Qty: 3, Note: "c"}}, "slice of structs"},
		{map[string]int{"a": 1, "b": 2, "c": 3}, "map"},
		{make([]byte, 4096), "bytes 4KiB"},
		{time.Now(), "time"},
		{chain(20), "linked list"},
	}

	for _, shape := range shapes {
		codec := codecFor(b, reflect.TypeOf(shape.value))
		val := reflect.ValueOf(shape.value)
		buf := encodeValue(b, shape.value)
		out := reflect.New(codec.Type()).Elem()

		b.Run(shape.name+"/encode", func(b *testing.B) {
			dst := make([]byte, 0, len(buf))

			b.ReportAllocs()

			for b.Loop() {
				_, err := codec.Append(dst, val)
				if err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(shape.name+"/decode", func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				reader := wire.NewReader(buf)

				err := codec.Decode(&reader, out)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCodecParallel shares one codec between goroutines, as concurrent calls of one method do.
func BenchmarkCodecParallel(b *testing.B) {
	codec := codecFor(b, reflect.TypeFor[order]())
	val := reflect.ValueOf(sampleOrder())

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		dst := make([]byte, 0, 1024)
		out := reflect.New(codec.Type()).Elem()

		for pb.Next() {
			buf, err := codec.Append(dst, val)
			if err == nil {
				reader := wire.NewReader(buf)
				err = codec.Decode(&reader, out)
			}

			if err != nil {
				b.Error(err)

				return
			}
		}
	})
}
