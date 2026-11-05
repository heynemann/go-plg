package plg_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/fastcontract"
	"github.com/heynemann/go-plg/internal/shadowcontract"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/wire"
	"github.com/heynemann/go-plg/plgimpl"
)

// fastContracts are the interfaces whose generated fast paths the parity tests hold to the generic binder.
func fastContracts() []reflect.Type {
	return []reflect.Type{
		reflect.TypeFor[testcontract.Fooer](), reflect.TypeFor[testcontract.Shaper](),
		reflect.TypeFor[testcontract.Hosted](), reflect.TypeFor[fastcontract.Kinds](),
		reflect.TypeFor[shadowcontract.Tables](),
	}
}

// answeringInvoker stands in for plg behind a generated adapter: it encodes the call's arguments and decodes
// the plugin's answer body into the call, as the binder does.
type answeringInvoker struct {
	argsErr error
	body    []byte
	args    []byte
	method  int
}

// Invoke implements plgimpl.Invoker. Like plg's, it returns the call's errors unchanged.
//
//nolint:wrapcheck // the errors are the ones under test
func (inv *answeringInvoker) Invoke(_ context.Context, method int, call plgimpl.Call) error {
	inv.method = method
	inv.args, inv.argsErr = call.AppendArgs(nil)

	if inv.argsErr != nil {
		return inv.argsErr
	}

	return plg.DecodeAnswer(inv.body, call)
}

// checkParity calls iface's method index with args (ctx first) through the adapter plg gen generated and through
// the generic binder, the plugin answering body, and requires the same argument encoding, the same errors
// and the same results.
func checkParity(t *testing.T, iface reflect.Type, index int, args []reflect.Value, body []byte) {
	t.Helper()

	method := iface.Method(index)
	inv := &answeringInvoker{body: body}
	adapter := plg.FastAdapter(iface, inv)
	require.NotNil(t, adapter, "%s has fast paths", iface)

	fn := reflect.ValueOf(adapter).MethodByName(method.Name)

	var out []reflect.Value

	if method.Type.IsVariadic() {
		out = fn.CallSlice(args)
	} else {
		out = fn.Call(args)
	}

	gotErr, _ := reflect.TypeAssert[error](out[len(out)-1])
	out = out[:len(out)-1]

	generic, err := plg.GenericCall(iface, index, args)
	require.NoError(t, err)

	wantArgs, wantArgsErr := generic.AppendArgs(nil)

	assert.Equal(t, index, inv.method, "the adapter calls its own method")
	requireSameError(t, wantArgsErr, inv.argsErr, "encoding the arguments")

	if wantArgsErr == nil {
		requireSameEncoding(t, method.Type, wantArgs, inv.args)

		wantErr := plg.DecodeAnswer(body, generic)
		requireSameError(t, wantErr, gotErr, "decoding the results")

		if wantErr == nil {
			for k, want := range plg.GenericResults(generic)[:len(out)] {
				assert.True(t, sameValue(want, out[k]), "result %d: generic %#v, generated %#v", k, want, out[k])
			}

			return
		}
	}

	for k, got := range out {
		assert.True(t, got.IsZero(), "result %d of a failed call is zero: %#v", k, got)
	}
}

func requireSameError(t *testing.T, want, got error, what string) {
	t.Helper()

	if want == nil {
		require.NoError(t, got, what)

		return
	}

	require.Error(t, got, what)
	require.Equal(t, want.Error(), got.Error(), what)

	for _, sentinel := range []error{
		wire.ErrTruncated, wire.ErrTrailing, wire.ErrInvalid, wire.ErrOverflow, wire.ErrPrecision, wire.ErrTooLong,
		wire.ErrTooDeep, wire.ErrTooLarge,
	} {
		assert.Equal(t, errors.Is(want, sentinel), errors.Is(got, sentinel), "%s: wraps %v", what, sentinel)
	}
}

