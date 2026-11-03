package wire_test

import (
	"encoding/binary"
	"reflect"
	"runtime"
	"slices"
	"strconv"
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

	// heavy takes 8 KiB in memory but encodes in one byte: its block never crosses.
	heavy struct {
		Block [1024]int `json:"-"`
		OK    bool
	}

	// faulty takes 24 bytes in memory for two of encoding.
	faulty struct {
		Err error
		OK  bool
	}
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

// heavies is the encoding of n present pointers to heavy values.
func heavies(n int) []byte {
	body := appendLen(nil, uint32(n)) //nolint:gosec // G115: small
	for range n {
		body = append(body, 1, 1)
	}

	return body
}

// greedyNest is a nest encoding of size bytes whose first levels each claim as many elements as the input left
// could hold, the first element of each being the next level; zeros (empty nests) fill the rest.
func greedyNest(size, levels int) []byte {
	body := make([]byte, 0, size)
	for range levels {
		body = appendLen(body, uint32((size-len(body)-4)/4)) //nolint:gosec // G115: small
	}

	return append(body, make([]byte, size-len(body))...)
}

// allocated returns the bytes the program allocated while fn ran.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

// Decoding claims memory for slices, maps and pointer targets against a bound proportional to the input, so a
// short hostile encoding cannot make the decoder allocate far more than its size, whatever the element types
// take in memory beyond their encoding (fields tagged `json:"-"`) and however deep its length prefixes claim.
//
//nolint:paralleltest // runtime.MemStats.TotalAlloc over one decode measures only it while no other test runs
func TestDecodedMemoryIsBoundedByTheInput(t *testing.T) {
	const count = 8192 // heavy values: 64 MiB

	keyed := appendLen(nil, count)
	for i := range uint16(count) { // distinct two-byte keys
		keyed = append(binary.LittleEndian.AppendUint16(appendLen(keyed, 2), i), 1)
	}

	tests := []struct {
		into reflect.Type
		name string
		in   []byte
	}{
		{reflect.TypeFor[[]heavy](), "slice", append(appendLen(nil, count), make([]byte, count)...)},
		{reflect.TypeFor[map[string]heavy](), "map", keyed},
		{reflect.TypeFor[[]*heavy](), "pointers", heavies(count)},
		{reflect.TypeFor[nest](), "nested claims", greedyNest(64<<10, 256)},
		{reflect.TypeFor[forest](), "nested map claims", greedyForest(64<<10, 256)},
	}

	var err error

	for _, tc := range tests {
		bytes := allocated(func() { _, err = decodeInto(t, tc.into, tc.in) })
		t.Logf("%s: %d bytes for %d: %v", tc.name, bytes, len(tc.in), err)
		require.ErrorIs(t, err, wire.ErrTooLarge, tc.name)
		assert.Less(t, bytes, uint64(wire.AllocationRatio*len(tc.in)+wire.AllocationAllowance+1<<20), tc.name)
	}

	empties := appendLen(nil, count)
	for range count {
		empties = appendLen(empties, 0)
	}

	bytes := allocated(func() { _, err = decodeInto(t, reflect.TypeFor[[]map[string]heavy](), empties) })
	t.Logf("empty maps: %d bytes for %d: %v", bytes, len(empties), err)
	require.NoError(t, err)
	assert.Less(t, bytes, uint64(2<<20), "empty maps of heavy values make no scratch value")
}

// greedyForest is a forest encoding of size bytes whose first levels each claim as many entries as the input
// left could hold, the first entry of each (key "") being the next level; zeros fill the rest.
func greedyForest(size, levels int) []byte {
	body := make([]byte, 0, size)
	for range levels {
		body = appendLen(appendLen(body, uint32((size-len(body)-4)/8)), 0) //nolint:gosec // G115: small
	}

	return append(body, make([]byte, size-len(body))...)
}

// Legitimate values decode however large: every type the encoding supports stays within half the allocation
// ratio, and values whose skipped fields take memory their encoding does not account for decode within the
// allowance. The bound is the reader's, over all the values it decodes.
func TestLargeValuesWithinTheBoundDecode(t *testing.T) {
	t.Parallel()

	nilErrors := make([]error, 1<<20) // 16 MiB from 1 MiB: the worst ratio
	assert.Equal(t, nilErrors, roundTrip(t, nilErrors))

	failures := make([]faulty, 1<<19)
	assert.Equal(t, failures, roundTrip(t, failures))

	pointers := make([]*faulty, 1<<18)
	for i := range pointers {
		pointers[i] = &faulty{}
	}

	assert.Equal(t, pointers, roundTrip(t, pointers))

	byKey := make(map[string]faulty, 1<<16)
	for i := range 1 << 16 {
		byKey[strconv.Itoa(i)] = faulty{OK: true}
	}

	assert.Equal(t, byKey, roundTrip(t, byKey))

	fits := wire.AllocationAllowance / int(reflect.TypeFor[heavy]().Size())
	within := make([]heavy, fits)
	assert.Equal(t, within, roundTrip(t, within), "heavy values within the allowance")

	codec := codecFor(t, reflect.TypeFor[[]heavy]())
	body := encodeValue(t, within)
	reader := wire.NewReader(append(slices.Clone(body), body...))
	require.NoError(t, codec.Decode(&reader, reflect.New(codec.Type()).Elem()))
	require.ErrorIs(t, codec.Decode(&reader, reflect.New(codec.Type()).Elem()), wire.ErrTooLarge,
		"the second value finds the bound spent")
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
		reflect.TypeFor[nest](), reflect.TypeFor[holder](), reflect.TypeFor[[]float32](), reflect.TypeFor[[]heavy](),
		reflect.TypeFor[map[string]heavy](), reflect.TypeFor[[]*heavy](), reflect.TypeFor[[]map[string]heavy](),
	}

	codecs := make([]*wire.Codec, len(types))
	for i, typ := range types {
		codecs[i] = codecFor(f, typ)
	}

	seed := encodeValue(f, sampleOrder())
	for _, value := range []any{
		tree{Name: "t", Kids: []tree{{Name: "k"}}, Index: map[string]*tree{"x": nil}},
		chain(3),
		forest{"a": {"b": nil}},
		nest{{}, nil},
		holder{B: []byte{1}, M: map[string]int{"m": 1}},
		[]float32{1, 2},
		[]heavy{{OK: true}, {}},
		map[string]heavy{"h": {OK: true}},
		[]*heavy{nil, {OK: true}},
		[]map[string]heavy{nil, {}, {"h": {}}},
	} {
		seed = append(seed, encodeValue(f, value)...)
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
