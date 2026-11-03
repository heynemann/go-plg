// Package fooer is the reference Go test plugin: it implements testcontract.Fooer.
package fooer

import (
	"context"
	"errors"
	"strings"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type fooer struct {
	calls int
}

// Create returns a fresh plugin instance.
func Create() (testcontract.Fooer, error) {
	return &fooer{}, nil
}

func (*fooer) Foo(_ context.Context, name string) (string, error) {
	return testcontract.Greeting(name), nil
}

func (f *fooer) Count(context.Context) (int, error) {
	f.calls++

	return f.calls, nil
}

func (*fooer) Fail(_ context.Context, msg string) error {
	if msg == "" {
		return nil
	}

	if msg == "panic" {
		panic("plugin panicked on purpose")
	}

	return errors.New(msg)
}

func (*fooer) Boil(_ context.Context, c testcontract.Celsius) (bool, float64, error) {
	return c >= 100, float64(c)*9/5 + 32, nil
}

func (*fooer) Scalars(
	_ context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
	u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string,
) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error) {
	return !b, i + 1, i8 + 1, i16 + 1, i32 + 1, i64 + 1, u + 1, u8 + 1, u16 + 1, u32 + 1, u64 + 1,
		f32 * 2, f64 * 2, strings.ToUpper(s), nil
}
