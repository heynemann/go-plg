// Package wire is the binary encoding shared by the plg host and its guests.
//
// Values are little-endian and length-prefixed, and carry no type information: both sides know the signature.
// A call's arguments (and a result's values) are the concatenation of their encodings, without a count.
//
//   - bool: one byte, 0 or 1.
//   - Integers of every kind: eight bytes, two's complement for signed kinds. Only |n| <= 2^53 ([MaxSafeInt])
//     crosses: Lua and JavaScript numbers represent no more integers exactly, so larger magnitudes fail to
//     encode (and to decode) with [ErrPrecision] instead of losing precision in another guest language.
//   - Floats of every kind: the eight-byte IEEE 754 binary64 bit pattern.
//   - string: a uint32 byte length, then the bytes.
//   - []byte (any slice of a uint8 kind): a uint32 length, then the bytes; the length 0xFFFFFFFF is a nil
//     slice, so nil and empty stay distinct.
//   - Other slices: a uint32 element count (0xFFFFFFFF for nil), then the elements.
//   - Maps (string keys only): a uint32 entry count (0xFFFFFFFF for nil), then per entry the key as a string
//     and the value. Entries are in map iteration order; a decoder rejects duplicate keys.
//   - Pointers: a presence byte (0 for nil), then the pointee when present.
//   - Structs: the encoded fields in declaration order, without names. A field tagged `json:"-"` is not
//     encoded (and decodes as its zero value). Names live in the [Schema], for guests without Go types.
//   - time.Time: Unix seconds then nanoseconds (0 to 999999999), two integers; the instant crosses, the
//     location and monotonic reading do not, and it decodes in UTC.
//   - error: a presence byte followed, when present, by its message as a string.
//
// Nesting deeper than [MaxDepth] fails, so a hostile encoding cannot exhaust the decoder's stack and a pointer
// cycle cannot hang its encoder. A length prefix never claims more elements than the remaining input can hold,
// and the memory of the slices, maps and pointers a decoder makes is bounded by the input's size
// ([AllocationRatio], [AllocationAllowance]), so a hostile encoding cannot make a decoder allocate far more
// than its input justifies, even for elements whose `json:"-"` fields take much more memory than they encode in.
//
// Every guest result buffer starts with a [Status] byte.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
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
	// StatusConvert means the plugin's results cannot be encoded (for example an integer beyond ±2^53); the
	// body is the message, naming the result and field. The instance stays usable.
	StatusConvert
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
	case StatusConvert:
		return "convert"
	default:
		return "invalid"
	}
}

// Encoding and decoding errors. They are wrapped with context by the codecs.
var (
	// ErrTruncated reports input that ends before a complete value, or a length prefix claiming more elements
	// than the remaining input can hold.
	ErrTruncated = errors.New("wire: truncated input")
	// ErrTrailing reports bytes left over after the last expected value.
	ErrTrailing = errors.New("wire: trailing bytes")
	// ErrInvalid reports a byte pattern no encoder produces (for example a bool byte other than 0 or 1).
	ErrInvalid = errors.New("wire: invalid encoding")
	// ErrOverflow reports a decoded number that does not fit the destination type.
	ErrOverflow = errors.New("wire: value out of range")
	// ErrPrecision reports an integer beyond ±2^53, which Lua and JavaScript numbers cannot represent exactly.
	ErrPrecision = errors.New("wire: integer beyond ±2^53")
	// ErrUnsupported reports a Go type the encoding cannot represent.
	ErrUnsupported = errors.New("wire: unsupported type")
	// ErrTooLong reports a string or collection longer than the uint32 length prefix can describe.
	ErrTooLong = errors.New("wire: value too long")
	// ErrTooDeep reports a value nested deeper than [MaxDepth] (on encoding, usually a pointer cycle).
	ErrTooDeep = errors.New("wire: value nested too deeply")
	// ErrTooLarge reports an encoding whose decoded values would take far more memory than its size justifies:
	// length prefixes and pointers claiming more than [AllocationRatio] times the input plus
	// [AllocationAllowance] bytes of elements.
	ErrTooLarge = errors.New("wire: decoded value too large for its encoding")
)

const (
	// MaxSafeInt is the largest integer magnitude that crosses: 2^53, the integers every float64 holds exactly.
	MaxSafeInt = 1 << 53
	// MaxDepth bounds the nesting of slices, maps, pointers and structs inside one value.
	MaxDepth = 10000
	// AllocationRatio bounds, with AllocationAllowance, the memory decoding an input of n bytes may claim for the
	// elements of slices, the entries of maps and the targets of pointers: AllocationRatio*n +
	// AllocationAllowance bytes, measured by their Go types' sizes (a map entry by the most a map table takes
	// per entry). Past that, decoding fails with [ErrTooLarge] before allocating. Every element claimed encodes
	// in bytes of its own, and no type the encoding supports claims more than 16 times the size of its smallest
	// encoding (a nil error: one byte, a 16-byte interface), so any encoding of types without `json:"-"` fields
	// stays within half the ratio; the allowance leaves room for values whose skipped fields take memory their
	// encoding does not account for. Strings and byte slices copy their own bytes, and per-collection overhead
	// (a map's header, a small map's first group) adds a small constant factor at most.
	AllocationRatio = 32
	// AllocationAllowance is the memory any input may claim on top of AllocationRatio times its size.
	AllocationAllowance = 4 << 20
)

const (
	wordSize   = 8
	lengthSize = 4
	nilLength  = math.MaxUint32 // the length prefix of a nil slice or map
	// maxRoomBase is the largest input size whose room an int holds (on 32-bit platforms, inputs past 64 MiB).
	maxRoomBase = (math.MaxInt - AllocationAllowance) / AllocationRatio
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

// appendLength appends the length prefix of a non-nil slice or map of n elements. 0xFFFFFFFF is reserved for
// nil, so n must be smaller. (Like AppendString's, that guard needs over 4 Gi elements to test.)
func appendLength(b []byte, n int) ([]byte, error) {
	if uint64(n) >= nilLength { //nolint:gosec // G115: n is a length, never negative
		return b, ErrTooLong
	}

	return binary.LittleEndian.AppendUint32(b, uint32(n)), nil //nolint:gosec // G115: bounded by the check above
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
	buf  []byte
	off  int
	room int // the bytes of elements, entries and pointer targets the decoders may still make
}

// NewReader returns a Reader over b. The Reader does not copy b; strings it returns are copies. Decoding from it
// claims at most [AllocationRatio]*len(b)+[AllocationAllowance] bytes of elements, entries and pointer targets.
func NewReader(b []byte) Reader {
	return Reader{buf: b, room: AllocationAllowance + AllocationRatio*min(len(b), maxRoomBase)}
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

// length decodes the length prefix of a slice or map; a nil one has length -1.
func (r *Reader) length() (int, error) {
	p, err := r.next(lengthSize)
	if err != nil {
		return 0, err
	}

	prefix := binary.LittleEndian.Uint32(p)
	if prefix == nilLength {
		return -1, nil
	}

	return int(prefix), nil
}

func (r *Reader) next(n int) ([]byte, error) {
	if r.Len() < n {
		return nil, ErrTruncated
	}

	p := r.buf[r.off : r.off+n]
	r.off += n

	return p, nil
}

// claim reserves the memory of count values of size bytes each, failing with [ErrTooLarge] past the reader's
// room.
func (r *Reader) claim(count, size int) error {
	if size > 0 && count > r.room/size {
		return fmt.Errorf("%w: %d × %d bytes", ErrTooLarge, count, size)
	}

	r.room -= count * size

	return nil
}
