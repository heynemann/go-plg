package testcase

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"go.yaml.in/yaml/v3"
)

// shape is how the values of a type are decoded, written and compared: every type of a shape alike. Switches over
// it list every shape (the exhaustive linter holds them to it), so a new shape needs each one's case. The scalar
// shapes come before shapeSlice; what sets each apart is its entry of scalars.
type shape uint8

const (
	shapeBool shape = iota + 1
	shapeInteger
	shapeFloat
	shapeString
	shapeBytes
	shapeTime
	shapeSlice
	shapeMap
	shapePointer
	shapeStruct
)

// timeType is time.Time, a data type of its own: written as an RFC 3339 timestamp.
var timeType = reflect.TypeFor[time.Time]() //nolint:gochecknoglobals // a constant type

// kindShapes are the shapes of the reflect kinds of the wire's data types, but for times and bytes;
// TestShapeOfAgreesWithTheWireKinds holds it to every kind of the wire.
//
//nolint:gochecknoglobals // immutable lookup table
var kindShapes = [...]shape{
	reflect.Bool: shapeBool, reflect.Int: shapeInteger, reflect.Int8: shapeInteger, reflect.Int16: shapeInteger,
	reflect.Int32: shapeInteger, reflect.Int64: shapeInteger, reflect.Uint: shapeInteger,
	reflect.Uint8: shapeInteger, reflect.Uint16: shapeInteger, reflect.Uint32: shapeInteger,
	reflect.Uint64: shapeInteger, reflect.Float32: shapeFloat, reflect.Float64: shapeFloat,
	reflect.String: shapeString, reflect.Slice: shapeSlice, reflect.Map: shapeMap, reflect.Pointer: shapePointer,
	reflect.Struct: shapeStruct,
}

// shapeOf returns typ's shape. A type without one panics instead of being taken for another shape: the contract's
// types are checked to cross the wire before its cases or examples are read.
func shapeOf(typ reflect.Type) shape {
	switch kind := typ.Kind(); {
	case typ == timeType:
		return shapeTime
	case kind == reflect.Slice && typ.Elem().Kind() == reflect.Uint8:
		return shapeBytes
	case int(kind) < len(kindShapes) && kindShapes[kind] != 0:
		return kindShapes[kind]
	default:
		panic(fmt.Sprintf("testcase: %s has no shape", typ))
	}
}

// noShape is the panic of a switch over shapes given none: shapeOf returns no such shape.
func noShape(s shape) string {
	return fmt.Sprintf("testcase: %d is no shape", s)
}

// nillable reports whether values of the shape may be nil (null).
func (s shape) nillable() bool {
	switch s {
	case shapeBytes, shapeSlice, shapeMap, shapePointer:
		return true
	case shapeBool, shapeInteger, shapeFloat, shapeString, shapeTime, shapeStruct:
		return false
	default:
		panic(noShape(s))
	}
}

// scalar is what sets the values of a scalar shape apart.
type scalar struct {
	// set sets out to the value of node, a scalar, reporting whether node holds one of out's type (a number in its
	// range).
	set func(node *yaml.Node, out reflect.Value) bool
	// format returns value as a test case writes it.
	format func(value reflect.Value) string
	// equal reports whether want and got, of one type, are the same value.
	equal func(want, got reflect.Value) bool
}

// scalars are the scalar shapes' scalars; TestScalarsRoundTripEveryScalarShape holds it to every shape before
// shapeSlice.
//
//nolint:gochecknoglobals // immutable lookup table
var scalars = [...]scalar{
	shapeBool:    {set: setBool, format: formatBool, equal: reflect.Value.Equal},
	shapeInteger: {set: setInteger, format: formatInteger, equal: reflect.Value.Equal},
	shapeFloat:   {set: setFloat, format: formatFloat, equal: reflect.Value.Equal},
	shapeString:  {set: setString, format: formatString, equal: reflect.Value.Equal},
	shapeBytes:   {set: setBytes, format: formatBytes, equal: equalBytes},
	shapeTime:    {set: setTime, format: formatTime, equal: equalTimes},
}

func setBool(node *yaml.Node, out reflect.Value) bool {
	var value bool

	ok := node.Tag == boolTag && node.Decode(&value) == nil
	out.SetBool(value)

	return ok
}

// setInteger sets out, a signed or an unsigned integer.
func setInteger(node *yaml.Node, out reflect.Value) bool {
	if out.CanInt() {
		return setInt(node, out)
	}

	return setUint(node, out)
}

func setInt(node *yaml.Node, out reflect.Value) bool {
	var value int64

	ok := node.Tag == intTag && node.Decode(&value) == nil && !out.OverflowInt(value)
	out.SetInt(value)

	return ok
}

func setUint(node *yaml.Node, out reflect.Value) bool {
	var value uint64

	ok := node.Tag == intTag && node.Decode(&value) == nil && !out.OverflowUint(value)
	out.SetUint(value)

	return ok
}

func setFloat(node *yaml.Node, out reflect.Value) bool {
	var value float64

	ok := (node.Tag == floatTag || node.Tag == intTag) && node.Decode(&value) == nil && !out.OverflowFloat(value)
	out.SetFloat(value)

	return ok
}

func setString(node *yaml.Node, out reflect.Value) bool {
	out.SetString(node.Value)

	return node.Tag == strTag
}

// setBytes decodes a base64 string.
func setBytes(node *yaml.Node, out reflect.Value) bool {
	if node.Tag != strTag {
		return false
	}

	decoded, err := base64.StdEncoding.DecodeString(node.Value)
	if err != nil {
		return false
	}

	out.SetBytes(decoded)

	return true
}

// setTime decodes an RFC 3339 timestamp.
func setTime(node *yaml.Node, out reflect.Value) bool {
	if node.Tag != timeTag && node.Tag != strTag {
		return false
	}

	parsed, err := time.Parse(time.RFC3339Nano, node.Value)
	if err != nil {
		return false
	}

	out.Set(reflect.ValueOf(parsed))

	return true
}

func formatBool(value reflect.Value) string {
	return strconv.FormatBool(value.Bool())
}

func formatInteger(value reflect.Value) string {
	if value.CanInt() {
		return strconv.FormatInt(value.Int(), 10)
	}

	return strconv.FormatUint(value.Uint(), 10)
}

func formatFloat(value reflect.Value) string {
	return strconv.FormatFloat(value.Float(), 'g', -1, value.Type().Bits())
}

func formatString(value reflect.Value) string {
	return strconv.Quote(value.String())
}

func formatBytes(value reflect.Value) string {
	return strconv.Quote(base64.StdEncoding.EncodeToString(value.Bytes()))
}

func formatTime(value reflect.Value) string {
	when, _ := reflect.TypeAssert[time.Time](value) // shapeTime's type

	return when.Format(time.RFC3339Nano)
}

func equalBytes(want, got reflect.Value) bool {
	return bytes.Equal(want.Bytes(), got.Bytes())
}

func equalTimes(want, got reflect.Value) bool {
	wantTime, _ := reflect.TypeAssert[time.Time](want) // shapeTime's type
	gotTime, _ := reflect.TypeAssert[time.Time](got)   // shapeTime's type

	return wantTime.Equal(gotTime)
}
