package wire

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"time"
)

// The functions below encode and decode one value of a known Go type. The reflective [Codec] is built from
// them, and so is the code `plg gen` generates for a contract's types (through the public plgimpl package):
// both produce the same bytes and fail with the same errors.

// AppendInt53 appends num, failing with [ErrPrecision] beyond ±2^53.
func AppendInt53(b []byte, num int64) ([]byte, error) {
	if num > MaxSafeInt || num < -MaxSafeInt {
		return b, imprecise(num)
	}

	return AppendInt(b, num), nil
}

// AppendUint53 appends num, failing with [ErrPrecision] beyond 2^53.
func AppendUint53(b []byte, num uint64) ([]byte, error) {
	if num > MaxSafeInt {
		return b, imprecise(num)
	}

	return AppendUint(b, num), nil
}

// AppendBytes appends data as a byte slice: nil stays distinct from empty.
func AppendBytes(dst, data []byte) ([]byte, error) {
	if data == nil {
		return AppendNil(dst), nil
	}

	dst, err := appendLength(dst, len(data))
	if err != nil {
		return dst, err
	}

	return append(dst, data...), nil
}

// AppendTime appends the instant t as Unix seconds and nanoseconds, failing with [ErrPrecision] when the
// seconds are beyond ±2^53.
func AppendTime(b []byte, t time.Time) ([]byte, error) {
	sec := t.Unix()
	if sec > MaxSafeInt || sec < -MaxSafeInt {
		return b, imprecise(sec)
	}

	return AppendInt(AppendInt(b, sec), int64(t.Nanosecond())), nil
}

// AppendNil appends the length prefix of a nil slice or map.
func AppendNil(b []byte) []byte {
	return binary.LittleEndian.AppendUint32(b, nilLength)
}

// AppendLen appends the length of a non-nil slice or map nested depth levels deep, failing with [ErrTooDeep]
// at [MaxDepth] levels.
func AppendLen(b []byte, n, depth int) ([]byte, error) {
	err := Deeper(depth)
	if err != nil {
		return b, err
	}

	return appendLength(b, n)
}

// Deeper fails with [ErrTooDeep] when a slice, map, pointer or struct is depth levels deep, depth reaching
// [MaxDepth].
func Deeper(depth int) error {
	if depth >= MaxDepth {
		return fmt.Errorf("%w: more than %d levels", ErrTooDeep, MaxDepth)
	}

	return nil
}

// Int53 decodes a signed integer, failing with [ErrPrecision] beyond ±2^53.
func (r *Reader) Int53() (int64, error) {
	num, err := r.Int64()

	switch {
	case err != nil:
		return 0, err
	case num > MaxSafeInt || num < -MaxSafeInt:
		return 0, imprecise(num)
	}

	return num, nil
}

// Uint53 decodes an unsigned integer, failing with [ErrPrecision] beyond 2^53.
func (r *Reader) Uint53() (uint64, error) {
	num, err := r.Uint64()

	switch {
	case err != nil:
		return 0, err
	case num > MaxSafeInt:
		return 0, imprecise(num)
	}

	return num, nil
}

// Bytes decodes a byte slice: nil for a nil one, otherwise a copy (non-nil even when empty) that does not
// alias the buffer.
func (r *Reader) Bytes() ([]byte, error) {
	n, err := r.length()
	if err != nil || n < 0 {
		return nil, err
	}

	p, err := r.next(n)
	if err != nil {
		return nil, err
	}

	data := make([]byte, n)
	copy(data, p)

	return data, nil
}

const nanosPerSecond = int64(time.Second)

// Time decodes what [AppendTime] wrote, as a UTC time.
func (r *Reader) Time() (time.Time, error) {
	sec, err := r.Int64()
	if err != nil {
		return time.Time{}, err
	}

	nsec, err := r.Int64()

	switch {
	case err != nil:
		return time.Time{}, err
	case sec > MaxSafeInt || sec < -MaxSafeInt:
		return time.Time{}, imprecise(sec)
	case nsec < 0 || nsec >= nanosPerSecond:
		return time.Time{}, fmt.Errorf("%w: %d nanoseconds", ErrInvalid, nsec)
	}

	return time.Unix(sec, nsec).UTC(), nil
}

// SliceLen decodes the element count of a slice nested depth levels deep whose elements encode in at least
// minSize bytes (minSize > 0) and take size bytes in memory: -1 for a nil slice. A count the remaining input
// cannot hold fails with [ErrTruncated], elements past the reader's memory bound with [ErrTooLarge].
func (r *Reader) SliceLen(depth, minSize, size int) (int, error) {
	count, err := r.length()
	if err != nil || count < 0 {
		return count, err
	}

	err = Deeper(depth)
	if err != nil {
		return 0, err
	}

	if count > r.Len()/minSize {
		return 0, fmt.Errorf("%w: %d elements", ErrTruncated, count)
	}

	return count, r.claim(count, size)
}

// MapLen decodes the entry count of a map nested depth levels deep whose values encode in at least minSize
// bytes and whose key and value take size bytes in memory: -1 for a nil map. A count the remaining input cannot
// hold fails with [ErrTruncated], entries past the reader's memory bound with [ErrTooLarge].
func (r *Reader) MapLen(depth, minSize, size int) (int, error) {
	count, err := r.length()
	if err != nil || count < 0 {
		return count, err
	}

	err = Deeper(depth)
	if err != nil {
		return 0, err
	}

	if count > r.Len()/(lengthSize+minSize) {
		return 0, fmt.Errorf("%w: %d entries", ErrTruncated, count)
	}

	return count, r.claim(count, mapEntrySize(size))
}

// mapEntrySize is the memory a map whose key and value take size bytes may take per entry: a slot and its
// control byte, at Go's maximum load of 7/8 in a table whose size is rounded up to a power of two.
func mapEntrySize(size int) int {
	const slots, entries = 16, 7 // 2 * 8/7

	return ((size+1)*slots + entries - 1) / entries
}

// Present decodes the presence byte of a pointer nested depth levels deep to a target of size bytes. A target
// past the reader's memory bound fails with [ErrTooLarge].
func (r *Reader) Present(depth, size int) (bool, error) {
	present, err := r.Bool()
	if err != nil || !present {
		return false, err
	}

	err = Deeper(depth)
	if err != nil {
		return false, err
	}

	return true, r.claim(1, size)
}

// Overflow is the error of a decoded number num that does not fit the Go type typeName (as reflect names it).
func Overflow[T int64 | uint64 | float64](num T, typeName string) error {
	return fmt.Errorf("%w: %v does not fit %s", ErrOverflow, num, typeName)
}

// OverflowsFloat32 reports whether num is out of float32's range, as reflect's Value.OverflowFloat does.
func OverflowsFloat32(num float64) bool {
	if num < 0 {
		num = -num
	}

	return math.MaxFloat32 < num && num <= math.MaxFloat64
}

// DuplicateKeys is the error of a map encoding that repeats a key.
func DuplicateKeys() error {
	return fmt.Errorf("%w: duplicate map keys", ErrInvalid)
}

// AtField locates err inside the struct field goName.
func AtField(err error, goName string) error { return within(err, "."+goName) }

// AtIndex locates err at index i of a slice.
func AtIndex(err error, i int) error { return within(err, "["+strconv.Itoa(i)+"]") }

// AtKey locates err at key k of a map.
func AtKey(err error, k string) error { return within(err, "["+strconv.Quote(k)+"]") }
