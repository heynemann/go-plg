package goguest_test

import (
	"bytes"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/goguest"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/wire"
)

func shaperType() reflect.Type { return reflect.TypeFor[testcontract.Shaper]() }

// loadShaper loads the shaper test plugin for the Shaper contract, whose interpreted types are structs,
// pointers, slices, maps, []byte and time.Time.
func loadShaper(t *testing.T) *goguest.Guest {
	t.Helper()

	dispatcher, err := goabi.RenderDispatcher(shaperType())
	require.NoError(t, err)

	names := make([]string, shaperType().NumMethod())
	for i := range names {
		names[i] = shaperType().Method(i).Name
	}

	var out bytes.Buffer

	guest := goguest.New(&out, &out)
	root := gopathWith(t, testdataPlugin(t, "shaper"), string(dispatcher))
	res := parse(t, guest.Init(send(guest, initPayload(t, root, names))))
	require.Equal(t, wire.StatusOK, res.status, res.msg)

	return guest
}

func callShaper(t *testing.T, guest *goguest.Guest, method string, args ...any) result {
	t.Helper()

	m, ok := shaperType().MethodByName(method)
	require.True(t, ok, method)

	return parse(t, guest.Call(uint32(m.Index), send(guest, encode(t, args...)))) //nolint:gosec // G115: few methods
}

// shaped calls method and decodes its single result into a value of want's type.
func shaped(t *testing.T, guest *goguest.Guest, want any, method string, args ...any) any {
	t.Helper()

	res := callShaper(t, guest, method, args...)
	require.Equal(t, wire.StatusOK, res.status, res.msg)

	return decode(t, res.body, want)[0]
}

func grown(name string, depth int) *testcontract.Tree {
	tree := &testcontract.Tree{Name: name}
	if depth > 0 {
		tree.Kids = []*testcontract.Tree{grown(name+".0", depth-1), grown(name+".1", depth-1)}
	}

	return tree
}

// TestDataTypesCrossTheGuest sends every data type through the interpreted plugin and back: the guest
// decodes into and encodes from the types yaegi creates for the contract.
//
//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestDataTypesCrossTheGuest(t *testing.T) {
	guest := loadShaper(t)
	sample := testcontract.SampleOrder()
	before1970 := time.Date(1969, time.December, 31, 23, 59, 59, 500, time.UTC)
	lines := []testcontract.Line{{SKU: "a", Qty: 1}, {SKU: "b", Qty: 2}, {SKU: "a", Qty: 3}}

	tests := []struct {
		want   any
		name   string
		method string
		args   []any
	}{
		{name: "nested struct", method: "Echo", args: []any{sample}, want: sample},
		{
			name:   "nil collections stay nil",
			method: "Echo",
			args:   []any{testcontract.Order{}},
			want:   testcontract.Order{},
		},
		{
			name: "empty collections stay empty", method: "Echo",
			args: []any{testcontract.EmptyOrder()}, want: testcontract.EmptyOrder(),
		},
		{
			name:   "nil pointer",
			method: "EchoPtr",
			args:   []any{(*testcontract.Order)(nil)},
			want:   (*testcontract.Order)(nil),
		},
		{name: "struct pointer", method: "EchoPtr", args: []any{&sample}, want: &sample},
		{name: "recursive type", method: "Grow", args: []any{3, 2}, want: grown("root", 3)},
		{
			name:   "nil map from nil slice",
			method: "Index",
			args:   []any{[]testcontract.Line(nil)},
			want:   map[string][]testcontract.Line(nil),
		},
		{
			name:   "empty map",
			method: "Index",
			args:   []any{[]testcontract.Line{}},
			want:   map[string][]testcontract.Line{},
		},
		{
			name: "map built by the plugin", method: "Index", args: []any{lines},
			want: map[string][]testcontract.Line{"a": {lines[0], lines[2]}, "b": {lines[1]}},
		},
		{name: "time before 1970", method: "Later", args: []any{before1970, 1}, want: before1970.Add(time.Second)},
		{name: "variadic without values", method: "Sum", args: []any{4, []int(nil)}, want: 4},
		{name: "variadic", method: "Sum", args: []any{4, []int{1, 2, 3}}, want: 10},
		{name: "nil bytes", method: "Reverse", args: []any{[]byte(nil)}, want: []byte(nil)},
		{name: "empty bytes", method: "Reverse", args: []any{[]byte{}}, want: []byte{}},
		{name: "bytes", method: "Reverse", args: []any{[]byte("abc")}, want: []byte("cba")},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.want, shaped(t, guest, tc.want, tc.method, tc.args...), tc.name)
	}
}

// TestInspectSeesWhatTheHostSent checks the arguments as the plugin code sees them, independently of how they
// come back: nil and empty collections, nil map values, a skipped field and the time's instant and zone.
//
//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestInspectSeesWhatTheHostSent(t *testing.T) {
	guest := loadShaper(t)
	zoned := time.Date(2026, time.March, 1, 12, 0, 0, 42, time.FixedZone("CET", 3600))

	tests := []struct {
		name  string
		order testcontract.Order
		want  testcontract.Inspection
	}{
		{
			name:  "nil collections and a skipped field",
			order: testcontract.Order{Base: testcontract.Base{ID: "z"}, When: zoned, Internal: "secret"},
			want: testcontract.Inspection{
				ID: "z", Zone: "UTC", Unix: zoned.Unix(), Nanos: 42,
				Lines: -1, ByCode: -1, Raw: -1, Tags: -1, Notes: -1, Grid: -1,
			},
		},
		{
			name:  "empty collections",
			order: testcontract.EmptyOrder(),
			want:  testcontract.Inspection{Zone: "UTC", Unix: time.Time{}.Unix()},
		},
		{
			name:  "sample",
			order: testcontract.SampleOrder(),
			want: testcontract.Inspection{
				ID: "o-1", Zone: "UTC", Unix: testcontract.SampleOrder().When.Unix(), Nanos: 123456789,
				Lines: 2, ByCode: 2, Raw: 4, Tags: 2, Notes: 3, Grid: 3, Origin: true, NilEntry: true,
			},
		},
	}

	for _, tc := range tests {
		got := shaped(t, guest, testcontract.Inspection{}, "Inspect", tc.order)
		assert.Equal(t, tc.want, got, tc.name)
	}
}

// TestNestedResultBeyondTwoToThe53 has the plugin push a nested field past 2^53: the failure names the result
// and the field, and the instance keeps serving.
//
//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestNestedResultBeyondTwoToThe53(t *testing.T) {
	guest := loadShaper(t)

	res := callShaper(t, guest, "Bump", testcontract.SampleOrder(), wire.MaxSafeInt-1)
	require.Equal(t, wire.StatusConvert, res.status, res.msg)
	assert.True(t, strings.HasPrefix(res.msg, "result 0: Lines[0].Qty: "), res.msg)
	assert.Contains(t, res.msg, strconv.Itoa(wire.MaxSafeInt+1))

	bumped, ok := shaped(t, guest, testcontract.Order{}, "Bump", testcontract.SampleOrder(), 10).(testcontract.Order)
	require.True(t, ok)
	assert.Equal(t, []int{12, 9}, []int{bumped.Lines[0].Qty, bumped.Lines[1].Qty})
}
