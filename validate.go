package plg

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

var (
	contextType = reflect.TypeFor[context.Context]() //nolint:gochecknoglobals // immutable type handle
	errorType   = reflect.TypeFor[error]()           //nolint:gochecknoglobals // immutable type handle
	timeType    = reflect.TypeFor[time.Time]()       //nolint:gochecknoglobals // immutable type handle
)

// errNotData reports a type that cannot cross the plugin boundary: funcs, channels, interfaces and the like
// carry behavior or identity, not data.
var errNotData = errors.New("is not a data type")

// validateInterface checks iface against the interface contract: a named, non-generic interface whose
// methods are all exported, take a context.Context first, return an error last and use only data types in
// between. The error names the method and the offending type.
func validateInterface(iface reflect.Type) error {
	err := checkInterface(iface)
	if err != nil {
		return fmt.Errorf("%w %s: %w", ErrInvalidInterface, iface, err)
	}

	return nil
}

var (
	errNotInterface = errors.New("not an interface type")
	errUnnamed      = errors.New("not a named interface type")
	errGeneric      = errors.New("generic interfaces are not supported")
	errNoMethods    = errors.New("declares no methods")
	errUnexported   = errors.New("is unexported")
	errNoContext    = errors.New("first parameter must be context.Context")
	errNoError      = errors.New("last result must be error")
)

func checkInterface(iface reflect.Type) error {
	switch {
	case iface.Kind() != reflect.Interface:
		return errNotInterface
	case iface.Name() == "" || iface.PkgPath() == "":
		return errUnnamed
	case strings.ContainsRune(iface.Name(), '['):
		return errGeneric
	case iface.NumMethod() == 0:
		return errNoMethods
	}

	for m := range iface.Methods() {
		err := checkMethod(&m)
		if err != nil {
			return fmt.Errorf("method %s: %w", m.Name, err)
		}
	}

	return nil
}

func checkMethod(m *reflect.Method) error {
	if !m.IsExported() {
		return errUnexported
	}

	fn := m.Type
	if fn.NumIn() == 0 || fn.In(0) != contextType {
		return errNoContext
	}

	if fn.NumOut() == 0 || fn.Out(fn.NumOut()-1) != errorType {
		return errNoError
	}

	for k := 1; k < fn.NumIn(); k++ {
		err := checkData(fn.In(k), map[reflect.Type]bool{})
		if err != nil {
			return fmt.Errorf("parameter %d has type %s: %w", k, fn.In(k), err)
		}
	}

	for k := range fn.NumOut() - 1 {
		err := checkData(fn.Out(k), map[reflect.Type]bool{})
		if err != nil {
			return fmt.Errorf("result %d has type %s: %w", k, fn.Out(k), err)
		}
	}

	return nil
}

// checkData accepts the data types: bool, integers, floats, string, time.Time, and slices, string-keyed maps,
// structs with exported fields and struct pointers built from them. seen holds the struct types being
// checked, so recursive types terminate.
func checkData(typ reflect.Type, seen map[reflect.Type]bool) error {
	if typ == timeType {
		return nil
	}

	switch typ.Kind() { //nolint:exhaustive // every other kind is rejected by the default branch
	case reflect.Bool, reflect.String, reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return nil
	case reflect.Slice:
		return checkData(typ.Elem(), seen)
	case reflect.Map:
		if typ.Key().Kind() != reflect.String {
			return fmt.Errorf("%s %w: map keys must be strings, not %s", typ, errNotData, typ.Key())
		}

		return checkData(typ.Elem(), seen)
	case reflect.Pointer:
		if typ.Elem().Kind() != reflect.Struct {
			return fmt.Errorf("%s %w: pointers must point to structs", typ, errNotData)
		}

		return checkData(typ.Elem(), seen)
	case reflect.Struct:
		return checkStruct(typ, seen)
	default:
		return fmt.Errorf("%s %w (kind %s)", typ, errNotData, typ.Kind())
	}
}

func checkStruct(typ reflect.Type, seen map[reflect.Type]bool) error {
	if seen[typ] {
		return nil
	}

	seen[typ] = true

	for field := range typ.Fields() {
		if !field.IsExported() {
			return fmt.Errorf("%s %w: unexported field %s", typ, errNotData, field.Name)
		}

		err := checkData(field.Type, seen)
		if err != nil {
			return fmt.Errorf("field %s.%s: %w", typ, field.Name, err)
		}
	}

	return nil
}