// requireSameEncoding requires two encodings of fn's arguments to be equal: byte for byte, or, as maps
// encode their entries in iteration order, decoding to the same values.
func requireSameEncoding(t *testing.T, fn reflect.Type, want, got []byte) {
	t.Helper()

	require.Len(t, got, len(want), "argument encoding length")

	if bytes.Equal(want, got) {
		return
	}

	wantValues, gotValues := decodeArgs(t, fn, want), decodeArgs(t, fn, got)
	for k := range wantValues {
		assert.True(t, sameValue(wantValues[k], gotValues[k]), "parameter %d", k+1)
	}
}

// decodeArgs decodes the arguments of fn after the context with the reflective codec.
func decodeArgs(t *testing.T, fn reflect.Type, body []byte) []reflect.Value {
	t.Helper()

	reader := wire.NewReader(body)
	values := make([]reflect.Value, 0, fn.NumIn()-1)

	for k := 1; k < fn.NumIn(); k++ {
		codec, err := wire.For(fn.In(k))
		require.NoError(t, err)

		value := reflect.New(fn.In(k)).Elem()
		require.NoError(t, codec.Decode(&reader, value))

		values = append(values, value)
	}

	require.NoError(t, reader.End())

	return values
}

// sameValue reports whether a and b hold the same data, floats compared by their bits (NaN included).
func sameValue(want, got reflect.Value) bool {
	if want.Type() != got.Type() {
		return false
	}

	switch want.Kind() { //nolint:exhaustive // the remaining data kinds compare with ==
	case reflect.Float32, reflect.Float64:
		return math.Float64bits(want.Float()) == math.Float64bits(got.Float())
	case reflect.Slice:
		return sameSlice(want, got)
	case reflect.Map:
		return sameMap(want, got)
	case reflect.Pointer:
		if want.IsNil() || got.IsNil() {
			return want.IsNil() == got.IsNil()
		}

		return sameValue(want.Elem(), got.Elem())
	case reflect.Struct:
		return sameStruct(want, got)
	default:
		return want.Interface() == got.Interface()
	}
}

func sameSlice(want, got reflect.Value) bool {
	if want.IsNil() != got.IsNil() || want.Len() != got.Len() {
		return false
	}

	for i := range want.Len() {
		if !sameValue(want.Index(i), got.Index(i)) {
			return false
		}
	}

	return true
}

func sameMap(want, got reflect.Value) bool {
	if want.IsNil() != got.IsNil() || want.Len() != got.Len() {
		return false
	}

	for iter := want.MapRange(); iter.Next(); {
		other := got.MapIndex(iter.Key())
		if !other.IsValid() || !sameValue(iter.Value(), other) {
			return false
		}
	}

	return true
}

func sameStruct(want, got reflect.Value) bool {
	if want.Type() == reflect.TypeFor[time.Time]() {
		return reflect.DeepEqual(want.Interface(), got.Interface())
	}

	for i := range want.NumField() {
		if want.Type().Field(i).IsExported() && !sameValue(want.Field(i), got.Field(i)) {
			return false
		}
	}

	return true
}

// argsFrom decodes the arguments of fn after the context from data with the reflective codec, as far as data
// goes, using zero values for the rest; ctx comes first.
func argsFrom(ctx context.Context, fn reflect.Type, data []byte) []reflect.Value {
	args := []reflect.Value{reflect.ValueOf(ctx)}
	reader := wire.NewReader(data)
	failed := false

	for k := 1; k < fn.NumIn(); k++ {
		value := reflect.New(fn.In(k)).Elem()

		if !failed {
			codec, err := wire.For(fn.In(k))
			if err == nil {
				err = codec.Decode(&reader, value)
			}

			if err != nil {
				failed = true

				value.SetZero()
			}
		}

		args = append(args, value)
	}

	return args
}

