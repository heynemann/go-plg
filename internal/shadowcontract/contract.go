// Package shadowcontract is a contract package redeclaring predeclared types, whose methods carry structs from
// other packages with fields of those types. The tests hold its generated codecs to internal/wire's reflective
// codec. No plugin implements it.
package shadowcontract

import (
	"context"
	"unicode"
)

//go:generate go run github.com/heynemann/go-plg/cmd/plg gen Tables

// int and uint16 shadow the predeclared types in this package only: a unicode.RangeTable's LatinOffset is still
// the predeclared int, and its ranges' bounds are still the predeclared uint16.
//
//nolint:gocritic,predeclared,revive,unused // the redeclarations are the case under test; no declaration uses them
type (
	int    = int32
	uint16 = string
)

// Tables passes structs of the unicode package.
type Tables interface {
	// Table returns t.
	Table(ctx context.Context, t unicode.RangeTable) (unicode.RangeTable, error)
	// Ranges returns r.
	Ranges(ctx context.Context, r []unicode.Range16) ([]unicode.Range16, error)
}
