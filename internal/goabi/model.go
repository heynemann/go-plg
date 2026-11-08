package goabi

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Kind is what a [Type] is.
type Kind uint8

// Type kinds.
const (
	KindNamed   Kind = iota + 1 // a named or predeclared type, written by its name
	KindPointer                 // *Elem
	KindSlice                   // []Elem
	KindArray                   // [Len]Elem
	KindMap                     // map[Key]Elem
	KindStruct                  // struct{ Fields }
)

// Type is a type as the dispatcher writes it: a named type (its definition stays in its package) or an unnamed
// pointer, slice, array, map or struct of them. Aliases are resolved, as reflection resolves them; an embedded
// field keeps the name it embeds its type under. A registered contract describes its types from reflection
// ([InterfaceOf]); plg validate describes them from go/types, so both render the same dispatcher.
type Type struct {
	Elem   *Type   // pointers, slices, arrays and maps (the value)
	Key    *Type   // maps
	Path   string  // named: the import path of the type's package; empty for predeclared types
	Pkg    string  // named: the name of the type's package
	Name   string  // named: the type's name
	Fields []Field // structs
	Len    int     // arrays
	Kind   Kind
}

// Field is a struct field.
type Field struct {
	Type     *Type
	Name     string // the field's name; an embedded field's is the name it embeds its type under
	Tag      string
	Embedded bool
}

// Func is a method's signature: Params include the leading context.Context and Results the final error. A
// variadic method's last parameter is a slice.
type Func struct {
	Params   []*Type
	Results  []*Type
	Variadic bool
}

// Method is an interface method.
type Method struct {
	Name string
	Func Func
}

// Interface is a named interface type and its methods, in reflection's method order (sorted by name).
type Interface struct {
	Type    *Type
	Methods []Method
}

// String writes t as reflection does: named types qualified by their package's name.
func (t *Type) String() string {
	switch t.Kind {
	case KindNamed:
		if t.Pkg == "" {
			return t.Name
		}

		return t.Pkg + "." + t.Name
	case KindPointer:
		return "*" + t.Elem.String()
	case KindSlice:
		return "[]" + t.Elem.String()
	case KindArray:
		return "[" + strconv.Itoa(t.Len) + "]" + t.Elem.String()
	case KindMap:
		return "map[" + t.Key.String() + "]" + t.Elem.String()
	case KindStruct:
		return t.structString()
	default:
		return "invalid type"
	}
}

func (t *Type) structString() string {
	fields := make([]string, 0, len(t.Fields))

	for i := range t.Fields {
		field := &t.Fields[i]

		written := field.Type.String()
		if !field.Embedded {
			written = field.Name + " " + written
		}

		if field.Tag != "" {
			written += " " + strconv.Quote(field.Tag)
		}

		fields = append(fields, written)
	}

	return "struct { " + strings.Join(fields, "; ") + " }"
}

// InterfaceOf describes the named interface iface from reflection. It fails with [ErrRender] when iface is not a
// named interface or a method is unexported or refers to a type the dispatcher cannot write.
func InterfaceOf(iface reflect.Type) (*Interface, error) {
	if iface.Kind() != reflect.Interface || iface.Name() == "" || iface.PkgPath() == "" {
		return nil, fmt.Errorf("%w: %s is not a named interface type", ErrRender, iface)
	}

	described := &Interface{Type: namedOf(iface), Methods: make([]Method, 0, iface.NumMethod())}

	for method := range iface.Methods() {
		if !method.IsExported() {
			return nil, fmt.Errorf("%w: method %s is unexported", ErrRender, method.Name)
		}

		fn, err := FuncOf(method.Type)
		if err != nil {
			return nil, fmt.Errorf("method %s: %w", method.Name, err)
		}

		described.Methods = append(described.Methods, Method{Name: method.Name, Func: *fn})
	}

	return described, nil
}

// FuncOf describes the func type fn from reflection.
func FuncOf(fn reflect.Type) (*Func, error) {
	described := &Func{Variadic: fn.IsVariadic()}

	for typ := range fn.Ins() {
		param, err := TypeOf(typ)
		if err != nil {
			return nil, err
		}

		described.Params = append(described.Params, param)
	}

	for typ := range fn.Outs() {
		result, err := TypeOf(typ)
		if err != nil {
			return nil, err
		}

		described.Results = append(described.Results, result)
	}

	return described, nil
}

// TypeOf describes typ from reflection, rejecting what cannot be written as a type expression: unsafe.Pointer,
// unnamed funcs, channels and interfaces, generic types and unexported struct fields.
func TypeOf(typ reflect.Type) (*Type, error) {
	if typ.Kind() == reflect.UnsafePointer {
		return nil, fmt.Errorf("%w: unsafe.Pointer", ErrRender)
	}

	if typ.Name() == "" {
		return unnamedOf(typ)
	}

	if strings.ContainsRune(typ.Name(), '[') {
		return nil, fmt.Errorf("%w: generic type %s", ErrRender, typ)
	}

	return namedOf(typ), nil
}

// unnamedOf describes an unnamed type: a pointer, slice, array, map or struct.
func unnamedOf(typ reflect.Type) (*Type, error) {
	var (
		described *Type
		err       error
	)

	switch typ.Kind() { //nolint:exhaustive // every other unnamed kind is rejected by the default branch
	case reflect.Pointer:
		described, err = elemOf(KindPointer, typ)
	case reflect.Slice:
		described, err = elemOf(KindSlice, typ)
	case reflect.Array:
		described, err = elemOf(KindArray, typ)
		if err == nil {
			described.Len = typ.Len()
		}
	case reflect.Map:
		described, err = elemOf(KindMap, typ)
		if err == nil {
			described.Key, err = TypeOf(typ.Key())
		}
	case reflect.Struct:
		described = &Type{Kind: KindStruct}
		described.Fields, err = fieldsOf(typ)
	default:
		return nil, fmt.Errorf("%w: unnamed %s type %s", ErrRender, typ.Kind(), typ)
	}

	if err != nil {
		return nil, err
	}

	return described, nil
}

// elemOf describes a pointer, slice, array or map of kind with its element.
func elemOf(kind Kind, typ reflect.Type) (*Type, error) {
	elem, err := TypeOf(typ.Elem())
	if err != nil {
		return nil, err
	}

	return &Type{Kind: kind, Elem: elem}, nil
}

func fieldsOf(typ reflect.Type) ([]Field, error) {
	fields := make([]Field, 0, typ.NumField())

	for field := range typ.Fields() {
		if !field.IsExported() {
			return nil, fmt.Errorf("%w: unexported field %s in %s", ErrRender, field.Name, typ)
		}

		described, err := TypeOf(field.Type)
		if err != nil {
			return nil, err
		}

		fields = append(fields, Field{
			Type: described, Name: field.Name, Tag: string(field.Tag), Embedded: field.Anonymous,
		})
	}

	return fields, nil
}

// namedOf describes a named type.
func namedOf(typ reflect.Type) *Type {
	pkg, _, _ := strings.Cut(typ.String(), ".")
	if typ.PkgPath() == "" {
		pkg = ""
	}

	return &Type{Kind: KindNamed, Path: typ.PkgPath(), Pkg: pkg, Name: typ.Name()}
}
