// Package wire is the binary encoding shared by the plg host and its guests.
//
// Values are little-endian and length-prefixed: bool is one byte (0 or 1); every integer kind is eight bytes
// (two's complement for signed kinds); every float kind is the eight-byte IEEE 754 binary64 bit pattern;
// strings are a uint32 byte length followed by the bytes; an error is a presence byte followed, when present,
// by its message as a string. A call's arguments (and a result's values) are the concatenation of their
// encodings, without a count: both sides know the signature.
//
// Every guest result buffer starts with a [Status] byte.
package wire

import (
	"encoding/binary"
	"errors"
	"math"
	"unicode/utf8"
)

// Status is the first byte of every result buffer a guest returns. Zero is deliberately invalid, so that an
// empty or zeroed buffer is never mistaken for success.
type Status byte

// Result statuses. The body after the status byte is the encoded results for [StatusOK] and a single
// string message for every other status.
const (
	// StatusOK means the call completed; the body holds its results, excluding the final error.
	StatusOK Status = iota + 1
	// StatusError means the plugin returned a non-nil error; the body is its message.
	StatusError
	// StatusPanic means the guest recovered a panic; the body is its message. The instance must be discarded.
	StatusPanic
	// StatusLoad means the plugin source failed to load (parse, type-check or Create signature).
	StatusLoad
	// StatusABI means the guest rejected the request itself (malformed buffer, unknown method, bad state).
	StatusABI
)

// String returns the status name, for diagnostics.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusError:
		return "error"
	case StatusPanic:
		return "panic"
	case StatusLoad:
		return "load"
	case StatusABI:
		return "abi"
	default:
		return "invalid"
	}
}

// Decoding errors. They are wrapped with context by the decoders.
var (
	// ErrTruncated reports input that ends before a complete value.
	ErrTruncated = errors.New("wire: truncated input")
	// ErrTrailing reports bytes left over after the last expected value.
	ErrTrailing = errors.New("wire: trailing bytes")
	// ErrInvalid reports a byte pattern no encoder produces (for example a bool byte other than 0 or 1).
	ErrInvalid = errors.New("wire: invalid encoding")
	// ErrOverflow reports a decoded number that does not fit the destination type.
	ErrOverflow = errors.New("wire: value out of range")
	// ErrUnsupported reports a Go type the encoding cannot represent.
	ErrUnsupported = errors.New("wire: unsupported type")
	// ErrTooLong reports a string longer than the uint32 length prefix can describe.
	ErrTooLong = errors.New("wire: value too long")
)

const (
	wordSize   = 8
	lengthSize = 4
)

// AppendBool appends the encoding of v.
func AppendBool(b []byte, v bool) []byte { //nolint:revive // flag-parameter: v is the encoded value, not a control flag
	if v {
		return append(b, 1)
	}

	return append(b, 0)
}

// AppendInt appends the encoding of a signed integer.
func AppendInt(b []byte, v int64) []byte {
	u := uint64(v) //nolint:gosec // G115: two's complement reinterpretation is the format

	return binary.LittleEndian.AppendUint64(b, u)
}

// AppendUint appends the encoding of an unsigned integer.
func AppendUint(b []byte, v uint64) []byte {
	return binary.LittleEndian.AppendUint64(b, v)
}

// AppendFloat appends the encoding of a float.
func AppendFloat(b []byte, v float64) []byte {
	return binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
}

// AppendString appends the length-prefixed encoding of s. It fails with [ErrTooLong] when len(s) does not fit
// the uint32 prefix. (That guard is not unit-tested: it needs a string over 4 GiB, which no guest could
// receive anyway.)
func AppendString(b []byte, s string) ([]byte, error) {
	if uint64(len(s)) > math.MaxUint32 {
		return b, ErrTooLong
	}

	b = binary.LittleEndian.AppendUint32(b, uint32(len(s))) //nolint:gosec // G115: bounded by the check above

	return append(b, s...), nil
}

// MaxMessage bounds the status messages a result buffer carries, so a hostile plugin's error text cannot
// inflate results without limit.
const MaxMessage = 64 << 10

// AppendMessage appends msg as a string, truncated to at most [MaxMessage] bytes at a UTF-8 boundary.
func AppendMessage(dst []byte, msg string) []byte {
	if len(msg) > MaxMessage {
		n := MaxMessage
		for n > 0 && !utf8.RuneStart(msg[n]) {
			n--
		}

		msg = msg[:n]
	}

	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(msg))) //nolint:gosec // G115: len(msg) <= MaxMessage

	return append(dst, msg...)
}

// Reader decodes values from a buffer, in order. The zero value reads from an empty buffer.
type Reader struct {
	buf []byte
	off int
}

// NewReader returns a Reader over b. The Reader does not copy b; strings it returns are copies.
func NewReader(b []byte) Reader {
	return Reader{buf: b}
}

// Len returns the number of unread bytes.
func (r *Reader) Len() int {
	return len(r.buf) - r.off
}

// End returns [ErrTrailing] if unread bytes remain.
func (r *Reader) End() error {
	if r.Len() != 0 {
		return ErrTrailing
	}

	return nil
}

// Bool decodes a bool.
func (r *Reader) Bool() (bool, error) {
	p, err := r.next(1)
	if err != nil {
		return false, err
	}

	switch p[0] {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, ErrInvalid
	}
}

// Uint64 decodes an unsigned integer.
func (r *Reader) Uint64() (uint64, error) {
	p, err := r.next(wordSize)
	if err != nil {
		return 0, err
	}

	return binary.LittleEndian.Uint64(p), nil
}

// Int64 decodes a signed integer.
func (r *Reader) Int64() (int64, error) {
	u, err := r.Uint64()

	return int64(u), err //nolint:gosec // G115: two's complement reinterpretation is the format
}

// Float64 decodes a float.
func (r *Reader) Float64() (float64, error) {
	u, err := r.Uint64()

	return math.Float64frombits(u), err
}

// Text decodes a length-prefixed string. The result is a copy and does not alias the buffer.
func (r *Reader) Text() (string, error) {
	p, err := r.next(lengthSize)
	if err != nil {
		return "", err
	}

	n := int(binary.LittleEndian.Uint32(p))
	if r.Len() < n {
		return "", ErrTruncated
	}

	start := r.off
	r.off += n

	return string(r.buf[start:r.off]), nil
}

func (r *Reader) next(n int) ([]byte, error) {
	if r.Len() < n {
		return nil, ErrTruncated
	}

	p := r.buf[r.off : r.off+n]
	r.off += n

	return p, nil
}
