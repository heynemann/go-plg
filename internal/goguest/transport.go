package goguest

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
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
// round trip's error. Redirects, cookies and client timeouts stay the http.Client's job, inside the guest, with
// one rule stricter than the client's own: once a redirect leaves the first request's origin (scheme, host and
// port), that hop and every later one go without the credential headers (see leftOrigin).
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

	header := req.Header
	if leftOrigin(req) {
		header = withoutCredentials(header)
	}

	enc := httpabi.AppendRequestHead(nil, method, req.URL.String(), header)
	if req.Body == nil {
		return enc, nil
	}

	enc, err := httpabi.AppendBody(enc, req.Body, maxRequest-len(enc))
	if err != nil {
		return nil, err //nolint:wrapcheck // httpabi errors already say what failed
	}

	return enc, nil
}

// leftOrigin reports whether req is a redirect hop that left its first request's origin, or follows a hop that did.
// An http.Client's redirect hop carries the redirect response that led to it in Response, whose Request is the hop
// before. The client itself keeps the credential headers on a redirect to the same host on another port or scheme
// (an https-to-http downgrade included) and to its subdomains; the guest does not, as fetch and http.request do not.
func leftOrigin(req *http.Request) bool {
	first := req
	for first.Response != nil && first.Response.Request != nil {
		first = first.Response.Request
	}

	for hop := req; hop != first; hop = hop.Response.Request {
		if !sameOrigin(hop.URL, first.URL) {
			return true
		}
	}

	return false
}

// sameOrigin reports whether a and b have the same scheme, host and port (the scheme's default port if none),
// ASCII letters compared in any case. A missing URL has no origin.
func sameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && equalFold(a.Scheme, b.Scheme) && equalFold(a.Hostname(), b.Hostname()) &&
		port(a) == port(b)
}

// port returns u's port, or its scheme's default.
func port(u *url.URL) string {
	switch {
	case u.Port() != "":
		return u.Port()
	case equalFold(u.Scheme, "https"):
		return "443"
	case equalFold(u.Scheme, "http"):
		return "80"
	default:
		return ""
	}
}

// equalFold reports whether a and b are equal with their ASCII letters in any case (non-ASCII bytes must match).
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range len(a) {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}

	return true
}

func lower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}

	return c
}

// withoutCredentials returns a copy of header without the headers carrying credentials (net/http's list).
func withoutCredentials(header http.Header) http.Header {
	out := make(http.Header, len(header))

	for name, values := range header {
		switch http.CanonicalHeaderKey(name) {
		case "Authorization", "Cookie", "Cookie2", "Proxy-Authenticate", "Proxy-Authorization", "Www-Authenticate":
		default:
			out[name] = values
		}
	}

	return out
}
