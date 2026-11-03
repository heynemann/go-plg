// Package hog is a hostile test plugin: Spin(n) allocates and keeps n more MiB, and Fail("loop") never returns.
// Its other methods behave like the reference fooer's, so it is healthy until provoked.
package hog

import (
	"context"
	"errors"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type hog struct {
	hoard [][]byte
	calls int
}

// Create returns a fresh plugin instance.
func Create() (testcontract.Fooer, error) {
	return &hog{}, nil
}

func (*hog) Foo(_ context.Context, name string) (string, error) {
	return testcontract.Greeting(name), nil
}

func (h *hog) Count(context.Context) (int, error) {
	h.calls++

	return h.calls, nil
}

func (*hog) Fail(_ context.Context, msg string) error {
	if msg == "loop" {
		for {
		}
	}

	return errors.New(msg)
}

func (*hog) Boil(_ context.Context, c testcontract.Celsius) (bool, float64, error) {
	return c >= 100, float64(c)*9/5 + 32, nil
}

func (*hog) Scalars(
	_ context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
	u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string,
) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error) {
	return b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s, nil
}

// Spin allocates n MiB more, touching every page so the memory is really committed, and returns how many MiB
// the instance holds.
func (h *hog) Spin(_ context.Context, n int) (int, error) {
	block := make([]byte, n<<20)
	for i := 0; i < len(block); i += 4096 {
		block[i] = 1
	}

	h.hoard = append(h.hoard, block)

	total := 0
	for _, b := range h.hoard {
		total += len(b) >> 20
	}

	return total, nil
}