// encodeValues encodes values with the reflective codec, one after the other.
func encodeValues(tb testing.TB, values ...any) []byte {
	tb.Helper()

	var body []byte

	for _, value := range values {
		codec, err := wire.For(reflect.TypeOf(value))
		require.NoError(tb, err)

		body, err = codec.Append(body, reflect.ValueOf(value))
		require.NoError(tb, err)
	}

	return body
}

func sampleEvery() fastcontract.Every {
	instant := time.Date(2026, 11, 3, 9, 30, 0, 123, time.UTC)
	inner := &fastcontract.Inner{A: 7, Tags: []string{}}

	every := fastcontract.Every{
		Labels: map[string]string{"k": "v"},
		Inner:  fastcontract.Inner{Hidden: "never crosses", A: -1, Tags: []string{"x", ""}},
		B:      true, I: -42, I8: -8, I16: 16, I32: -32, I64: wire.MaxSafeInt, U: 1, U8: 255, U16: 65535, U32: 1 << 31,
		U64: wire.MaxSafeInt, F32: 1.5, F64: math.Inf(-1), S: "ünïcode", Flag: true, Small: -128, Port: 8080,
		Code: "c", Temp: -40, Ratio: 0.25, Raw: []byte{}, Blob: fastcontract.Blob("blob"),
		Octets: fastcontract.Octets{1, 2, 3}, When: instant, At: &instant, List: fastcontract.List{nil, {}, {{}}},
		Index: fastcontract.Index{"a": {1, -2}, "nil": nil}, Ptr: inner,
		ByName: map[string]fastcontract.Inner{"k": {A: 1}}, ByPtr: map[string]*fastcontract.Inner{"p": inner, "n": nil},
		Grid: [][]int16{{1}, nil, {}}, Skip: "never crosses",
	}

	every.Anon.X = 9
	every.Anon.Y = []fastcontract.Ratio{0.5}
	every.Kids = []fastcontract.Every{{S: "kid", Next: &fastcontract.Every{S: "grandkid"}}}

	return every
}

// TestFastPathsNameShadowedPredeclaredTypes round-trips structs of the unicode package through the codecs plg
// gen generated in internal/shadowcontract, which redeclares int and uint16, and holds them to the generic
// binder: their int and uint16 fields still decode as the predeclared types.
func TestFastPathsNameShadowedPredeclaredTypes(t *testing.T) {
	t.Parallel()

	tables := reflect.TypeFor[shadowcontract.Tables]()
	table := *unicode.Latin

	for _, call := range []struct {
		arg  any
		name string
	}{
		{name: "Table", arg: table},
		{name: "Ranges", arg: table.R16},
	} {
		method, ok := tables.MethodByName(call.name)
		require.True(t, ok, call.name)

		args := []reflect.Value{reflect.ValueOf(t.Context()), reflect.ValueOf(call.arg)}
		checkParity(t, tables, method.Index, args, encodeValues(t, call.arg))

		results := reflect.ValueOf(plg.FastAdapter(tables, &answeringInvoker{body: encodeValues(t, call.arg)})).
			MethodByName(call.name).Call(args)
		require.Nil(t, results[1].Interface(), call.name)
		assert.Equal(t, call.arg, results[0].Interface(), "%s round-trips", call.name)
	}
}

