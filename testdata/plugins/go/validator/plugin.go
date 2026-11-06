// Package validator is the Go test plugin that validates itself (plg.WithValidation): it implements
// testcontract.Fooer with a Create taking testcontract.Host, and its Validate does what the input's Mode says:
//
//	valid   no issue
//	warn    a warning and an info issue
//	error   an error issue and a warning
//	fail    returns an error
//	panic   panics
//	loop    never returns
//	host    an info issue "create=<Host.Value in Create> validate=<Host.Value in Validate>"
//	http    an info issue "<status code> <body>" of a GET of the input's URL
//
// Validate also marks its instance validated: Foo("validated") reports whether this instance was.
package validator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/plugin"
)

type validator struct {
	host      testcontract.Host
	origin    string
	calls     int
	validated bool
}

// Create keeps the host and what Host.Value returns while Create runs.
func Create(host testcontract.Host) (testcontract.Fooer, error) {
	origin, err := host.Value(context.Background())
	if err != nil {
		return nil, err
	}

	return &validator{host: host, origin: origin}, nil
}

func (v *validator) Validate(ctx context.Context, in testcontract.Check) ([]plugin.Issue, error) {
	v.validated = true

	switch in.Mode {
	case "valid":
		return nil, nil
	case "warn":
		return []plugin.Issue{
			{Severity: plugin.Warning, Code: "validator.slow", Message: "slow", Field: "speed"},
			{Severity: plugin.Info, Code: "validator.note", Message: "noted"},
		}, nil
	case "error":
		return []plugin.Issue{
			{Severity: plugin.Error, Code: "validator.broken", Message: "broken", Field: "max"},
			{Severity: plugin.Warning, Code: "validator.slow", Message: "slow"},
		}, nil
	case "fail":
		return nil, errors.New("validation broke")
	case "panic":
		panic("Validate panicked on purpose")
	case "loop":
		for i := 0; ; i++ {
			_ = i
		}
	case "host":
		value, err := v.host.Value(ctx)
		if err != nil {
			return nil, err
		}

		return []plugin.Issue{{
			Severity: plugin.Info, Code: "validator.host", Message: "create=" + v.origin + " validate=" + value,
		}}, nil
	case "http":
		resp, err := http.Get(in.URL)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		return []plugin.Issue{{
			Severity: plugin.Info, Code: "validator.http", Message: strconv.Itoa(resp.StatusCode) + " " + string(body),
		}}, nil
	}

	return nil, fmt.Errorf("unknown mode %q", in.Mode)
}

func (v *validator) Foo(_ context.Context, name string) (string, error) {
	switch name {
	case "origin":
		return v.origin, nil
	case "validated":
		return strconv.FormatBool(v.validated), nil
	}

	return testcontract.Greeting(name), nil
}

func (v *validator) Count(context.Context) (int, error) {
	v.calls++

	return v.calls, nil
}

func (*validator) Fail(_ context.Context, msg string) error {
	if msg == "" {
		return nil
	}

	return errors.New(msg)
}

func (*validator) Boil(_ context.Context, c testcontract.Celsius) (bool, float64, error) {
	return c >= 100, float64(c)*9/5 + 32, nil
}

func (*validator) Scalars(
	_ context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
	u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string,
) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error) {
	return b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s, nil
}

func (*validator) Spin(_ context.Context, n int) (int, error) {
	return n, nil
}
