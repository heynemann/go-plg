package testcontract

import (
	"context"
	"time"
)

// Point is a small struct reached through a pointer.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Line is an order line: json tag names, including one with options.
type Line struct {
	SKU   string  `json:"sku"`
	Qty   int     `json:"qty"`
	Price float64 `json:"price,omitempty"`
}

// Base is embedded in Order: it crosses as one field named Base.
type Base struct {
	ID string `json:"id"`
}

// Order nests every data type the interface contract allows.
type Order struct {
	Base

	Internal string `json:"-"` // never crosses
	When     time.Time
	Lines    []Line
	ByCode   map[string]*Line
	Raw      []byte
	Origin   *Point
	Tags     []string
	Notes    map[string][]string
	Grid     [][]float32
}

// Tree is a recursive type.
type Tree struct {
	Name string
	Kids []*Tree
}

// Inspection is what a plugin saw of an Order. A length is -1 for a nil slice or map.
type Inspection struct {
	ID       string
	Zone     string
	Internal string
	Unix     int64
	Nanos    int
	Lines    int
	ByCode   int
	Raw      int
	Tags     int
	Notes    int
	Grid     int
	Origin   bool
	NilEntry bool // some ByCode value is a nil *Line
}

// Shaper is implemented by plugins that exercise every data type across the boundary.
type Shaper interface {
	// Echo returns o unchanged.
	Echo(ctx context.Context, o Order) (Order, error)
	// EchoPtr returns p unchanged, nil included.
	EchoPtr(ctx context.Context, p *Order) (*Order, error)
	// Inspect reports what the plugin received.
	Inspect(ctx context.Context, o Order) (Inspection, error)
	// Bump adds by to the quantity of every line.
	Bump(ctx context.Context, o Order, by int) (Order, error)
	// Grow builds a tree of the given depth where every node has width kids.
	Grow(ctx context.Context, depth, width int) (*Tree, error)
	// Index groups lines by SKU; nil lines give a nil map.
	Index(ctx context.Context, lines []Line) (map[string][]Line, error)
	// Reverse returns b reversed; nil stays nil.
	Reverse(ctx context.Context, b []byte) ([]byte, error)
	// Later returns t plus the given number of seconds.
	Later(ctx context.Context, t time.Time, seconds int) (time.Time, error)
	// Sum returns base plus every value of more.
	Sum(ctx context.Context, base int, more ...int) (int, error)
}