// FuzzFastPathsMatchTheGenericBinder calls a method of a contract with arguments decoded from args through the
// generated adapter and the generic binder, the plugin answering body, and requires the same encoding,
// errors and results: plg gen's codecs encode and decode exactly as internal/wire's reflective codec.
func FuzzFastPathsMatchTheGenericBinder(f *testing.F) {
	order := testcontract.SampleOrder()
	every := sampleEvery()
	shaper, kinds := uint8(1), uint8(3)

	f.Add(shaper, uint8(1), encodeValues(f, order), encodeValues(f, order))               // Echo
	f.Add(shaper, uint8(1), encodeValues(f, order), encodeValues(f, order)[:40])          // truncated
	f.Add(shaper, uint8(1), encodeValues(f, order), append(encodeValues(f, order), 0))    // trailing
	f.Add(kinds, uint8(0), encodeValues(f, every), encodeValues(f, every))                // Every
	f.Add(kinds, uint8(0), encodeValues(f, every), encodeValues(f, fastcontract.Every{})) // zero
	f.Add(kinds, uint8(1), encodeValues(f, true, int8(-1), "c", []float32{1, 2}), encodeValues(f,
		uint16(1), 0.5, []byte{1}, map[string]int8{"a": 1, "b": 2})) // Many
	f.Add(kinds, uint8(1), []byte(nil), encodeValues(f, uint64(70000), 0.5, []byte(nil), map[string]int8(nil)))
	f.Add(kinds, uint8(1), []byte(nil), encodeValues(f, uint16(1), 0.5, []byte(nil), map[string]int64{"a": 300}))
	f.Add(kinds, uint8(1), []byte(nil), append(encodeValues(f, uint16(1), 0.5, []byte(nil)),
		duplicateKeys(f)...))
	f.Add(kinds, uint8(2), []byte(nil), []byte(nil))                             // Nothing
	f.Add(kinds, uint8(2), []byte(nil), []byte{1})                               // Nothing, trailing
	f.Add(kinds, uint8(4), encodeValues(f, &fastcontract.Inner{}), deepList(20)) // Point, garbage
	f.Add(kinds, uint8(3), deepList(5), deepList(wire.MaxDepth+1))               // Tree, too deep
	f.Add(kinds, uint8(0), []byte(nil), encodeValues(f, fastcontract.Every{F32: math.MaxFloat32})[:200])
	f.Add(uint8(0), uint8(4), encodeValues(f, true, 1, int8(2), int16(3), int32(4), int64(5), uint(6), uint8(7),
		uint16(8), uint32(9), uint64(10), float32(11), 12.0, "s"), encodeValues(f, true, 1, int64(300)))
	f.Add(kinds, uint8(5), encodeValues(f, []fastcontract.Heavy{{OK: true}}), encodeValues(f, // Weighty
		[]fastcontract.Heavy{{OK: true}, {}}, map[string]fastcontract.Heavy{"h": {OK: true}},
		[]*fastcontract.Heavy{nil, {OK: true}}))
	f.Add(kinds, uint8(5), []byte(nil), append(binary.LittleEndian.AppendUint32(nil, 1024), make([]byte, 1024)...))

	contracts := fastContracts()

	f.Fuzz(func(t *testing.T, which, method uint8, args, body []byte) {
		iface := contracts[int(which)%len(contracts)]
		index := int(method) % iface.NumMethod()

		checkParity(t, iface, index, argsFrom(t.Context(), iface.Method(index).Type, args), body)
	})
}

// duplicateKeys is the encoding of a map[string]Small with a key twice.
func duplicateKeys(tb testing.TB) []byte {
	tb.Helper()

	body := binary.LittleEndian.AppendUint32(nil, 2)

	for range 2 {
		var err error

		body, err = wire.AppendString(body, "k")
		require.NoError(tb, err)

		body = wire.AppendInt(body, 1)
	}

	return body
}

// deepList is the encoding of a List nested depth levels deep.
func deepList(depth int) []byte {
	var body []byte

	for range depth {
		body = binary.LittleEndian.AppendUint32(body, 1)
	}

	return binary.LittleEndian.AppendUint32(body, 0)
}

