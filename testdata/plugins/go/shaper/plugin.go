// Package shaper is the Go test plugin for testcontract.Shaper: every data type crosses it.
package shaper

import (
	"context"
	"strconv"
	"time"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type shaper struct{}

// Create returns the plugin.
func Create() (testcontract.Shaper, error) {
	return shaper{}, nil
}

func (shaper) Echo(_ context.Context, o testcontract.Order) (testcontract.Order, error) {
	return o, nil
}

func (shaper) EchoPtr(_ context.Context, p *testcontract.Order) (*testcontract.Order, error) {
	return p, nil
}

func (shaper) Inspect(_ context.Context, o testcontract.Order) (testcontract.Inspection, error) {
	in := testcontract.Inspection{
		ID:       o.ID,
		Zone:     o.When.Location().String(),
		Internal: o.Internal,
		Unix:     o.When.Unix(),
		Nanos:    o.When.Nanosecond(),
		Lines:    -1,
		ByCode:   -1,
		Raw:      -1,
		Tags:     -1,
		Notes:    -1,
		Grid:     -1,
		Origin:   o.Origin != nil,
	}

	if o.Lines != nil {
		in.Lines = len(o.Lines)
	}

	if o.ByCode != nil {
		in.ByCode = len(o.ByCode)
	}

	if o.Raw != nil {
		in.Raw = len(o.Raw)
	}

	if o.Tags != nil {
		in.Tags = len(o.Tags)
	}

	if o.Notes != nil {
		in.Notes = len(o.Notes)
	}

	if o.Grid != nil {
		in.Grid = len(o.Grid)
	}

	for _, line := range o.ByCode {
		if line == nil {
			in.NilEntry = true
		}
	}

	return in, nil
}

func (shaper) Bump(_ context.Context, o testcontract.Order, by int) (testcontract.Order, error) {
	for i := range o.Lines {
		o.Lines[i].Qty += by
	}

	return o, nil
}

func (shaper) Grow(_ context.Context, depth, width int) (*testcontract.Tree, error) {
	return grow("root", depth, width), nil
}

func grow(name string, depth, width int) *testcontract.Tree {
	tree := &testcontract.Tree{Name: name}
	if depth == 0 {
		return tree
	}

	tree.Kids = []*testcontract.Tree{}
	for i := 0; i < width; i++ {
		tree.Kids = append(tree.Kids, grow(name+"."+strconv.Itoa(i), depth-1, width))
	}

	return tree
}

func (shaper) Index(_ context.Context, lines []testcontract.Line) (map[string][]testcontract.Line, error) {
	if lines == nil {
		return nil, nil
	}

	index := map[string][]testcontract.Line{}
	for _, line := range lines {
		index[line.SKU] = append(index[line.SKU], line)
	}

	return index, nil
}

func (shaper) Reverse(_ context.Context, b []byte) ([]byte, error) {
	if b == nil {
		return nil, nil
	}

	out := make([]byte, len(b))
	for i, c := range b {
		out[len(b)-1-i] = c
	}

	return out, nil
}

func (shaper) Later(_ context.Context, t time.Time, seconds int) (time.Time, error) {
	return t.Add(time.Duration(seconds) * time.Second), nil
}

func (shaper) Sum(_ context.Context, base int, more ...int) (int, error) {
	for _, n := range more {
		base += n
	}

	return base, nil
}
