// Package fastcontract is a contract whose methods reach every kind of type plg gen's fast paths encode: named
// scalars, byte slices of named bytes, recursive slices, named maps with named keys, anonymous structs,
// pointers to structs and times, skipped and embedded fields, recursion through pointers and slices. The tests hold its
// generated codecs to internal/wire's reflective codec. No plugin implements it.
package fastcontract

import (
	"context"
	"time"
)

//go:generate go run github.com/heynemann/go-plg/cmd/plg gen Kinds

type (
	// Flag is a named bool.
	Flag bool
	// Small is a named int8: decoding checks its range.
	Small int8
	// Port is a named uint16.
	Port uint16
	// Code is a named string, also a map key.
	Code string
	// Temp is a named float32: decoding checks its range.
	Temp float32
	// Ratio is a named float64.
	Ratio float64
	// Octet is a named byte.
	Octet uint8
	// Blob is a named byte slice.
	Blob []byte
	// Octets is a slice of a named byte: it crosses as bytes.
	Octets []Octet
	// List is a recursive slice.
	List []List
	// Index is a named map with a named key.
	Index map[Code][]Small
)

// Inner is reached by value, by pointer and as a map value.
type Inner struct {
	Hidden string `json:"-"`
	Tags   []string
	A      int `json:"a"`
}

// Meta is embedded in Every: it crosses as one field named Meta.
type Meta struct {
	Labels map[string]string
}

// Every holds every kind once (in the order govet's fieldalignment wants).
type Every struct {
	Meta

	When   time.Time
	At     *time.Time
	Index  Index
	Ptr    *Inner
	ByName map[string]Inner
	ByPtr  map[string]*Inner
	Next   *Every
	S      string
	Code   Code
	Skip   string `json:"-"`
	Raw    []byte
	Blob   Blob
	Octets Octets
	List   List
	Grid   [][]int16
	Kids   []Every
	Inner  Inner
	Anon   struct { //nolint:revive // nested-structs: an anonymous struct type is a case the codecs must cover
		Y []Ratio
		X int `json:"x"`
	}

	I     int
	I64   int64
	U     uint
	U64   uint64
	F64   float64
	Ratio Ratio
	I32   int32
	U32   uint32
	F32   float32
	Temp  Temp
	I16   int16
	U16   uint16
	Port  Port
	B     bool
	I8    int8
	U8    uint8
	Flag  Flag
	Small Small
}

// Kinds sends every kind of value across, as arguments and as results.
type Kinds interface {
	// Every returns e.
	Every(ctx context.Context, e Every) (Every, error)
	// Many mixes scalar, composite and variadic parameters with several results.
	Many(ctx context.Context, f Flag, s Small, c Code, more ...Temp) (Port, Ratio, Octets, map[string]Small, error)
	// Nothing has no arguments and no results.
	Nothing(ctx context.Context) error
	// Tree returns l.
	Tree(ctx context.Context, l List) (List, error)
	// Point returns p.
	Point(ctx context.Context, p *Inner) (*Inner, error)
}
