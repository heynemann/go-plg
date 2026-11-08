// Package shapes is a contract for the scaffolds' tests: several results, a variadic method, unnamed parameters,
// parameters named like keywords and like an import, times, pointers, maps, a Host and a validation input.
package shapes

import (
	"context"
	"time"
)

// Shape is the interface plugins implement.
type Shape interface {
	Area(ctx context.Context, scale float64, at time.Time) (Measure, error)
	Corners(ctx context.Context, rest ...int) ([]Point, int, error)
	Label(context.Context, string, int) (Tag, error)
	Keywords(ctx context.Context, function string, end int, time time.Duration) error
	Find(ctx context.Context, byName map[string]*Point) (*Point, bool, error)
}

// Measure is an area.
type Measure struct {
	At     time.Time `json:"at"`
	Unit   *string   `json:"unit"`
	Raw    []byte    `json:"raw"`
	Nested Point     `json:"nested"`
	Value  float64   `json:"value"`
}

// Point is a point.
type Point struct {
	Note string `json:"note"`
	X, Y int
}

// Tag is a named string.
type Tag string

// Canvas is the Host.
type Canvas interface {
	Size(ctx context.Context) (int, error)
}

// Limits is the validation input.
type Limits struct {
	Until time.Time `json:"until"`
	Max   int       `json:"max"`
}