// TestFastPathsFailLikeTheGenericBinder holds the generated encoders to the reflective codec's failures: each
// names the parameter and the place in the value, and wraps the same error.
func TestFastPathsFailLikeTheGenericBinder(t *testing.T) {
	t.Parallel()

	const tooBig = wire.MaxSafeInt + 1

	kinds := reflect.TypeFor[fastcontract.Kinds]()

	cycle := sampleEvery()
	cycle.Next = &cycle

	deep := fastcontract.List{}
	for range wire.MaxDepth {
		deep = fastcontract.List{deep}
	}

	tests := []struct {
		name   string
		mutate func(e *fastcontract.Every)
		every  *fastcontract.Every
		want   string
	}{
		{name: "int", mutate: func(e *fastcontract.Every) { e.I64 = tooBig }, want: "I64: wire: integer beyond ±2^53"},
		{name: "uint", mutate: func(e *fastcontract.Every) { e.U = tooBig }, want: "U: wire"},
		{
			name: "nested in a slice of structs",
			mutate: func(e *fastcontract.Every) {
				e.Kids = []fastcontract.Every{
					{},
					{Next: &fastcontract.Every{Inner: fastcontract.Inner{A: -tooBig}}},
				}
			},
			want: "Kids[1].Next.Inner.A",
		},
		{
			name:   "map value",
			mutate: func(e *fastcontract.Every) { e.ByName = map[string]fastcontract.Inner{"k": {A: tooBig}} },
			want:   `ByName["k"].A`,
		},
		{
			name: "time",
			mutate: func(e *fastcontract.Every) {
				e.When = time.Unix(wire.MaxSafeInt+1, 0)
			},
			want: "When: wire: integer beyond ±2^53",
		},
		{name: "pointer cycle", every: &cycle, want: "nested too deeply"},
		{
			name:   "nesting",
			mutate: func(e *fastcontract.Every) { e.List = deep },
			want:   "nested too deeply",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			every := sampleEvery()
			if tc.every != nil {
				every = *tc.every
			} else {
				tc.mutate(&every)
			}

			args := []reflect.Value{reflect.ValueOf(t.Context()), reflect.ValueOf(every)}
			checkParity(t, kinds, 0, args, nil) // the arguments fail first

			generic, err := plg.GenericCall(kinds, 0, args)
			require.NoError(t, err)

			_, err = generic.AppendArgs(nil)
			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), "parameter 1: "), err.Error())
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestFastPathsDecodeIntoTheirOwnTypes covers results only the generated decoders build directly: named types,
// a slice of a named byte, a recursive slice, nil and empty kept apart.
func TestFastPathsDecodeIntoTheirOwnTypes(t *testing.T) {
	t.Parallel()

	inv := &answeringInvoker{body: encodeValues(t, uint16(443), 0.5, []byte{9, 8}, map[string]int64{"a": -128})}
	kinds, ok := plg.FastAdapter(reflect.TypeFor[fastcontract.Kinds](), inv).(fastcontract.Kinds)
	require.True(t, ok)

	port, ratio, octets, small, err := kinds.Many(t.Context(), true, 1, "c", 1.5, 2.5)
	require.NoError(t, err)
	assert.Equal(t, fastcontract.Port(443), port)
	assert.InDelta(t, 0.5, float64(ratio), 0, "exactly")
	assert.Equal(t, fastcontract.Octets{9, 8}, octets)
	assert.Equal(t, map[string]fastcontract.Small{"a": -128}, small)
	assert.Equal(t, encodeValues(t, true, int8(1), "c", []float32{1.5, 2.5}), inv.args, "the variadic tail is a slice")

	list := fastcontract.List{nil, {}, {{nil}}}
	inv.body = encodeValues(t, list)

	got, err := kinds.Tree(t.Context(), list)
	require.NoError(t, err)
	assert.Equal(t, list, got)
	assert.Nil(t, got[0])
	assert.NotNil(t, got[1])

	inv.body = nil

	require.NoError(t, kinds.Nothing(t.Context()))
}

