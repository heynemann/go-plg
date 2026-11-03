// Package single is a one-file Go plugin of an FS storage.
package single

import (
	"context"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type single struct{}

// Create returns a fresh plugin instance.
func Create() (testcontract.Fooer, error) {
	return single{}, nil
}

func (single) Foo(_ context.Context, name string) (string, error) {
	return testcontract.Greeting(name) + " (single)", nil
}

func (single) Count(context.Context) (int, error) { return 0, nil }

func (single) Fail(context.Context, string) error { return nil }

func (single) Boil(context.Context, testcontract.Celsius) (bool, float64, error) { return false, 0, nil }

func (single) Scalars(
	_ context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
	u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string,
) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error) {
	return b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s, nil
}

func (single) Spin(_ context.Context, n int) (int, error) { return n, nil }
