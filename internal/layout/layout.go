// Package layout describes how the wire encoding walks the types of an interface's methods: the wire kind of
// every type, the encoded fields of every struct, and the order the codec visits them. It is computed from two
// views of the same Go types: reflect, by the host when a contract is registered, and go/types, by `plg gen`
// for the codecs it generates. Equal [Layout.Fingerprint]s mean the generated codecs encode exactly what the
// host's reflective codec (internal/wire) does, so stale generated code is caught when the program starts.
package layout

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Kind is how the wire encoding represents a type. Integer and float kinds keep their size: decoders check
// that a value fits.
type Kind uint8

// The kinds, as the wire encoding distinguishes them.
const (
	Bool Kind = iota + 1
	Int
	Int8
	Int16
	Int32
	Int64
	Uint
	Uint8
	Uint16
	Uint32
	Uint64
	Float32
	Float64
	String
	Bytes // a slice of a uint8 kind
	Time  // time.Time
	Slice
	Map // string keys
	Pointer
	Struct
)

//nolint:gochecknoglobals // immutable lookup table
var kindNames = [...]string{
	Bool: "bool", Int: "int", Int8: "int8", Int16: "int16", Int32: "int32", Int64: "int64",
	Uint: "uint", Uint8: "uint8", Uint16: "uint16", Uint32: "uint32", Uint64: "uint64",
	Float32: "float32", Float64: "float64", String: "string", Bytes: "bytes", Time: "time",
	Slice: "slice", Map: "map", Pointer: "pointer", Struct: "struct",
}

// String returns the kind's name.
func (k Kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}

	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// Composite reports whether values of the kind contain other values (and so count towards the nesting depth).
func (k Kind) Composite() bool {
	return k >= Slice
}

// ErrUnsupported reports a type the wire encoding cannot represent.
var ErrUnsupported = errors.New("unsupported type")

// RawField is a struct field as a [View] reports it.
type RawField[T any] struct {
	Type     T
	Name     string
	Tag      reflect.StructTag
	Exported bool
}

// Class is what a [View] reports of a type: its kind and, for slices, maps and pointers, the element (map value)
// type or, for structs, every field in declaration order.
type Class[T any] struct {
	Elem   T
	Fields []RawField[T]
	Kind   Kind
}

// View classifies the Go types of one representation (reflect.Type, go/types' types.Type).
type View[T any] interface {
	// Classify returns the class of typ. It fails, wrapping [ErrUnsupported], for types the encoding cannot
	// represent (including maps whose keys are not strings).
	Classify(typ T) (Class[T], error)
	// Same reports whether a and b are the same Go type.
	Same(a, b T) bool
}

// Node is one type of a layout.
type Node[T any] struct {
	Type    T
	Elem    *Node[T]   // slices, maps (the value) and pointers
	Fields  []Field[T] // structs: the encoded fields, in declaration order
	Skipped []string   // structs: the Go names of the fields tagged `json:"-"`, which do not cross
	Kind    Kind
	MinSize int // bytes of the smallest encoding, against which decoders check length prefixes
	ID      int // composites: from 1, in the order the layout first reaches them; 0 for scalars
}

// Field is an encoded struct field.
type Field[T any] struct {
	Node   *Node[T]
	GoName string
	Name   string // the name Lua and TypeScript see: the `json` tag name, else the Go name
}

// Signature is a method as the wire encodes its calls: the parameters after the context and the results
// before the error.
type Signature[T any] struct {
	Name    string
	Params  []T
	Results []T
}

// Method is a [Signature] whose types are planned.
type Method[T any] struct {
	Name    string
	Params  []*Node[T]
	Results []*Node[T]
}

// Layout is the planned encoding of an interface's methods.
type Layout[T any] struct {
	Methods    []Method[T]
	Composites []*Node[T] // by ID - 1
	nodes      []*Node[T] // every distinct type, scalars included
}

// Build plans the methods' types with view, in order: each method's parameters, then its results, each type
// depth first. A type reached again (a recursive one included) is planned once.
func Build[T any](view View[T], methods []Signature[T]) (*Layout[T], error) {
	layout := &Layout[T]{}

	for _, sig := range methods {
		meth, err := layout.planMethod(view, sig)
		if err != nil {
			return nil, err
		}

		layout.Methods = append(layout.Methods, meth)
	}

	pending := map[*Node[T]]bool{}

	for _, node := range layout.Composites {
		if node.Kind == Struct {
			pending[node] = true
		}
	}

	for _, node := range layout.Composites {
		sizeStruct(node, pending)
	}

	for _, node := range layout.Composites {
		if node.Kind == Slice && node.Elem.MinSize == 0 {
			return nil, fmt.Errorf("%w: %v: its elements encode as nothing", ErrUnsupported, node.Type)
		}
	}

	return layout, nil
}

func (l *Layout[T]) planMethod(view View[T], sig Signature[T]) (Method[T], error) {
	meth := Method[T]{Name: sig.Name}

	for i, typ := range sig.Params {
		node, err := l.plan(view, typ)
		if err != nil {
			return meth, fmt.Errorf("method %s: parameter %d: %w", sig.Name, i+1, err)
		}

		meth.Params = append(meth.Params, node)
	}

	for i, typ := range sig.Results {
		node, err := l.plan(view, typ)
		if err != nil {
			return meth, fmt.Errorf("method %s: result %d: %w", sig.Name, i, err)
		}

		meth.Results = append(meth.Results, node)
	}

	return meth, nil
}

