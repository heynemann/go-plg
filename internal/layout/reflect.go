package layout

import (
	"fmt"
	"reflect"
	"time"
)

// Reflect is the [View] of reflect types.
type Reflect struct{}

//nolint:gochecknoglobals // immutable lookup table
var reflectKinds = map[reflect.Kind]Kind{
	reflect.Bool: Bool, reflect.Int: Int, reflect.Int8: Int8, reflect.Int16: Int16, reflect.Int32: Int32,
	reflect.Int64: Int64, reflect.Uint: Uint, reflect.Uint8: Uint8, reflect.Uint16: Uint16,
	reflect.Uint32: Uint32, reflect.Uint64: Uint64, reflect.Float32: Float32, reflect.Float64: Float64,
	reflect.String: String,
}

// Classify implements [View].
func (Reflect) Classify(typ reflect.Type) (Class[reflect.Type], error) {
	if typ == reflect.TypeFor[time.Time]() {
		return Class[reflect.Type]{Kind: Time}, nil
	}

	if kind, ok := reflectKinds[typ.Kind()]; ok {
		return Class[reflect.Type]{Kind: kind}, nil
	}

	switch typ.Kind() { //nolint:exhaustive // the scalars are above; every other kind is unsupported
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			return Class[reflect.Type]{Kind: Bytes}, nil
		}

		return Class[reflect.Type]{Kind: Slice, Elem: typ.Elem()}, nil
	case reflect.Map:
		return reflectMap(typ)
	case reflect.Pointer:
		return Class[reflect.Type]{Kind: Pointer, Elem: typ.Elem()}, nil
	case reflect.Struct:
		return Class[reflect.Type]{Kind: Struct, Fields: reflectFields(typ)}, nil
	default:
		return Class[reflect.Type]{}, fmt.Errorf("%w: %s", ErrUnsupported, typ)
	}
}

func reflectMap(typ reflect.Type) (Class[reflect.Type], error) {
	if typ.Key().Kind() != reflect.String {
		return Class[reflect.Type]{}, fmt.Errorf("%w: %s: map keys must be strings", ErrUnsupported, typ)
	}

	return Class[reflect.Type]{Kind: Map, Elem: typ.Elem()}, nil
}

func reflectFields(typ reflect.Type) []RawField[reflect.Type] {
	fields := make([]RawField[reflect.Type], 0, typ.NumField())
	for field := range typ.Fields() {
		fields = append(fields, RawField[reflect.Type]{
			Type: field.Type, Name: field.Name, Tag: field.Tag, Exported: field.IsExported(),
		})
	}

	return fields
}

// Same implements [View].
func (Reflect) Same(a, b reflect.Type) bool {
	return a == b
}

// OfInterface builds the layout of iface's methods (reflect's method order), whose first parameter is the
// context and last result the error, as the interface contract requires.
func OfInterface(iface reflect.Type) (*Layout[reflect.Type], error) {
	methods := make([]Signature[reflect.Type], iface.NumMethod())

	for i := range methods {
		method := iface.Method(i)
		fn := method.Type
		sig := Signature[reflect.Type]{Name: method.Name}

		for k := 1; k < fn.NumIn(); k++ {
			sig.Params = append(sig.Params, fn.In(k))
		}

		for k := range fn.NumOut() - 1 {
			sig.Results = append(sig.Results, fn.Out(k))
		}

		methods[i] = sig
	}

	return Build[reflect.Type](Reflect{}, methods)
}
