// Package httpabi is the encoding of the HTTP exchanges between a guest and the host's plg.http_roundtrip
// import, built on the [wire] encoding. Every guest language uses it, so it stays free of policy.
//
// A request is its method and URL (strings), its header (a pair count, then name and value strings) and then
// its body: every remaining byte. A response starts with a [wire.Status]: [wire.StatusOK] is followed by the
// status code (an unsigned integer), the header as in a request and the body; [wire.StatusError] by a message
// saying why the host refused or failed the request.
//
// The Host header is not sent: the host always uses the URL's host, so a plugin cannot reach another virtual
// host than the one the policy allowed.
package httpabi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/heynemann/go-plg/internal/wire"
)

var (
	// ErrMalformed reports bytes that are not a valid request or response.
	ErrMalformed = errors.New("httpabi: malformed message")
	// ErrBodyTooLarge reports a body over the limit given to [AppendBody].
	ErrBodyTooLarge = errors.New("httpabi: body too large")
)

// HostError is a request the host refused or failed. Its message is the host's.
type HostError struct {
	Message string
}

// Error returns the host's message.
func (e *HostError) Error() string {
	return e.Message
}

// Request is a decoded request.
type Request struct {
	Header http.Header
	Method string
	URL    string
	// Body aliases the decoded buffer.
	Body []byte
}

// Response is a decoded response.
type Response struct {
	Header http.Header
	// Body aliases the decoded buffer.
	Body   []byte
	Status int
}

// pairSize is the encoded size of an empty header pair; it bounds how many pairs a message can hold.
const pairSize = 8

// AppendRequestHead appends a request's method, URL and header. The body follows: append it with
// [AppendBody] or append.
func AppendRequestHead(dst []byte, method, url string, header http.Header) []byte {
	return appendHeader(appendText(appendText(dst, method), url), header)
}

// AppendResponseHead appends [wire.StatusOK], a response's status code and its header. The body follows:
// append it with [AppendBody] or append.
func AppendResponseHead(dst []byte, status int, header http.Header) []byte {
	dst = wire.AppendUint(append(dst, byte(wire.StatusOK)), uint64(max(status, 0)))

	return appendHeader(dst, header)
}

// AppendFailure appends a response saying the host refused or failed the request, with msg as the reason.
func AppendFailure(dst []byte, msg string) []byte {
	return wire.AppendMessage(append(dst, byte(wire.StatusError)), msg)
}

func appendHeader(dst []byte, header http.Header) []byte {
	pairs := 0
	for _, values := range header {
		pairs += len(values)
	}

	dst = wire.AppendUint(dst, uint64(pairs))

	for name, values := range header {
		for _, value := range values {
			dst = appendText(appendText(dst, name), value)
		}
	}

	return dst
}

// appendText appends s as a [wire] string. Unlike [wire.AppendString] it cannot fail: every string encoded
// here is far shorter than 4 GiB, since guest strings live in a wasm32 address space and the host bounds the
// response headers it encodes.
func appendText(dst []byte, s string) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(s))) //nolint:gosec // G115: see above

	return append(dst, s...)
}

// readChunk is how much spare capacity AppendBody makes before each read.
const readChunk = 32 << 10

// AppendBody appends everything r yields to dst, reading straight into dst's spare capacity. It fails with
// [ErrBodyTooLarge] as soon as r yields more than limit bytes, so it never buffers more than limit plus one
// read; the bytes read so far stay appended.
func AppendBody(dst []byte, r io.Reader, limit int) ([]byte, error) {
	start := len(dst)

	for {
		if cap(dst)-len(dst) < readChunk {
			dst = append(dst, make([]byte, readChunk)...)[:len(dst)]
		}

		n, err := r.Read(dst[len(dst):cap(dst)])
		dst = dst[:len(dst)+n]

		if len(dst)-start > limit {
			return dst, fmt.Errorf("%w: over %d bytes", ErrBodyTooLarge, limit)
		}

		if errors.Is(err, io.EOF) {
			return dst, nil
		}

		if err != nil {
			return dst, fmt.Errorf("httpabi: reading body: %w", err)
		}
	}
}

// DecodeRequest decodes a request. It never allocates more than the input justifies.
func DecodeRequest(enc []byte) (Request, error) {
	reader := wire.NewReader(enc)

	method, err := reader.Text()
	if err != nil {
		return Request{}, fmt.Errorf("%w: method: %w", ErrMalformed, err)
	}

	url, err := reader.Text()
	if err != nil {
		return Request{}, fmt.Errorf("%w: url: %w", ErrMalformed, err)
	}

	header, err := decodeHeader(&reader)
	if err != nil {
		return Request{}, err
	}

	return Request{Method: method, URL: url, Header: header, Body: rest(enc, &reader)}, nil
}

// DecodeResponse decodes a response. A failure the host reported is a [*HostError].
func DecodeResponse(enc []byte) (Response, error) {
	if len(enc) == 0 {
		return Response{}, fmt.Errorf("%w: empty response", ErrMalformed)
	}

	reader := wire.NewReader(enc[1:])

	switch wire.Status(enc[0]) { //nolint:exhaustive // the host answers with OK or Error only
	case wire.StatusOK:
	case wire.StatusError:
		msg, err := reader.Text()
		if err == nil {
			err = reader.End()
		}

		if err != nil {
			return Response{}, fmt.Errorf("%w: failure message: %w", ErrMalformed, err)
		}

		return Response{}, &HostError{Message: msg}
	default:
		return Response{}, fmt.Errorf("%w: status %d", ErrMalformed, enc[0])
	}

	status, err := reader.Uint64()
	if err != nil {
		return Response{}, fmt.Errorf("%w: status code: %w", ErrMalformed, err)
	}

	if status > maxStatus {
		return Response{}, fmt.Errorf("%w: status code %d", ErrMalformed, status)
	}

	header, err := decodeHeader(&reader)
	if err != nil {
		return Response{}, err
	}

	return Response{Status: int(status), Header: header, Body: rest(enc[1:], &reader)}, nil
}

// maxStatus is the largest status code a response may carry; HTTP codes have three digits.
const maxStatus = 999

func decodeHeader(reader *wire.Reader) (http.Header, error) {
	count, err := reader.Uint64()
	if err != nil {
		return nil, fmt.Errorf("%w: header count: %w", ErrMalformed, err)
	}

	if count > uint64(reader.Len()/pairSize) { //nolint:gosec // G115: Len is never negative
		return nil, fmt.Errorf("%w: header count %d exceeds the message", ErrMalformed, count)
	}

	header := make(http.Header, count)

	for i := range count {
		name, err := reader.Text()
		if err != nil {
			return nil, fmt.Errorf("%w: header %d name: %w", ErrMalformed, i, err)
		}

		value, err := reader.Text()
		if err != nil {
			return nil, fmt.Errorf("%w: header %s: %w", ErrMalformed, name, err)
		}

		header[name] = append(header[name], value)
	}

	return header, nil
}

// rest returns the unread bytes of b, which reader reads.
func rest(b []byte, reader *wire.Reader) []byte {
	return b[len(b)-reader.Len():]
}
