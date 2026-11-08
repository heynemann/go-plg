package testcase

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/heynemann/go-plg/internal/wire"
)

// refKey is the key of a mapping that takes its value from a fixture: {$ref: <path>}.
const refKey = "$ref"

// timeType is time.Time, a data type of its own: written as an RFC 3339 timestamp.
var timeType = reflect.TypeFor[time.Time]() //nolint:gochecknoglobals // a constant type

// isBytes reports whether typ is a slice of a uint8 kind, which crosses as bytes, written in base64.
func isBytes(typ reflect.Type) bool {
	return typ.Kind() == reflect.Slice && typ.Elem().Kind() == reflect.Uint8
}

// nillable reports whether values of typ's kind may be nil (null).
func nillable(typ reflect.Type) bool {
	return typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map
}

// decoder decodes the values of a plugin's test cases into the contract's types, following fixtures.
type decoder struct {
	files fs.FS
	refs  []string // the fixtures being decoded, outermost first
}

// decode decodes node, in file, into a new value of typ; path names the value in errors.
func (d *decoder) decode(file string, node *yaml.Node, typ reflect.Type, path string) (reflect.Value, error) {
	out := reflect.New(typ).Elem()

	err := d.into(file, node, out, path)
	if err != nil {
		return reflect.Value{}, err
	}

	return out, nil
}

// into decodes node, in file, into out.
func (d *decoder) into(file string, node *yaml.Node, out reflect.Value, path string) error {
	typ := out.Type()

	switch {
	case isRef(node):
		return d.fixture(file, node.Content[1], out, path)
	case node.Kind == yaml.ScalarNode && node.Tag == nullTag && nillable(typ):
		return nil // out is the zero value: nil
	case typ == timeType:
		return d.timestamp(file, node, out, path)
	case isBytes(typ):
		return d.bytes(file, node, out, path)
	default:
		return d.composite(file, node, out, path)
	}
}

// composite decodes node into out, a pointer, list, map, struct or scalar.
func (d *decoder) composite(file string, node *yaml.Node, out reflect.Value, path string) error {
	switch out.Kind() { //nolint:exhaustive // the contract's data types only: the others fail as scalars
	case reflect.Pointer:
		out.Set(reflect.New(out.Type().Elem()))

		return d.into(file, node, out.Elem(), path)
	case reflect.Slice:
		return d.slice(file, node, out, path)
	case reflect.Map:
		return d.dict(file, node, out, path)
	case reflect.Struct:
		return d.record(file, node, out, path)
	default:
		return d.scalar(file, node, out, path)
	}
}

// isRef reports whether node is {$ref: <path>}.
func isRef(node *yaml.Node) bool {
	return node.Kind == yaml.MappingNode && len(node.Content) == 2 && node.Content[0].Value == refKey
}

// fixture decodes the fixture ref names, from file, into out.
func (d *decoder) fixture(file string, ref *yaml.Node, out reflect.Value, path string) error {
	name, err := text(file, ref, refKey)
	if err != nil {
		return failAt(file, ref, path, "$ref is a path")
	}

	cleaned, err := cleanRef(name)
	if err != nil {
		return failAt(file, ref, path, "%w", err)
	}

	if slices.Contains(d.refs, cleaned) {
		return failAt(file, ref, path, "%s refers to itself (through %s)", cleaned, strings.Join(d.refs, " -> "))
	}

	root, err := readYAML(d.files, cleaned)
	if err != nil {
		return failAt(file, ref, path, "$ref: %w", err)
	}

	d.refs = append(d.refs, cleaned)
	defer func() { d.refs = d.refs[:len(d.refs)-1] }()

	return d.into(cleaned, root, out, path)
}

// failAt returns the error of the value at path, written at node in file.
func failAt(file string, node *yaml.Node, path, format string, args ...any) error {
	return errorAt(at(file, node), "%s: %w", path, fmt.Errorf(format, args...)) //nolint:err113 // author's file
}

// mismatch reports a node that is not a value of typ.
func mismatch(file string, node *yaml.Node, typ reflect.Type, path string) error {
	return failAt(file, node, path, "%s is not %s", describe(node), kindOf(typ))
}

// kindOf names what values of typ are, for errors.
func kindOf(typ reflect.Type) string {
	switch {
	case typ == timeType:
		return "a timestamp (RFC 3339)"
	case isBytes(typ):
		return "bytes (a base64 string)"
	default:
		return kindNames[typ.Kind()]
	}
}

//nolint:gochecknoglobals // a constant table
var kindNames = map[reflect.Kind]string{
	reflect.Bool: "a bool", reflect.String: "a string", reflect.Slice: "a list", reflect.Map: aMapping,
	reflect.Struct: aMapping, reflect.Float32: "a number (float32)", reflect.Float64: "a number (float64)",
	reflect.Int: "an integer (int)", reflect.Int8: "an integer (int8)", reflect.Int16: "an integer (int16)",
	reflect.Int32: "an integer (int32)", reflect.Int64: "an integer (int64)", reflect.Uint: "an integer (uint)",
	reflect.Uint8: "an integer (uint8)", reflect.Uint16: "an integer (uint16)", reflect.Uint32: "an integer (uint32)",
	reflect.Uint64: "an integer (uint64)", reflect.Pointer: aMapping,
}

