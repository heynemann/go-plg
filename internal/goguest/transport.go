package goguest

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/heynemann/go-plg/internal/httpabi"
)

// maxRequest bounds an encoded request: the plg.http_roundtrip import takes its length as a uint32. The host's
// policy refuses far smaller requests.
const maxRequest = math.MaxInt32

// ErrNoURL reports a request without a URL.
var ErrNoURL = errors.New("goguest: request without a URL")

// Transport is the http.RoundTripper the guest installs as http.DefaultTransport, so plugins use the standard
// net/http client: it encodes each request with [httpabi], hands it to the host through Exchange and decodes
// the host's answer. The host applies the plugin's HTTP policy and makes the request; a refusal is the
// round trip's error. Redirects, cookies and client timeouts stay the http.Client's job, inside the guest.
type Transport struct {
	// Exchange sends an encoded request to the host and returns the encoded response, in a buffer the
	// returned response's body may keep.
	Exchange func(req []byte) []byte
}

// RoundTrip sends req through the host. It always closes req's body.
func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	enc, err := encodeRequest(req)
	if err != nil {
		return nil, err
	}

	decoded, err := httpabi.DecodeResponse(t.Exchange(enc))
	if err != nil {
		return nil, err //nolint:wrapcheck // the host's refusal is the error, as a dialing error would be
	}

	return &http.Response{
		Status:        strconv.Itoa(decoded.Status) + " " + http.StatusText(decoded.Status),
		StatusCode:    decoded.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        decoded.Header,
		Body:          io.NopCloser(bytes.NewReader(decoded.Body)),
		ContentLength: int64(len(decoded.Body)),
		Request:       req,
	}, nil
}

// encodeRequest encodes req and closes its body.
func encodeRequest(req *http.Request) ([]byte, error) {
	if req.Body != nil {
		defer req.Body.Close() //nolint:errcheck // the body was read to the end; closing it reports nothing useful
	}

	if req.URL == nil {
		return nil, ErrNoURL
	}

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}

	enc := httpabi.AppendRequestHead(nil, method, req.URL.String(), req.Header)
	if req.Body == nil {
		return enc, nil
	}

	enc, err := httpabi.AppendBody(enc, req.Body, maxRequest-len(enc))
	if err != nil {
		return nil, err //nolint:wrapcheck // httpabi errors already say what failed
	}

	return enc, nil
}
