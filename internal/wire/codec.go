package wire

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Codec encodes and decodes the values of one Go type. Build it once per type with a [Planner] (or [For]) and
// reuse it: a Codec is immutable and safe for concurrent use.
//
// The codec works on any reflect type the encoding supports, including the types a guest interpreter creates
// at run time: named types are encoded by their kind and structs by their fields, so a contract's
// `type Celsius float64` and the float64 an interpreter may report for it share one encoding.
type Codec struct {
	typ  reflect.Type
	root *node
}

// node is the plan of one type: its encoder and decoder, and what a [Schema] says about it. Nodes are built
// once by a Planner and immutable afterwards; recursive types refer back to their own node.
type node struct {
	typ     reflect.Type
	enc     encodeFunc
	dec     decodeFunc
	elem    *node   // slices, maps (the value) and pointers
	fields  []field // structs: the encoded fields, in declaration order
	skipped []int   // structs: the indexes of the fields tagged `json:"-"`
	minSize int     // bytes of the smallest encoding, so a length prefix is checked against the input
	claim   int     // bytes in memory of a slice's element, a map's entry (key and value) or a pointer's target
	kind    Kind
}

type field struct {
	node   *node
	name   string // the name other guest languages see
	goName string // the Go field name, for error paths
	index  int
}

type (
	// encodeFunc appends the encoding of val; depth counts the slices, maps, pointers and structs around val.
	encodeFunc func(b []byte, val reflect.Value, depth int) ([]byte, error)
	// decodeFunc decodes into val, which is settable.
	decodeFunc func(reader *Reader, val reflect.Value, depth int) error
)

var (
	errorType = reflect.TypeFor[error]()     //nolint:gochecknoglobals // immutable type handle, computed once
	timeType  = reflect.TypeFor[time.Time]() //nolint:gochecknoglobals // immutable type handle, computed once
)

// Planner builds codecs and plans each type once: the codecs of one Planner share the plans of the types
// they have in common, and a recursive type has a single plan. A Planner is not safe for concurrent use; the
// codecs it returns are.
type Planner struct {
	plans map[reflect.Type]*node
	added []reflect.Type // the plans the current For call added, forgotten if it fails
}

// For returns the codec for typ, or an error wrapping [ErrUnsupported] when the encoding cannot represent typ:
// kinds other than bool, integers, floats, strings, slices, string-keyed maps, pointers and structs (apart
// from error and time.Time), structs with unexported fields or two fields of the same name, and slices of
// elements that encode as nothing (such as struct{}), whose length no input could bound.
func (p *Planner) For(typ reflect.Type) (*Codec, error) {
	if p.plans == nil {
		p.plans = map[reflect.Type]*node{}
	}

	p.added = p.added[:0]

	root, err := p.plan(typ)
	if err == nil {
		p.sizeStructs()

		err = p.checkSlices()
	}

	if err != nil {
		for _, t := range p.added {
			delete(p.plans, t)
		}

		return nil, err
	}

	return &Codec{typ: typ, root: root}, nil
}

// sizeStructs sets the minimum size of the structs planned by the current For call, once the plans are
// complete: while planning, a struct reached again through a slice, map or pointer of its own fields (A{[]B},
// B{A}) has no size yet.
func (p *Planner) sizeStructs() {
	pending := map[*node]bool{}

	for _, t := range p.added {
		if n := p.plans[t]; n.kind == KindStruct {
			pending[n] = true
		}
	}

	for _, t := range p.added {
		sizeStruct(p.plans[t], pending)
	}
}

// sizeStruct sets the minimum size of n, a pending struct, to the sum of its fields', sizing the pending
// structs among them first. Pointers, slices and maps have fixed minimums, and a Go struct cannot contain
// itself, so the recursion ends.
func sizeStruct(n *node, pending map[*node]bool) int {
	if !pending[n] {
		return n.minSize
	}

	delete(pending, n)

	for _, f := range n.fields {
		n.minSize += sizeStruct(f.node, pending)
	}

	return n.minSize
}