// timestamp decodes an RFC 3339 timestamp.
func (*decoder) timestamp(file string, node *yaml.Node, out reflect.Value, path string) error {
	if node.Kind != yaml.ScalarNode || (node.Tag != timeTag && node.Tag != strTag) {
		return mismatch(file, node, out.Type(), path)
	}

	parsed, err := time.Parse(time.RFC3339Nano, node.Value)
	if err != nil {
		return mismatch(file, node, out.Type(), path)
	}

	out.Set(reflect.ValueOf(parsed))

	return nil
}

// bytes decodes a base64 string.
func (*decoder) bytes(file string, node *yaml.Node, out reflect.Value, path string) error {
	if node.Kind != yaml.ScalarNode || node.Tag != strTag {
		return mismatch(file, node, out.Type(), path)
	}

	decoded, err := base64.StdEncoding.DecodeString(node.Value)
	if err != nil {
		return mismatch(file, node, out.Type(), path)
	}

	out.SetBytes(decoded)

	return nil
}

// slice decodes a list.
func (d *decoder) slice(file string, node *yaml.Node, out reflect.Value, path string) error {
	if node.Kind != yaml.SequenceNode {
		return mismatch(file, node, out.Type(), path)
	}

	out.Set(reflect.MakeSlice(out.Type(), len(node.Content), len(node.Content)))

	for i, item := range node.Content {
		err := d.into(file, item, out.Index(i), path+"["+strconv.Itoa(i)+"]")
		if err != nil {
			return err
		}
	}

	return nil
}

// dict decodes a mapping into a map with string keys.
func (d *decoder) dict(file string, node *yaml.Node, out reflect.Value, path string) error {
	if node.Kind != yaml.MappingNode {
		return mismatch(file, node, out.Type(), path)
	}

	out.Set(reflect.MakeMapWithSize(out.Type(), len(node.Content)/2))

	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := reflect.New(out.Type().Elem()).Elem()

		err := d.into(file, node.Content[i+1], value, path+"["+strconv.Quote(key)+"]")
		if err != nil {
			return err
		}

		out.SetMapIndex(reflect.ValueOf(key).Convert(out.Type().Key()), value)
	}

	return nil
}

// record decodes a mapping into a struct, by the names its fields have for other guest languages.
func (d *decoder) record(file string, node *yaml.Node, out reflect.Value, path string) error {
	if node.Kind != yaml.MappingNode {
		return mismatch(file, node, out.Type(), path)
	}

	fields := fieldsOf(out.Type())

	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]

		index := slices.IndexFunc(fields, func(f field) bool { return f.name == key.Value })
		if index < 0 {
			return failAt(file, key, path, "unknown field %q (fields: %s)", key.Value, names(fields))
		}

		err := d.into(file, node.Content[i+1], out.Field(fields[index].index), join(path, key.Value))
		if err != nil {
			return err
		}
	}

	return nil
}

// scalar decodes a bool, a number or a string.
func (*decoder) scalar(file string, node *yaml.Node, out reflect.Value, path string) error {
	if node.Kind != yaml.ScalarNode || !setScalar(node, out) {
		return mismatch(file, node, out.Type(), path)
	}

	return nil
}