// TestDecodersBoundMemoryByTheirInput feeds the generated and the generic decoders results whose length prefixes
// claim far more memory than their bytes justify: elements whose large field never crosses (in a slice, as map
// values with distinct keys, through pointers) and a recursive slice claiming the whole remaining input at
// every level. Both fail with the same wire.ErrTooLarge having allocated a bounded amount, where they used to
// allocate 64–100 MiB for 8–64 KiB.
//
//nolint:paralleltest // runtime.MemStats.TotalAlloc over one decode measures only it while no other test runs
func TestDecodersBoundMemoryByTheirInput(t *testing.T) {
	const count = 8192 // elements of 8 KiB claimed with one or two bytes each: 64 MiB

	null := binary.LittleEndian.AppendUint32(nil, math.MaxUint32) // a nil slice or map
	empty := binary.LittleEndian.AppendUint32(nil, 0)
	keyed := binary.LittleEndian.AppendUint32(nil, count)
	pointers := binary.LittleEndian.AppendUint32(nil, count)

	for i := range uint16(count) {
		keyed = append(binary.LittleEndian.AppendUint16(binary.LittleEndian.AppendUint32(keyed, 2), i), 0)
		pointers = append(pointers, 1, 0) // present, OK false
	}

	tests := []struct {
		name   string
		method string
		body   []byte
	}{
		{
			name: "slice", method: "Weighty",
			body: append(binary.LittleEndian.AppendUint32(nil, count), make([]byte, count)...),
		},
		{name: "map", method: "Weighty", body: slices.Concat(empty, keyed, null)},
		{name: "pointers", method: "Weighty", body: slices.Concat(empty, null, pointers)},
		{name: "nested claims", method: "Tree", body: greedyList(64<<10, 256)},
	}

	kinds := reflect.TypeFor[fastcontract.Kinds]()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			method, ok := kinds.MethodByName(tc.method)
			require.True(t, ok)

			args := argsFrom(t.Context(), method.Type, nil)

			generic, err := plg.GenericCall(kinds, method.Index, args)
			require.NoError(t, err)

			var wantErr, gotErr error

			allocated := allocatedBy(func() { wantErr = plg.DecodeAnswer(tc.body, generic) })
			t.Logf("generic: %d bytes for a %d byte body: %v", allocated, len(tc.body), wantErr)
			require.ErrorIs(t, wantErr, wire.ErrTooLarge)
			assert.Less(t, allocated, uint64(16<<20), "the generic decoder allocates a bounded amount")

			fn := reflect.ValueOf(plg.FastAdapter(kinds, &answeringInvoker{body: tc.body})).MethodByName(tc.method)
			allocated = allocatedBy(func() {
				out := fn.Call(args)
				gotErr, _ = reflect.TypeAssert[error](out[len(out)-1])
			})
			t.Logf("generated: %d bytes: %v", allocated, gotErr)
			requireSameError(t, wantErr, gotErr, "decoding the results")
			assert.Less(t, allocated, uint64(16<<20), "the generated decoder allocates a bounded amount")
		})
	}
}

