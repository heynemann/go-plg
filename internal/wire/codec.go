package wire

import (
	"fmt"
	"reflect"
)

// Codec encodes and decodes the values of one Go type. Build it once per type with [For] and reuse it: a
// Codec is immutable and safe for concurrent use.
type Codec struct {
	typ reflect.Type
	enc encodeFunc
	dec decodeFunc
}

// RemoteError is an error decoded from the wire: only its message crosses the boundary.
type RemoteError struct {
	Message string
}

// Error returns the message.
func (e *RemoteError) Error() string {
	return e.Message
}

type (
	encodeFunc func(b []byte, val reflect.Value) ([]byte, error)
	decodeFunc func(reader *Reader, val reflect.Value) error
)

var errorType = reflect.TypeFor[error]() //nolint:gochecknoglobals // immutable type handle, computed once

// For returns the codec for typ, or an error wrapping [ErrUnsupported] when the encoding cannot represent typ.
// Named types are encoded by their kind, so a contract's `type Celsius float64` and the float64 a guest
// interpreter reports for it share one encoding.
func For(typ reflect.Type) (*Codec, error) {
	enc, dec, err := plan(typ)
	if err != nil {
		return nil, err
	}

	return &Codec{typ: typ, enc: enc, dec: dec}, nil
}

func plan(typ reflect.Type) (encodeFunc, decodeFunc, error) {
	if typ == errorType {
		return encodeError, decodeError, nil
	}

	switch typ.Kind() { //nolint:exhaustive // every other kind is unsupported, reported by the default branch
	case reflect.Bool:
		return encodeBool, decodeBool, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return encodeInt, decodeInt, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return encodeUint, decodeUint, nil
	case reflect.Float32, reflect.Float64:
		return encodeFloat, decodeFloat, nil
	case reflect.String:
		return encodeString, decodeString, nil
	default:
		return nil, nil, fmt.Errorf("%w: %s", ErrUnsupported, typ)
	}
}

// Type returns the type the codec was built for.
func (c *Codec) Type() reflect.Type {
	return c.typ
}

// Append appends the encoding of val, which must have the codec's type (or, for a guest interpreter, a type
// of the same kind).
func (c *Codec) Append(b []byte, val reflect.Value) ([]byte, error) {
	b, err := c.enc(b, val)
	if err != nil {
		return b, fmt.Errorf("encoding %s: %w", c.typ, err)
	}

	return b, nil
}

// Decode reads one value from reader into val, which must be settable and of the codec's type (or kind).
func (c *Codec) Decode(reader *Reader, val reflect.Value) error {
	err := c.dec(reader, val)
	if err != nil {
		return fmt.Errorf("decoding %s: %w", c.typ, err)
	}

	return nil
}

func encodeBool(b []byte, val reflect.Value) ([]byte, error) {
	return AppendBool(b, val.Bool()), nil
}

func decodeBool(reader *Reader, val reflect.Value) error {
	x, err := reader.Bool()
	if err != nil {
		return err
	}

	val.SetBool(x)

	return nil
}

func encodeInt(b []byte, val reflect.Value) ([]byte, error) {
	return AppendInt(b, val.Int()), nil
}

func decodeInt(reader *Reader, val reflect.Value) error {
	num, err := reader.Int64()
	if err != nil {
		return err
	}

	if val.OverflowInt(num) {
		return fmt.Errorf("%w: %d", ErrOverflow, num)
	}

	val.SetInt(num)

	return nil
}

func encodeUint(b []byte, val reflect.Value) ([]byte, error) {
	return AppendUint(b, val.Uint()), nil
}

func decodeUint(reader *Reader, val reflect.Value) error {
	num, err := reader.Uint64()
	if err != nil {
		return err
	}

	if val.OverflowUint(num) {
		return fmt.Errorf("%w: %d", ErrOverflow, num)
	}

	val.SetUint(num)

	return nil
}

func encodeFloat(b []byte, val reflect.Value) ([]byte, error) {
	return AppendFloat(b, val.Float()), nil
}

func decodeFloat(reader *Reader, val reflect.Value) error {
	num, err := reader.Float64()
	if err != nil {
		return err
	}

	if val.OverflowFloat(num) {
		return fmt.Errorf("%w: %g", ErrOverflow, num)
	}

	val.SetFloat(num)

	return nil
}

func encodeString(b []byte, val reflect.Value) ([]byte, error) {
	return AppendString(b, val.String())
}

func decodeString(reader *Reader, val reflect.Value) error {
	s, err := reader.Text()
	if err != nil {
		return err
	}

	val.SetString(s)

	return nil
}

// encodeError writes a presence byte and, for a non-nil error, its message.
func encodeError(b []byte, val reflect.Value) ([]byte, error) {
	err, present := reflect.TypeAssert[error](val) // a nil interface value does not assert
	if !present {
		return AppendBool(b, false), nil
	}

	return AppendString(AppendBool(b, true), err.Error())
}

// decodeError reads what encodeError wrote. A present error decodes to a [*RemoteError] carrying the message.
func decodeError(reader *Reader, val reflect.Value) error {
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