// setScalar sets out, a bool, a number or a string, to the value of node, reporting whether node holds one of
// out's kind (a number in out's range).
func setScalar(node *yaml.Node, out reflect.Value) bool {
	switch out.Kind() { //nolint:exhaustive // the contract's scalar kinds; the others are no scalars
	case reflect.Bool:
		var value bool

		ok := node.Tag == boolTag && node.Decode(&value) == nil
		out.SetBool(value)

		return ok
	case reflect.String:
		out.SetString(node.Value)

		return node.Tag == strTag
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return setInt(node, out)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return setUint(node, out)
	case reflect.Float32, reflect.Float64:
		return setFloat(node, out)
	default:
		return false
	}
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

// field is a struct field that crosses the plugin boundary: the name other guest languages see and its index.
type field struct {
	name  string
	index int
}

// fieldsOf returns the fields of typ, a struct, that cross the plugin boundary, in declaration order.
func fieldsOf(typ reflect.Type) []field {
	fields := make([]field, 0, typ.NumField())

	for i := range typ.NumField() {
		sf := typ.Field(i)
		if !sf.IsExported() {
			continue
		}

		if name, ok := wire.FieldName(&sf); ok {
			fields = append(fields, field{name: name, index: i})
		}
	}

	return fields
}

// names lists fields' names.
func names(fields []field) string {
	listed := make([]string, len(fields))
	for i, f := range fields {
		listed[i] = f.name
	}

	return strings.Join(listed, ", ")
}

// join appends a field's name to path (empty for a value of its own).
func join(path, name string) string {
	if path == "" {
		return name
	}

	return path + "." + name
}

// Format writes v as a test case would: YAML's flow style, struct fields by the names other guest languages see,
// strings quoted, nil values null, bytes in base64 and times in RFC 3339.
func Format(v reflect.Value) string {
	var builder strings.Builder

	writeValue(&builder, v)

	return builder.String()
}

func writeValue(builder *strings.Builder, value reflect.Value) {
	switch {
	case value.Type() == timeType:
		when, _ := reflect.TypeAssert[time.Time](value) // timeType
		builder.WriteString(when.Format(time.RFC3339Nano))
	case nillable(value.Type()) && value.IsNil():
		builder.WriteString("null")
	case isBytes(value.Type()):
		builder.WriteString(strconv.Quote(base64.StdEncoding.EncodeToString(value.Bytes())))
	default:
		formatComposite(builder, value)
	}
}

// formatComposite writes value, a non-nil pointer, list, map or struct, or a scalar.
func formatComposite(builder *strings.Builder, value reflect.Value) {
	switch value.Kind() { //nolint:exhaustive // the contract's data types; the others are scalars
	case reflect.Pointer:
		writeValue(builder, value.Elem())
	case reflect.Slice:
		builder.WriteByte('[')

		for i := range value.Len() {
			separate(builder, i)
			writeValue(builder, value.Index(i))
		}

		builder.WriteByte(']')
	case reflect.Map:
		formatMap(builder, value)
	case reflect.Struct:
		builder.WriteByte('{')

		for i, f := range fieldsOf(value.Type()) {
			separate(builder, i)
			builder.WriteString(f.name + ": ")
			writeValue(builder, value.Field(f.index))
		}

		builder.WriteByte('}')
	case reflect.String:
		builder.WriteString(strconv.Quote(value.String()))
	default:
		fmt.Fprint(builder, value.Interface())
	}
}

// separate writes the separator before the item at index i of a list or mapping.
func separate(builder *strings.Builder, i int) {
	if i > 0 {
		builder.WriteString(", ")
	}
}

// formatMap writes a map, its keys in order.
func formatMap(builder *strings.Builder, v reflect.Value) {
	builder.WriteByte('{')

	for i, key := range sortedKeys(v) {
		separate(builder, i)
		builder.WriteString(strconv.Quote(key.String()) + ": ")
		writeValue(builder, v.MapIndex(key))
	}

	builder.WriteByte('}')
}

// sortedKeys returns the keys of v, a map with string keys, in order.
func sortedKeys(v reflect.Value) []reflect.Value {
	keys := v.MapKeys()
	slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })

	return keys
}

// diff appends to out a line per difference between want and got, two values of one type, each naming where it
// is (below path) and the two values there.
func diff(path string, want, got reflect.Value, out []string) []string {
	switch typ := want.Type(); {
	case typ == timeType:
		wantTime, _ := reflect.TypeAssert[time.Time](want) // timeType
		gotTime, _ := reflect.TypeAssert[time.Time](got)   // timeType

		if !wantTime.Equal(gotTime) {
			return differ(out, path, want, got)
		}
	case nillable(typ) && (want.IsNil() || got.IsNil()):
		if want.IsNil() != got.IsNil() {
			return differ(out, path, want, got)
		}
	case isBytes(typ):
		if !bytes.Equal(want.Bytes(), got.Bytes()) {
			return differ(out, path, want, got)
		}
	default:
		return diffComposite(path, want, got, out)
	}

	return out
}

// differ appends to out the line showing want and got, which differ at path.
func differ(out []string, path string, want, got reflect.Value) []string {
	return append(out, path+": expected "+Format(want)+", got "+Format(got))
}

// diffComposite is diff for non-nil pointers, lists, maps and structs, and for scalars.
func diffComposite(path string, want, got reflect.Value, out []string) []string {
	switch want.Kind() { //nolint:exhaustive // the contract's data types; the others are scalars
	case reflect.Pointer:
		return diff(path, want.Elem(), got.Elem(), out)
	case reflect.Slice:
		if want.Len() != got.Len() {
			return differ(out, path, want, got)
		}

		for i := range want.Len() {
			out = diff(path+"["+strconv.Itoa(i)+"]", want.Index(i), got.Index(i), out)
		}
	case reflect.Map:
		return diffMap(path, want, got, out)
	case reflect.Struct:
		for _, f := range fieldsOf(want.Type()) {
			out = diff(join(path, f.name), want.Field(f.index), got.Field(f.index), out)
		}
	default:
		if !want.Equal(got) {
			return differ(out, path, want, got)
		}
	}

	return out
}

// diffMap is diff for maps: for each key in order, a key only one of them has, or the difference of its values.
func diffMap(path string, want, got reflect.Value, out []string) []string {
	for _, key := range sortedKeys(want) {
		where := path + "[" + strconv.Quote(key.String()) + "]"

		value := got.MapIndex(key)
		if !value.IsValid() {
			out = append(out, where+": expected "+Format(want.MapIndex(key))+", got nothing")

			continue
		}

		out = diff(where, want.MapIndex(key), value, out)
	}

	for _, key := range sortedKeys(got) {
		if !want.MapIndex(key).IsValid() {
			out = append(out, path+"["+strconv.Quote(key.String())+"]: expected nothing, got "+
				Format(got.MapIndex(key)))
		}
	}

	return out
}
