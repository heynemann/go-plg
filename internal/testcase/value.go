package testcase

import (
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/heynemann/go-plg/internal/wire"
)

// refKey is the key of a mapping that takes its value from a fixture: {$ref: <path>}.
const refKey = "$ref"

// decoder decodes the values of one test case (or one input) into the contract's types, following fixtures. It
// reads each fixture once, and counts its bytes towards maxFixtureBytes each time a $ref takes it.
type decoder struct {
	files    fs.FS
	fixtures map[string]fixture // the fixtures read, by path
	refs     []string           // the fixtures being decoded, outermost first
	taken    int                // the bytes of the fixtures the $refs took
}

// fixture is a fixture file read: its root node and its size in bytes.
type fixture struct {
	root *yaml.Node
	size int
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
	if isRef(node) {
		return d.fixture(file, node.Content[1], out, path)
	}

	shp := shapeOf(out.Type())
	if isNull(node) && shp.nillable() {
		return nil // out is the zero value: nil
	}

	switch shp {
	case shapeBool, shapeInteger, shapeFloat, shapeString, shapeBytes, shapeTime:
		return d.scalar(file, node, out, path, shp)
	case shapePointer:
		out.Set(reflect.New(out.Type().Elem()))

		return d.into(file, node, out.Elem(), path)
	case shapeSlice:
		return d.slice(file, node, out, path)
	case shapeMap:
		return d.dict(file, node, out, path)
	case shapeStruct:
		return d.record(file, node, out, path)
	default:
		panic(noShape(shp))
	}
}

// isRef reports whether node is {$ref: <path>}.
func isRef(node *yaml.Node) bool {
	return node.Kind == yaml.MappingNode && len(node.Content) == 2 && node.Content[0].Value == refKey
}

// isNull reports whether node is null.
func isNull(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Tag == nullTag
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

	root, err := d.take(cleaned)
	if err != nil {
		return failAt(file, ref, path, "$ref: %w", err)
	}

	d.refs = append(d.refs, cleaned)
	defer func() { d.refs = d.refs[:len(d.refs)-1] }()

	return d.into(cleaned, root, out, path)
}

// take returns the root node of the fixture name, reading it the first time, and counts its bytes again.
func (d *decoder) take(name string) (*yaml.Node, error) {
	if taken, read := d.fixtures[name]; read {
		err := d.count(name, taken.size)
		if err != nil {
			return nil, err
		}

		return taken.root, nil
	}

	data, err := readFile(d.files, name)
	if err == nil {
		err = d.count(name, len(data))
	}

	if err != nil {
		return nil, err
	}

	root, err := parseYAML(data, name)
	if err != nil {
		return nil, err
	}

	if d.fixtures == nil {
		d.fixtures = map[string]fixture{}
	}

	d.fixtures[name] = fixture{root: root, size: len(data)}

	return root, nil
}

// count adds the size of the fixture name, which a $ref takes, to the bytes taken, failing past maxFixtureBytes.
func (d *decoder) count(name string, size int) error {
	d.taken += size
	if d.taken > maxFixtureBytes {
		return fmt.Errorf("%s %w", name, errFixturesTooLarge)
	}

	return nil
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
	switch shp := shapeOf(typ); shp {
	case shapeBool, shapeString:
		return "a " + typ.Kind().String()
	case shapeInteger:
		return "an integer (" + typ.Kind().String() + ")"
	case shapeFloat:
		return "a number (" + typ.Kind().String() + ")"
	case shapeBytes:
		return "bytes (a base64 string)"
	case shapeTime:
		return "a timestamp (RFC 3339)"
	case shapeSlice:
		return "a list"
	case shapeMap, shapeStruct:
		return aMapping
	case shapePointer:
		return kindOf(typ.Elem())
	default:
		panic(noShape(shp))
	}
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

// scalar decodes node, a scalar, into out, of the scalar shape shp.
func (*decoder) scalar(file string, node *yaml.Node, out reflect.Value, path string, shp shape) error {
	if node.Kind != yaml.ScalarNode || !scalars[shp].set(node, out) {
		return mismatch(file, node, out.Type(), path)
	}

	return nil
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
	shp := shapeOf(value.Type())
	if shp.nillable() && value.IsNil() {
		builder.WriteString("null")

		return
	}

	switch shp {
	case shapeBool, shapeInteger, shapeFloat, shapeString, shapeBytes, shapeTime:
		builder.WriteString(scalars[shp].format(value))
	case shapePointer:
		writeValue(builder, value.Elem())
	case shapeSlice:
		builder.WriteByte('[')

		for i := range value.Len() {
			separate(builder, i)
			writeValue(builder, value.Index(i))
		}

		builder.WriteByte(']')
	case shapeMap:
		formatMap(builder, value)
	case shapeStruct:
		formatStruct(builder, value)
	default:
		panic(noShape(shp))
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

// formatStruct writes a struct, its fields in declaration order.
func formatStruct(builder *strings.Builder, v reflect.Value) {
	builder.WriteByte('{')

	for i, f := range fieldsOf(v.Type()) {
		separate(builder, i)
		builder.WriteString(f.name + ": ")
		writeValue(builder, v.Field(f.index))
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
	shp := shapeOf(want.Type())
	if shp.nillable() && (want.IsNil() || got.IsNil()) {
		if want.IsNil() == got.IsNil() {
			return out
		}

		return differ(out, path, want, got)
	}

	return diffShape(path, shp, want, got, out)
}

// differ appends to out the line showing want and got, which differ at path.
func differ(out []string, path string, want, got reflect.Value) []string {
	return append(out, path+": expected "+Format(want)+", got "+Format(got))
}

// diffShape is diff for want and got of the shape shp, neither of them nil.
func diffShape(path string, shp shape, want, got reflect.Value, out []string) []string {
	switch shp {
	case shapeBool, shapeInteger, shapeFloat, shapeString, shapeBytes, shapeTime:
		if !scalars[shp].equal(want, got) {
			return differ(out, path, want, got)
		}
	case shapePointer:
		return diff(path, want.Elem(), got.Elem(), out)
	case shapeSlice:
		return diffList(path, want, got, out)
	case shapeMap:
		return diffMap(path, want, got, out)
	case shapeStruct:
		for _, f := range fieldsOf(want.Type()) {
			out = diff(join(path, f.name), want.Field(f.index), got.Field(f.index), out)
		}
	default:
		panic(noShape(shp))
	}

	return out
}

// diffList is diff for lists: their lengths, or the difference of each item.
func diffList(path string, want, got reflect.Value, out []string) []string {
	if want.Len() != got.Len() {
		return differ(out, path, want, got)
	}

	for i := range want.Len() {
		out = diff(path+"["+strconv.Itoa(i)+"]", want.Index(i), got.Index(i), out)
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
