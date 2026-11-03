package testcontract

import "time"

// SampleOrder returns an Order using every data type, with nil and empty collections, a nil map value and a
// time with nanoseconds: what the tests send through plugins.
func SampleOrder() Order {
	return Order{
		Base:  Base{ID: "o-1"}, //nolint:modernize // embedlit: the guest's yaegi interprets this file
		When:  time.Date(2026, time.November, 3, 10, 30, 0, 123456789, time.UTC),
		Lines: []Line{{SKU: "a", Qty: 2, Price: 2}, {SKU: "b", Qty: -1}},
		ByCode: map[string]*Line{
			"a":    {SKU: "a", Qty: 2, Price: 2},
			"none": nil,
		},
		Raw:    []byte{0, 1, 2, 255},
		Origin: &Point{X: -1, Y: 2},
		Tags:   []string{"x", ""},
		Notes:  map[string][]string{"empty": {}, "nil": nil, "two": {"p", "q"}},
		Grid:   [][]float32{{1, 2.5}, nil, {}},
	}
}

// EmptyOrder returns an Order whose slices and maps are empty but not nil.
func EmptyOrder() Order {
	return Order{
		Lines:  []Line{},
		ByCode: map[string]*Line{},
		Raw:    []byte{},
		Tags:   []string{},
		Notes:  map[string][]string{},
		Grid:   [][]float32{},
	}
}
