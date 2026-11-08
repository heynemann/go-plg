// Package secrets is a test plugin reading the secrets its plugin.yaml declares (api_key, create, broken) with
// plugin.Secret. Foo(name) returns the secret name; Foo("@create") what Create read of the secret create (its value,
// or "error: " and why). Fail(cmd) leaks a secret as cmd says:
//
//	print <name>        writes "token <value>" to stdout (a Go plugin's stderr holds its panics' tracebacks only)
//	error <name>        returns the error "rejected <value>"
//	panic <name>        panics with "leaked <value>"
//	fetch <name> <url>  GETs <url>/<value>
package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/plugin"
)

type secrets struct {
	created string // what Create read of the secret create
}

// Create reads the secret create.
func Create() (testcontract.Fooer, error) {
	value, err := plugin.Secret(context.Background(), "create")
	if err != nil {
		value = "error: " + err.Error()
	}

	return &secrets{created: value}, nil
}

func (s *secrets) Foo(ctx context.Context, name string) (string, error) {
	if name == "@create" {
		return s.created, nil
	}

	return plugin.Secret(ctx, name)
}

func (*secrets) Count(context.Context) (int, error) { return 0, nil }

func (*secrets) Fail(ctx context.Context, cmd string) error {
	args := strings.Fields(cmd)
	if len(args) < 2 {
		return errors.New("bad command")
	}

	value, err := plugin.Secret(ctx, args[1])
	if err != nil {
		return err
	}

	switch args[0] {
	case "print":
		fmt.Println("token " + value)

		return nil
	case "error":
		return errors.New("rejected " + value)
	case "panic":
		panic("leaked " + value)
	case "fetch":
		resp, err := http.Get(args[2] + "/" + value)
		if err != nil {
			return err
		}

		return resp.Body.Close()
	default:
		return errors.New("unknown command " + args[0])
	}
}

func (*secrets) Boil(_ context.Context, c testcontract.Celsius) (bool, float64, error) {
	return c >= 100, float64(c)*9/5 + 32, nil
}

func (*secrets) Scalars(
	_ context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
	u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string,
) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error) {
	return b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s, nil
}

func (*secrets) Spin(_ context.Context, n int) (int, error) { return n, nil }