// checkSlices rejects the slices planned by the current For call whose elements encode as nothing: a hostile
// length prefix could then claim any number of them without the input bounding it. It runs once the structs
// are sized.
func (p *Planner) checkSlices() error {
	for _, t := range p.added {
		n := p.plans[t]
		if n.kind == KindSlice && n.elem.minSize == 0 {
			return fmt.Errorf("%w: %s: its elements encode as nothing", ErrUnsupported, n.typ)
		}
	}

	return nil
}

// For returns the codec for typ, planned by a Planner of its own. See [Planner.For].
func For(typ reflect.Type) (*Codec, error) {
	var planner Planner

	return planner.For(typ)
}

func (p *Planner) plan(typ reflect.Type) (*node, error) {
	if planned, ok := p.plans[typ]; ok {
		return planned, nil // possibly still being filled: a recursive reference
	}

	n := &node{typ: typ}
	p.plans[typ] = n
	p.added = append(p.added, typ)

	if n.fillScalar() {
		return n, nil
	}

	var err error

	switch typ.Kind() { //nolint:exhaustive // every other kind is unsupported, reported by the default branch
	case reflect.Slice:
		err = p.fillSlice(n)
	case reflect.Map:
		err = p.fillMap(n)
	case reflect.Pointer:
		err = p.fillPointer(n)
	case reflect.Struct:
		err = p.fillStruct(n)
	default:
		err = fmt.Errorf("%w: %s", ErrUnsupported, typ)
	}

	if err != nil {
		return nil, err
	}

	return n, nil
}

func (n *node) set(kind Kind, minSize int, enc encodeFunc, dec decodeFunc) {
	n.kind, n.minSize, n.enc, n.dec = kind, minSize, enc, dec
}

// fillScalar plans the types that contain no other type, reporting whether typ is one.
func (n *node) fillScalar() bool {
	switch n.typ {
	case errorType:
		n.set(KindError, 1, encodeError, decodeError)

		return true
	case timeType:
		n.set(KindTime, 2*wordSize, encodeTime, decodeTime)

		return true
	}

	switch n.typ.Kind() { //nolint:exhaustive // the composite and unsupported kinds are planned by the caller
	case reflect.Bool:
		n.set(KindBool, 1, encodeBool, decodeBool)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n.set(KindInt, wordSize, encodeInt, decodeInt)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n.set(KindUint, wordSize, encodeUint, decodeUint)
	case reflect.Float32, reflect.Float64:
		n.set(KindFloat, wordSize, encodeFloat, decodeFloat)
	case reflect.String:
		n.set(KindString, lengthSize, encodeString, decodeString)
	default:
		return false
	}

	return true
}

// The minimum sizes below are set before the element is planned, so a recursive reference to a node still
// being filled already sees them.

func (p *Planner) fillSlice(n *node) error {
	if n.typ.Elem().Kind() == reflect.Uint8 {
		n.set(KindBytes, lengthSize, encodeBytes, decodeBytes)

		return nil
	}

	n.set(KindSlice, lengthSize, n.encodeSlice, n.decodeSlice)

	n.claim = sizeOf(n.typ.Elem())

	elem, err := p.plan(n.typ.Elem())
	n.elem = elem

	return err
}

func (p *Planner) fillMap(n *node) error {
	if n.typ.Key().Kind() != reflect.String {
		return fmt.Errorf("%w: %s: map keys must be strings", ErrUnsupported, n.typ)
	}

	n.set(KindMap, lengthSize, n.encodeMap, n.decodeMap)

	n.claim = sizeOf(n.typ.Key()) + sizeOf(n.typ.Elem())

	elem, err := p.plan(n.typ.Elem())
	n.elem = elem

	return err
}

func (p *Planner) fillPointer(n *node) error {
	n.set(KindPointer, 1, n.encodePointer, n.decodePointer)

	n.claim = sizeOf(n.typ.Elem())

	elem, err := p.plan(n.typ.Elem())
	n.elem = elem

	return err
}

// sizeOf is the memory a value of typ takes, which a decoder claims for each one it makes.
func sizeOf(typ reflect.Type) int {
	return int(typ.Size())
}