func (l *Layout[T]) plan(view View[T], typ T) (*Node[T], error) {
	for _, node := range l.nodes {
		if view.Same(node.Type, typ) {
			return node, nil // possibly still being planned: a recursive reference
		}
	}

	class, err := view.Classify(typ)
	if err != nil {
		return nil, err //nolint:wrapcheck // the view's error names the type; the callers add where it is
	}

	node := &Node[T]{Type: typ, Kind: class.Kind, MinSize: scalarSize(class.Kind)}
	l.nodes = append(l.nodes, node)

	if !class.Kind.Composite() {
		return node, nil
	}

	l.Composites = append(l.Composites, node)
	node.ID = len(l.Composites)

	if class.Kind == Struct {
		return node, l.planStruct(view, node, class.Fields)
	}

	node.Elem, err = l.plan(view, class.Elem)

	return node, err
}

func (l *Layout[T]) planStruct(view View[T], node *Node[T], fields []RawField[T]) error {
	names := map[string]string{} // name -> Go name

	for _, raw := range fields {
		if !raw.Exported {
			return fmt.Errorf("%w: %v: unexported field %s", ErrUnsupported, node.Type, raw.Name)
		}

		name, ok := fieldName(raw.Name, raw.Tag)
		if !ok {
			node.Skipped = append(node.Skipped, raw.Name)

			continue
		}

		if other, dup := names[name]; dup {
			return fmt.Errorf("%w: %v: fields %s and %s are both named %q", ErrUnsupported, node.Type, other,
				raw.Name, name)
		}

		names[name] = raw.Name

		elem, err := l.plan(view, raw.Type)
		if err != nil {
			return fmt.Errorf("field %v.%s: %w", node.Type, raw.Name, err)
		}

		node.Fields = append(node.Fields, Field[T]{Node: elem, GoName: raw.Name, Name: name})
	}

	return nil
}

// sizeStruct sets the minimum size of node, a pending struct, to the sum of its fields', sizing the pending
// structs among them first. It runs once the layout is complete: while planning, a struct reached again through
// a slice, map or pointer of its own fields (A{[]B}, B{A}) has no size yet. Pointers, slices and maps have
// fixed minimums, and a Go struct cannot contain itself, so the recursion ends.
func sizeStruct[T any](node *Node[T], pending map[*Node[T]]bool) int {
	if !pending[node] {
		return node.MinSize
	}

	delete(pending, node)

	for _, field := range node.Fields {
		node.MinSize += sizeStruct(field.Node, pending)
	}

	return node.MinSize
}

// fieldName returns the name a struct field has for other guest languages, as internal/wire names it: its
// `json` tag name, else its Go name. It reports false for a field tagged `json:"-"`.
func fieldName(goName string, tag reflect.StructTag) (string, bool) {
	value, tagged := tag.Lookup("json")
	name, _, _ := strings.Cut(value, ",")

	switch {
	case tagged && value == "-":
		return "", false
	case name == "":
		return goName, true
	default:
		return name, true
	}
}

// scalarSize is the minimum encoded size of a kind's values; a struct's is the sum of its fields', added as
// they are planned.
func scalarSize(kind Kind) int {
	switch kind { //nolint:exhaustive // structs start at 0, in the default branch
	case Bool, Pointer:
		return 1
	case String, Bytes, Slice, Map:
		return 4 //nolint:mnd // the uint32 length prefix
	case Time:
		return 16 //nolint:mnd // two words
	case Int, Int8, Int16, Int32, Int64, Uint, Uint8, Uint16, Uint32, Uint64, Float32, Float64:
		return 8 //nolint:mnd // a word
	default:
		return 0
	}
}

// Fingerprint identifies the layout: two layouts with the same fingerprint encode the same way, with the
// same Go field names.
func (l *Layout[T]) Fingerprint() string {
	var text strings.Builder

	for _, meth := range l.Methods {
		text.WriteString(meth.Name)
		writeList(&text, meth.Params)
		writeList(&text, meth.Results)
		text.WriteByte('\n')
	}

	for _, node := range l.Composites {
		writeComposite(&text, node)
	}

	sum := sha256.Sum256([]byte(text.String()))

	return hex.EncodeToString(sum[:])
}

func writeList[T any](text *strings.Builder, nodes []*Node[T]) {
	text.WriteByte('(')

	for i, node := range nodes {
		if i > 0 {
			text.WriteByte(',')
		}

		text.WriteString(ref(node))
	}

	text.WriteByte(')')
}

func writeComposite[T any](text *strings.Builder, node *Node[T]) {
	text.WriteString(ref(node) + " " + node.Kind.String())

	if node.Kind != Struct {
		text.WriteString(" " + ref(node.Elem) + "\n")

		return
	}

	for _, field := range node.Fields {
		text.WriteString(" " + field.GoName + ":" + ref(field.Node))
	}

	for _, name := range node.Skipped {
		text.WriteString(" " + name + ":-")
	}

	text.WriteByte('\n')
}

func ref[T any](node *Node[T]) string {
	if node.ID > 0 {
		return "#" + strconv.Itoa(node.ID)
	}

	return node.Kind.String()
}
