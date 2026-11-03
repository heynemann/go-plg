// Package hostile is the hostile-corpus Go plugin for failures at call time: Fail(msg) misbehaves as msg says
// ("panic", "oom", "recurse", "abort", "loop"; "print" writes to stdout first and then panics). Its other
// methods behave like the reference fooer's, so each instance is healthy until provoked.
package hostile

import (
	"context"
	"errors"
	"fmt"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type hostile struct {
	hoard [][]byte
	calls int
}

// Create returns a fresh plugin instance.
func Create() (testcontract.Fooer, error) {
	return &hostile{}, nil
}

func (*hostile) Foo(_ context.Context, name string) (string, error) {
	return testcontract.Greeting(name), nil
}

func (h *hostile) Count(context.Context) (int, error) {
	h.calls++

	return h.calls, nil
}

func (h *hostile) Fail(_ context.Context, msg string) error {
	switch msg {
	case "panic":
		explode("hostile panic")
	case "oom":
		for {
			h.hoard = append(h.hoard, make([]byte, 1<<20))
		}
	case "recurse":
		return fmt.Errorf("depth %d", recurse(0))
	case "abort":
		select {} // every goroutine asleep: the Go runtime aborts
	case "loop":
		for {
		}
	case "print":
		fmt.Println("hostile says hello on stdout")
		println("and through the println builtin")
		explode("after printing")
	}

	return errors.New(msg)
}

// explode panics two interpreted frames below Fail, so the guest traceback has several positions.
func explode(msg string) {
	deeper(msg)
}

func deeper(msg string) {
	panic(msg)
}

// recurse never returns.
func recurse(n int) int {
	return recurse(n+1) + 1
}

func (*hostile) Boil(_ context.Context, c testcontract.Celsius) (bool, float64, error) {
	return c >= 100, float64(c)*9/5 + 32, nil
}

func (*hostile) Scalars(
	_ context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
	u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string,
) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error) {
	return b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s, nil
}

func (*hostile) Spin(_ context.Context, n int) (int, error) {
	count := 0
	for count < n {
		count++
	}

	return count, nil
}