func (p *Planner) fillStruct(n *node) error {
	n.set(KindStruct, 0, n.encodeStruct, n.decodeStruct)

	names := map[string]string{} // name -> Go name

	for structField := range n.typ.Fields() {
		// yaegi (v0.16) builds a struct holding a struct still being declared by value (Child{Parent} with
		// Parent{[]Child}) around a placeholder of size 0 and swaps the field's type in afterwards, leaving the
		// struct smaller than its fields: storing a value there would overwrite memory beyond it.
		if structField.Offset+structField.Type.Size() > n.typ.Size() {
			return fmt.Errorf("%w: %s: field %s does not fit in the struct as the Go interpreter laid it out "+
				"(a struct holding, by value, a struct that holds it back through a slice or map)",
				ErrUnsupported, n.typ, structField.Name)
		}

		if !structField.IsExported() {
			return fmt.Errorf("%w: %s: unexported field %s", ErrUnsupported, n.typ, structField.Name)
		}

		name, ok := FieldName(&structField)
		if !ok {
			n.skipped = append(n.skipped, structField.Index[0])

			continue
		}

		if other, dup := names[name]; dup {
			return fmt.Errorf("%w: %s: fields %s and %s are both named %q", ErrUnsupported, n.typ, other,
				structField.Name, name)
		}

		names[name] = structField.Name

		elem, err := p.plan(structField.Type)
		if err != nil {
			return fmt.Errorf("field %s.%s: %w", n.typ, structField.Name, err)
		}

		n.fields = append(n.fields, field{
			node: elem, name: name, goName: structField.Name, index: structField.Index[0],
		})
	}

	return nil
}

// FieldName returns the name a struct field has for other guest languages: its `json` tag name, else its Go
// name (an embedded struct is one field named after its type, not flattened). It reports false for a field
// tagged `json:"-"`, which does not cross at all.
func FieldName(sf *reflect.StructField) (string, bool) {
	tag, tagged := sf.Tag.Lookup("json")
	name, _, _ := strings.Cut(tag, ",")

	switch {
	case tagged && tag == "-":
		return "", false
	case name == "":
		return sf.Name, true
	default:
		return name, true
	}
}

// Type returns the type the codec was built for.
func (c *Codec) Type() reflect.Type {
	return c.typ
}

// Append appends the encoding of val, which must have the codec's type. A failure names the field, index or
// key inside val where it happened.
func (c *Codec) Append(b []byte, val reflect.Value) ([]byte, error) {
	return c.root.enc(b, val, 0)
}

// Decode reads one value from reader into val, which must be settable and have the codec's type. Decoded
// values never alias the reader's buffer. A failure names the field, index or key where it happened; val may
// then hold part of the value.
func (c *Codec) Decode(reader *Reader, val reflect.Value) error {
	return c.root.dec(reader, val, 0)
}

// pathError locates a failure inside a value: the fields, indexes and keys leading to it.
type pathError struct {
	err  error
	path []string // innermost first, such as ".Qty", "[1]", ".Items"
}

func (e *pathError) Error() string {
	var b strings.Builder

	for _, segment := range slices.Backward(e.path) {
		b.WriteString(segment)
	}

	return strings.TrimPrefix(b.String(), ".") + ": " + e.err.Error()
}

func (e *pathError) Unwrap() error {
	return e.err
}

// within adds the segment around err's location.
func within(err error, segment string) error {
	if located, ok := errors.AsType[*pathError](err); ok {
		located.path = append(located.path, segment)

		return located
	}

	return &pathError{err: err, path: []string{segment}}
}

func encodeBool(b []byte, val reflect.Value, _ int) ([]byte, error) {
	return AppendBool(b, val.Bool()), nil
}

func decodeBool(reader *Reader, val reflect.Value, _ int) error {
	x, err := reader.Bool()
	if err != nil {
		return err
	}

	val.SetBool(x)

	return nil
}

func imprecise[T int64 | uint64](num T) error {
	return fmt.Errorf("%w: %d", ErrPrecision, num)
}

func encodeInt(b []byte, val reflect.Value, _ int) ([]byte, error) {
	return AppendInt53(b, val.Int())
}

