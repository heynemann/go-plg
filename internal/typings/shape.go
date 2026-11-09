package typings

import (
	"fmt"

	"github.com/heynemann/go-plg/internal/layout"
)

// shape is how the typings and skeletons tell wire kinds apart: every kind of a shape is written alike. Switches
// over it list every shape (the exhaustive linter holds them to it), so a new shape needs each one's case. The
// scalar shapes come before shapeSlice.
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

// kindShapes are the shapes of the wire kinds; TestShapeOfClassifiesEveryKind holds it to every kind layout has.
//
//nolint:gochecknoglobals // immutable lookup table
var kindShapes = [...]shape{
	layout.Bool: shapeBool, layout.Int: shapeInteger, layout.Int8: shapeInteger, layout.Int16: shapeInteger,
	layout.Int32: shapeInteger, layout.Int64: shapeInteger, layout.Uint: shapeInteger, layout.Uint8: shapeInteger,
	layout.Uint16: shapeInteger, layout.Uint32: shapeInteger, layout.Uint64: shapeInteger,
	layout.Float32: shapeFloat, layout.Float64: shapeFloat, layout.String: shapeString, layout.Bytes: shapeBytes,
	layout.Time: shapeTime, layout.Slice: shapeSlice, layout.Map: shapeMap, layout.Pointer: shapePointer,
	layout.Struct: shapeStruct,
}

// shapeOf returns kind's shape; a kind without one panics instead of being written as another shape.
func shapeOf(kind layout.Kind) shape {
	if int(kind) < len(kindShapes) && kindShapes[kind] != 0 {
		return kindShapes[kind]
	}

	panic(fmt.Sprintf("typings: wire kind %s has no shape", kind))
}

// noShape is the panic of a switch over shapes given none: shapeOf returns no such shape.
func noShape(s shape) string {
	return fmt.Sprintf("typings: %d is no shape", s)
}
