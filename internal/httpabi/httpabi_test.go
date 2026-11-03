package httpabi_test

import (
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/httpabi"
	"github.com/heynemann/go-plg/internal/wire"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func encodeRequest(method, url string, header http.Header, body string) []byte {
	return append(httpabi.AppendRequestHead(nil, method, url, header), body...)
}

// appendStrings appends wire strings.
func appendStrings(tb testing.TB, strs ...string) []byte {
	tb.Helper()

	var out []byte

	for _, s := range strs {
		var err error

		out, err = wire.AppendString(out, s)
		require.NoError(tb, err)
	}

	return out
}

func TestRequestRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		header http.Header
		name   string
		method string
		url    string
		body   string
	}{
		{nil, "bare GET", "GET", "http://a.example/", ""},
		{
			http.Header{"Accept": {"a", "b"}, "X-Empty": {""}, "lower-case": {"kept as sent"}},
			"repeated, empty and non-canonical headers", "POST", "https://a.example/p?q=1", "body \x00 bytes",
		},
		{http.Header{}, "empty everything", "", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prefix := []byte("existing")
			enc := append(httpabi.AppendRequestHead(prefix, tc.method, tc.url, tc.header), tc.body...)
			require.Equal(t, prefix, enc[:len(prefix)], "appends")

			got, err := httpabi.DecodeRequest(enc[len(prefix):])
			require.NoError(t, err)
			assert.Equal(t, tc.method, got.Method)
			assert.Equal(t, tc.url, got.URL)
			assert.Len(t, got.Header, len(tc.header))

			for name, values := range tc.header {
				assert.Equal(t, values, got.Header[name], name)
			}

			assert.Equal(t, tc.body, string(got.Body), "the body is every remaining byte")
		})
	}
}

func TestResponseRoundTrip(t *testing.T) {
	t.Parallel()

	enc := append(httpabi.AppendResponseHead(nil, http.StatusTeapot, http.Header{"X-Reply": {"1", "2"}}),
		"short and stout"...)

	got, err := httpabi.DecodeResponse(enc)
	require.NoError(t, err)
	assert.Equal(t, http.StatusTeapot, got.Status)
	assert.Equal(t, http.Header{"X-Reply": {"1", "2"}}, got.Header)
	assert.Equal(t, "short and stout", string(got.Body))

	got, err = httpabi.DecodeResponse(httpabi.AppendResponseHead(nil, -1, nil))
	require.NoError(t, err)
	assert.Zero(t, got.Status, "a negative status is clamped to zero")
	assert.Empty(t, got.Body)
}

func TestFailureDecodesToHostError(t *testing.T) {
	t.Parallel()

	_, err := httpabi.DecodeResponse(httpabi.AppendFailure(nil, "host not allowed"))

	var hostErr *httpabi.HostError

	require.ErrorAs(t, err, &hostErr)
	assert.EqualError(t, err, "host not allowed", "the host's message, verbatim")
}

func TestDecodeRejectsMalformedMessages(t *testing.T) {
	t.Parallel()

	okHead := httpabi.AppendResponseHead(nil, http.StatusOK, http.Header{"A": {"b"}})
	reqHead := encodeRequest("GET", "u", http.Header{"A": {"b"}}, "")

	requests := map[string][]byte{
		"empty":                {},
		"truncated method":     {9, 0, 0, 0, 'G'},
		"truncated url":        reqHead[:9],
		"missing header count": reqHead[:4+3+4+1],
		"header count too big": wire.AppendUint(appendStrings(t, "GET", "u"), 1<<40),
		"truncated name":       append(wire.AppendUint(appendStrings(t, "GET", "u"), 1), 100, 0, 0, 0, 1, 2, 3, 4),
		"truncated value":      reqHead[:len(reqHead)-2],
	}

	for name, enc := range requests {
		_, err := httpabi.DecodeRequest(enc)
		require.ErrorIs(t, err, httpabi.ErrMalformed, "request: %s", name)
	}

	responses := map[string][]byte{
		"empty":                 {},
		"zero status":           {0},
		"panic status":          {byte(wire.StatusPanic)},
		"truncated status code": okHead[:4],
		"status code over 999":  wire.AppendUint([]byte{byte(wire.StatusOK)}, 1000),
		"truncated header":      okHead[:len(okHead)-1],
		"truncated failure":     {byte(wire.StatusError), 5, 0, 0, 0},
		"failure with trailing": append(httpabi.AppendFailure(nil, "x"), 0),
	}

	for name, enc := range responses {
		_, err := httpabi.DecodeResponse(enc)
		require.ErrorIs(t, err, httpabi.ErrMalformed, "response: %s", name)
	}
}

func TestAppendBody(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("abcdefgh", 10_000) // 80 kB: several reads

	tests := []struct {
		reader  io.Reader
		wantErr error
		name    string
		want    string
		limit   int
	}{
		{strings.NewReader(""), nil, "empty", "", 0},
		{strings.NewReader(big), nil, "exactly the limit", big, len(big)},
		{iotest.OneByteReader(strings.NewReader("slow")), nil, "short reads", "slow", 10},
		{iotest.DataErrReader(strings.NewReader("eof with data")), nil, "EOF with the last data", "eof with data", 13},
		{strings.NewReader(big), httpabi.ErrBodyTooLarge, "one byte over", "", len(big) - 1},
		{strings.NewReader("x"), httpabi.ErrBodyTooLarge, "zero limit", "", 0},
		{iotest.ErrReader(errBoom), errBoom, "read error", "", 10},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := httpabi.AppendBody([]byte("head:"), tc.reader, tc.limit)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.LessOrEqual(t, len(got), len("head:")+tc.limit+32<<10, "reads stop right after the limit")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, "head:"+tc.want, string(got))
		})
	}
}

var errBoom = errors.New("boom")

func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte{})
	f.Add(encodeRequest("GET", "http://a/", http.Header{"A": {"b", "c"}}, "body"))
	f.Add(wire.AppendUint(appendStrings(f, "GET", "u"), math.MaxUint64))

	f.Fuzz(func(t *testing.T, enc []byte) {
		got, err := httpabi.DecodeRequest(enc)
		if err != nil {
			require.ErrorIs(t, err, httpabi.ErrMalformed)

			return
		}

		again := append(httpabi.AppendRequestHead(nil, got.Method, got.URL, got.Header), got.Body...)
		redecoded, err := httpabi.DecodeRequest(again)
		require.NoError(t, err, "what decodes re-encodes")
		assert.Equal(t, got, redecoded)
	})
}

func FuzzDecodeResponse(f *testing.F) {
	f.Add(append(httpabi.AppendResponseHead(nil, http.StatusOK, http.Header{"A": {"b"}}), "body"...))
	f.Add(httpabi.AppendFailure(nil, "refused"))
	f.Add([]byte{byte(wire.StatusOK)})

	f.Fuzz(func(t *testing.T, enc []byte) {
		got, err := httpabi.DecodeResponse(enc)

		var hostErr *httpabi.HostError

		switch {
		case errors.As(err, &hostErr):
			assert.Equal(t, httpabi.AppendFailure(nil, hostErr.Message), enc, "failures are canonical")
		case err != nil:
			require.ErrorIs(t, err, httpabi.ErrMalformed)
		default:
			assert.LessOrEqual(t, got.Status, 999)

			again := httpabi.AppendResponseHead(nil, got.Status, got.Header)

			redecoded, err := httpabi.DecodeResponse(append(again, got.Body...))
			require.NoError(t, err)
			assert.Equal(t, got, redecoded)
		}
	})
}