func decodeInt(reader *Reader, val reflect.Value, _ int) error {
	num, err := reader.Int53()

	switch {
	case err != nil:
		return err
	case val.OverflowInt(num):
		return Overflow(num, val.Type().String())
	}

	val.SetInt(num)

	return nil
}

func encodeUint(b []byte, val reflect.Value, _ int) ([]byte, error) {
	return AppendUint53(b, val.Uint())
}

func decodeUint(reader *Reader, val reflect.Value, _ int) error {
	num, err := reader.Uint53()

	switch {
	case err != nil:
		return err
	case val.OverflowUint(num):
		return Overflow(num, val.Type().String())
	}

	val.SetUint(num)

	return nil
}

func encodeFloat(b []byte, val reflect.Value, _ int) ([]byte, error) {
	return AppendFloat(b, val.Float()), nil
}

func decodeFloat(reader *Reader, val reflect.Value, _ int) error {
	num, err := reader.Float64()
	if err != nil {
		return err
	}

	if val.OverflowFloat(num) {
		return Overflow(num, val.Type().String())
	}

	val.SetFloat(num)

	return nil
}

func encodeString(b []byte, val reflect.Value, _ int) ([]byte, error) {
	return AppendString(b, val.String())
}

func decodeString(reader *Reader, val reflect.Value, _ int) error {
	s, err := reader.Text()
	if err != nil {
		return err
	}

	val.SetString(s)

	return nil
}

func encodeBytes(dst []byte, val reflect.Value, _ int) ([]byte, error) {
	return AppendBytes(dst, val.Bytes()) // a nil slice's Bytes is nil
}

func decodeBytes(reader *Reader, val reflect.Value, _ int) error {
	data, err := reader.Bytes()
	if err != nil {
		return err
	}

	if data == nil {
		val.SetZero()

		return nil
	}

	val.SetBytes(data)

	return nil
}

// encodeTime writes the instant as Unix seconds and nanoseconds.
func encodeTime(b []byte, val reflect.Value, _ int) ([]byte, error) {
	t, _ := reflect.TypeAssert[time.Time](val) // the plan guarantees the type

	return AppendTime(b, t)
}

// decodeTime reads what encodeTime wrote, as a UTC time.
func decodeTime(reader *Reader, val reflect.Value, _ int) error {
	t, err := reader.Time()
	if err != nil {
		return err
	}

	dst, _ := reflect.TypeAssert[*time.Time](val.Addr()) // settable, so addressable; the plan guarantees the type
	*dst = t

	return nil
}

// encodeError writes a presence byte and, for a non-nil error, its message.
func encodeError(b []byte, val reflect.Value, _ int) ([]byte, error) {
	err, present := reflect.TypeAssert[error](val) // a nil interface value does not assert
	if !present {
		return AppendBool(b, false), nil
	}

	return AppendString(AppendBool(b, true), err.Error())
}

// RemoteError is an error decoded from the wire: only its message crosses the boundary.
type RemoteError struct {
	Message string
}

// Error returns the message.
func (e *RemoteError) Error() string {
	return e.Message
}

// decodeError reads what encodeError wrote. A present error decodes to a [*RemoteError] carrying the message.
func decodeError(reader *Reader, val reflect.Value, _ int) error {
	present, err := reader.Bool()
	if err != nil {
		return err
	}

	if !present {
		val.SetZero()

		return nil
	}

	msg, err := reader.Text()
	if err != nil {
		return err
	}

	val.Set(reflect.ValueOf(&RemoteError{Message: msg}))

	return nil
}

func (n *node) encodeSlice(dst []byte, val reflect.Value, depth int) ([]byte, error) {
	if val.IsNil() {
		return AppendNil(dst), nil
	}

	count := val.Len()

	dst, err := AppendLen(dst, count, depth)
	if err != nil {
		return dst, err
	}

	for i := range count {
		dst, err = n.elem.enc(dst, val.Index(i), depth+1)
		if err != nil {
			return dst, AtIndex(err, i)
		}
	}

	return dst, nil
}