// allocatedBy returns the bytes the program allocated while fn ran.
func allocatedBy(fn func()) uint64 {
	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

// greedyList is a List encoding of size bytes whose first levels each claim as many elements as the input
// left could hold, the first element of each being the next level; zeros (empty lists) fill the rest.
func greedyList(size, levels int) []byte {
	body := make([]byte, 0, size)

	for range levels {
		body = binary.LittleEndian.AppendUint32(body, uint32((size-len(body)-4)/4)) //nolint:gosec // G115: small
	}

	return append(body, make([]byte, size-len(body))...)
}

// TestDecodersDecodeLargeLegitimateResults holds both decoders to the same large results within the bound:
// values whose skipped fields take memory their encoding does not account for, within the allowance, and
// a wide recursive slice.
func TestDecodersDecodeLargeLegitimateResults(t *testing.T) {
	t.Parallel()

	kinds := reflect.TypeFor[fastcontract.Kinds]()
	decodes := func(name string, body []byte) {
		method, ok := kinds.MethodByName(name)
		require.True(t, ok)

		args := argsFrom(t.Context(), method.Type, nil)
		checkParity(t, kinds, method.Index, args, body)

		generic, err := plg.GenericCall(kinds, method.Index, args)
		require.NoError(t, err)
		require.NoError(t, plg.DecodeAnswer(body, generic), name)
	}

	items := make([]fastcontract.Heavy, 200)
	byKey := map[string]fastcontract.Heavy{}
	pointers := make([]*fastcontract.Heavy, 100)

	for i := range 50 {
		items[i].OK = true
		byKey[strconv.Itoa(i)] = fastcontract.Heavy{OK: i%2 == 0}
		pointers[2*i] = &fastcontract.Heavy{OK: true}
	}

	decodes("Weighty", encodeValues(t, items, byKey, pointers))

	wide := make(fastcontract.List, 1<<16)
	for i := range wide {
		wide[i] = fastcontract.List{}
	}

	decodes("Tree", encodeValues(t, wide))
}

// bufferInvoker answers every call with body, reusing one argument buffer as plg's pool slots do.
type bufferInvoker struct {
	buf  []byte
	body []byte
}

// Invoke implements plgimpl.Invoker. Like plg's, it returns the call's errors unchanged.
//
//nolint:wrapcheck // as plg's invoker, it adds nothing to the call's errors
func (inv *bufferInvoker) Invoke(_ context.Context, _ int, call plgimpl.Call) error {
	var err error

	inv.buf, err = call.AppendArgs(inv.buf[:0])
	if err != nil {
		return err
	}

	return plg.DecodeAnswer(inv.body, call)
}

// BenchmarkCallCodec is the host's codec share of one call, without a guest: encoding the arguments and
// decoding the results of Fooer.Foo (strings) and Shaper.Echo (SampleOrder both ways), by the code plg gen
// generated (through its adapter) and by the generic binder's reflective codec (without reflect.MakeFunc).
func BenchmarkCallCodec(b *testing.B) {
	order := testcontract.SampleOrder()

	for _, tc := range []struct {
		iface reflect.Type
		run   func(b *testing.B, adapter any)
		name  string
		args  []any
		body  []byte
		index int
	}{
		{
			name: "Foo", iface: reflect.TypeFor[testcontract.Fooer](), index: 3, args: []any{"bench"},
			body: encodeValues(b, "bench"),
			run: func(b *testing.B, adapter any) {
				b.Helper()

				fooer, ok := adapter.(testcontract.Fooer)
				require.True(b, ok)

				for b.Loop() {
					_, err := fooer.Foo(b.Context(), "bench")
					if err != nil {
						b.Fatal(err)
					}
				}
			},
		},
		{
			name: "Echo", iface: reflect.TypeFor[testcontract.Shaper](), index: 1, args: []any{order},
			body: encodeValues(b, order),
			run: func(b *testing.B, adapter any) {
				b.Helper()

				shaper, ok := adapter.(testcontract.Shaper)
				require.True(b, ok)

				for b.Loop() {
					_, err := shaper.Echo(b.Context(), order)
					if err != nil {
						b.Fatal(err)
					}
				}
			},
		},
	} {
		b.Run(tc.name+"/binder=generated", func(b *testing.B) {
			adapter := plg.FastAdapter(tc.iface, &bufferInvoker{body: tc.body})

			b.ReportAllocs()
			tc.run(b, adapter)
		})

		b.Run(tc.name+"/binder=generic", func(b *testing.B) {
			benchGenericCodec(b, tc.iface, tc.index, tc.args, tc.body)
		})
	}
}

// benchGenericCodec encodes args and decodes body as the generic binder's call of iface's method index does.
func benchGenericCodec(b *testing.B, iface reflect.Type, index int, values []any, body []byte) {
	b.Helper()

	args := make([]reflect.Value, 0, 1+len(values))
	args = append(args, reflect.ValueOf(b.Context()))

	for _, value := range values {
		args = append(args, reflect.ValueOf(value))
	}

	var buf []byte

	b.ReportAllocs()

	for b.Loop() {
		call, err := plg.GenericCall(iface, index, args)
		if err == nil {
			buf, err = call.AppendArgs(buf[:0])
		}

		if err == nil {
			err = plg.DecodeAnswer(body, call)
		}

		if err != nil {
			b.Fatal(err)
		}
	}
}
