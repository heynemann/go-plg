// Package fetcher is a test plugin making HTTP requests with the standard net/http client. Foo takes a command:
//
//	GET <url>             one GET
//	POST <url> <n>        one POST with an n-byte body
//	REPEAT <n> <url>      n GETs in a row
//	HOST <url> <host>     one GET with the Host header overridden
//	CREDS <url>           one GET with the six headers carrying credentials set (Authorization, Cookie, ...)
//
// and returns "<status code>|<X-Reply header>|<body>" of the last response; CREDS adds "|<how many of the six
// headers the request still holds once answered>".
package fetcher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type fetcher struct{}

// Create returns a fresh plugin instance.
func Create() (testcontract.Fooer, error) {
	return fetcher{}, nil
}

func (fetcher) Foo(_ context.Context, cmd string) (string, error) {
	args := strings.Fields(cmd)
	if len(args) < 2 {
		return "", errors.New("bad command")
	}

	switch args[0] {
	case "GET":
		return get(args[1], "")
	case "HOST":
		return get(args[1], args[2])
	case "CREDS":
		return creds(args[1])
	case "POST":
		n, _ := strconv.Atoi(args[2])

		resp, err := http.Post(args[1], "text/plain", strings.NewReader(strings.Repeat("x", n)))
		if err != nil {
			return "", err
		}

		return describe(resp)
	case "REPEAT":
		n, _ := strconv.Atoi(args[1])

		out := ""
		for i := 0; i < n; i++ {
			got, err := get(args[2], "")
			if err != nil {
				return "", err
			}

			out = got
		}

		return out, nil
	default:
		return "", errors.New("unknown command " + args[0])
	}
}

func get(url, host string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("X-Test", "fetcher")

	if host != "" {
		req.Host = host
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}

	return describe(resp)
}

func creds(url string) (string, error) {
	names := []string{
		"Authorization", "Cookie", "Cookie2", "Proxy-Authenticate", "Proxy-Authorization", "Www-Authenticate",
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("X-Test", "fetcher")

	for _, name := range names {
		req.Header.Set(name, "secret")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}

	got, err := describe(resp)
	if err != nil {
		return "", err
	}

	held := 0

	for _, name := range names {
		if req.Header.Get(name) == "secret" {
			held++
		}
	}

	return got + "|" + strconv.Itoa(held), nil
}

func describe(resp *http.Response) (string, error) {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return strconv.Itoa(resp.StatusCode) + "|" + resp.Header.Get("X-Reply") + "|" + string(body), nil
}

func (fetcher) Count(context.Context) (int, error) { return 0, nil }

func (fetcher) Fail(context.Context, string) error { return nil }

func (fetcher) Boil(context.Context, testcontract.Celsius) (bool, float64, error) { return false, 0, nil }

func (fetcher) Scalars(
	_ context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
	u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string,
) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error) {
	return b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s, nil
}

func (fetcher) Spin(context.Context, int) (int, error) { return 0, nil }