func (n *node) decodeSlice(reader *Reader, val reflect.Value, depth int) error {
	count, err := reader.SliceLen(depth, n.elem.minSize, n.claim) // elem.minSize > 0: the plan rejects empty ones
	if err != nil {
		return err
	}

	if count < 0 {
		val.SetZero()

		return nil
	}

	items := reflect.MakeSlice(n.typ, count, count)
	for i := range count {
		err = n.elem.dec(reader, items.Index(i), depth+1)
		if err != nil {
			return AtIndex(err, i)
		}
	}

	val.Set(items)

	return nil
}

func (n *node) encodeMap(dst []byte, val reflect.Value, depth int) ([]byte, error) {
	if val.IsNil() {
		return AppendNil(dst), nil
	}

	dst, err := AppendLen(dst, val.Len(), depth)
	if err != nil {
		return dst, err
	}

	key, elem := reflect.New(n.typ.Key()).Elem(), reflect.New(n.typ.Elem()).Elem()

	var iter reflect.MapIter

	iter.Reset(val)

	for iter.Next() {
		key.SetIterKey(&iter)
		elem.SetIterValue(&iter)

		dst, err = AppendString(dst, key.String())
		if err == nil {
			dst, err = n.elem.enc(dst, elem, depth+1)
		}

		if err != nil {
			return dst, AtKey(err, key.String())
		}
	}

	return dst, nil
}

func (n *node) decodeMap(reader *Reader, val reflect.Value, depth int) error {
	count, err := reader.MapLen(depth, n.elem.minSize, n.claim)
	if err != nil {
		return err
	}

	if count < 0 {
		val.SetZero()

		return nil
	}

	entries := reflect.MakeMapWithSize(n.typ, count)
	if count > 0 { // only then: the entries' scratch value may be large, and an empty map claims no memory
		err = n.decodeEntries(reader, entries, count, depth)
		if err != nil {
			return err
		}
	}

	val.Set(entries)

	return nil
}

// decodeEntries decodes count entries into the empty map entries.
func (n *node) decodeEntries(reader *Reader, entries reflect.Value, count, depth int) error {
	key, elem := reflect.New(n.typ.Key()).Elem(), reflect.New(n.typ.Elem()).Elem()

	for range count {
		name, err := reader.Text()
		if err != nil {
			return err
		}

		err = n.elem.dec(reader, elem, depth+1) // overwrites all of elem, so it is reused
		if err != nil {
			return AtKey(err, name)
		}

		key.SetString(name)
		entries.SetMapIndex(key, elem)
	}

	if entries.Len() != count {
		return DuplicateKeys()
	}

	return nil
}

func (n *node) encodePointer(dst []byte, val reflect.Value, depth int) ([]byte, error) {
	if val.IsNil() {
		return AppendBool(dst, false), nil
	}

	err := Deeper(depth)
	if err != nil {
		return dst, err
	}

	return n.elem.enc(AppendBool(dst, true), val.Elem(), depth+1)
}

func (n *node) decodePointer(reader *Reader, val reflect.Value, depth int) error {
	present, err := reader.Present(depth, n.claim)
	if err != nil {
		return err
	}

	if !present {
		val.SetZero()

		return nil
	}

	target := reflect.New(n.typ.Elem())

	err = n.elem.dec(reader, target.Elem(), depth+1)
	if err != nil {
		return err
	}

	val.Set(target)

	return nil
}

func (n *node) encodeStruct(dst []byte, val reflect.Value, depth int) ([]byte, error) {
	err := Deeper(depth)
	if err != nil {
		return dst, err
	}

	for i := range n.fields {
		f := &n.fields[i]

		dst, err = f.node.enc(dst, val.Field(f.index), depth+1)
		if err != nil {
			return dst, AtField(err, f.goName)
		}
	}

	return dst, nil
}

func (n *node) decodeStruct(reader *Reader, val reflect.Value, depth int) error {
	err := Deeper(depth)
	if err != nil {
		return err
	}

	for i := range n.fields {
		f := &n.fields[i]

		err = f.node.dec(reader, val.Field(f.index), depth+1)
		if err != nil {
			return AtField(err, f.goName)
		}
	}

	for _, i := range n.skipped {
		val.Field(i).SetZero()
	}

	return nil
}
