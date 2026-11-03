package testcontract

import (
	"context"
	"reflect"

	"github.com/heynemann/go-plg"
)

// This file is written by hand in the shape `plg gen Fooer` generates: an adapter whose W fields plg fills
// with calls into a plugin, a compile-time check that it still implements Fooer, and the contract
// registration. The guest never sees it: Source embeds contract.go only.

// plgFooer implements Fooer by forwarding each method to its W field. A field's type is its method's type.
type plgFooer struct {
	WFoo     func(context.Context, string) (string, error)
	WCount   func(context.Context) (int, error)
	WFail    func(context.Context, string) error
	WBoil    func(context.Context, Celsius) (bool, float64, error)
	WScalars func(
		context.Context, bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64, string,
	) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error)
	WSpin func(context.Context, int) (int, error)
}

var _ Fooer = (*plgFooer)(nil)

func (a *plgFooer) Foo(ctx context.Context, name string) (string, error) {
	return a.WFoo(ctx, name)
}

func (a *plgFooer) Count(ctx context.Context) (int, error) {
	return a.WCount(ctx)
}

func (a *plgFooer) Fail(ctx context.Context, msg string) error {
	return a.WFail(ctx, msg)
}

//nolint:gocritic // unnamedResult: the signature is the contract's Boil, verbatim
func (a *plgFooer) Boil(ctx context.Context, c Celsius) (bool, float64, error) {
	return a.WBoil(ctx, c)
}

//nolint:revive,gocritic // argument-limit, function-result-limit, tooManyResults: the contract's Scalars, verbatim
func (a *plgFooer) Scalars(
	ctx context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
	u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string,
) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error) {
	return a.WScalars(ctx, b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s)
}

func (a *plgFooer) Spin(ctx context.Context, n int) (int, error) {
	return a.WSpin(ctx, n)
}

//nolint:gochecknoinits // contracts register from init, as the code plg gen generates does
func init() {
	plg.MustRegisterContract(plg.Contract{
		Interface: reflect.TypeFor[Fooer](),
		Adapter:   reflect.TypeFor[plgFooer](),
		Source:    Source,
	})
}
