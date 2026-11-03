// A test plugin declared as package main: the dispatcher imports it under an alias, so its name is irrelevant.
package main

import (
	"context"
	"fmt"

	contract "github.com/heynemann/go-plg/internal/testcontract"
)

type fooer struct{}

func Create() (contract.Fooer, error) { return fooer{}, nil }

func (fooer) Foo(_ context.Context, name string) (string, error) {
	return fmt.Sprintf("main says %s", contract.Greeting(name)), nil
}

func (fooer) Count(context.Context) (int, error)                 { return 0, nil }
func (fooer) Fail(context.Context, string) error                 { return nil }
func (fooer) Boil(context.Context, contract.Celsius) (bool, float64, error) { return false, 0, nil }

func (fooer) Scalars(
	_ context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
	u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string,
) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error) {
	return b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s, nil
}

func (fooer) Spin(_ context.Context, n int) (int, error) { return n, nil }

func main() {}
