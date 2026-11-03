package httpcap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/heynemann/go-plg/internal/httpabi"
)

// Handler returns the function serving the http_roundtrip requests of the plugin with the given key (the
// shape of engine.HTTPFunc). It appends the encoded answer to dst; req is the encoded request and n counts the
// requests of the current call. A panic in the capability (or net/http) is passed to panicked, from the
// recovering goroutine (so it may record the panicking stack), and answered with [ErrInternal].
func (c *Client) Handler(key string, panicked func(ctx context.Context, value any)) func(
	ctx context.Context, dst, req []byte, n int,
) []byte {
	return func(ctx context.Context, dst, req []byte, n int) []byte {
		return c.serve(ctx, key, panicked, dst, req, n)
	}
}

// exchange is one request in progress, for its log line.
type exchange struct {
	start    time.Time
	method   string
	target   string // scheme://host/path: no credentials, no query
	status   int
	sent     int
	received int
}

// serve answers one request, turning a panic in the capability (or net/http) into an internal error.
func (c *Client) serve(ctx context.Context, key string, panicked func(context.Context, any), dst, req []byte,
	n int,
) []byte {
	var out []byte

	func() {
		defer func() {
			if v := recover(); v != nil {
				panicked(ctx, v)

				out = httpabi.AppendFailure(dst, ErrInternal.Error())
			}
		}()

		out = c.answer(ctx, key, dst, req, n)
	}()

	return out
}

// answer makes or refuses one request and logs it.
func (c *Client) answer(ctx context.Context, key string, dst, req []byte, n int) []byte {
	xch := exchange{start: time.Now(), sent: len(req)}
	head := len(dst)

	out, err := c.do(ctx, &xch, dst, req, n)
	if err != nil {
		c.log(ctx, key, &xch, err)

		return httpabi.AppendFailure(out[:head], message(err))
	}

	xch.received = len(out) - head
	c.log(ctx, key, &xch, nil)

	return out
}

// do checks and makes one request, appending the encoded response to dst.
func (c *Client) do(ctx context.Context, xch *exchange, dst, raw []byte, n int) ([]byte, error) {
	if !c.enabled {
		return dst, ErrDisabled
	}

	if n > c.policy.MaxRequestsPerCall {
		return dst, fmt.Errorf("%w: limit %d", ErrTooManyRequests, c.policy.MaxRequestsPerCall)
	}

	if len(raw) > c.policy.MaxRequestBytes {
		return dst, fmt.Errorf("%w: %d bytes, limit %d", ErrRequestTooLarge, len(raw), c.policy.MaxRequestBytes)
	}

	hreq, cancel, err := c.request(ctx, xch, raw)
	if err != nil {
		return dst, err
	}
	defer cancel()

	resp, err := c.transport.RoundTrip(hreq)
	if err != nil {
		return dst, failure(ctx, hreq.Context(), c.policy.Timeout, err)
	}
	defer resp.Body.Close() //nolint:errcheck // the body was read; a close error changes nothing

	xch.status = resp.StatusCode

	out, err := httpabi.AppendBody(httpabi.AppendResponseHead(dst, resp.StatusCode, resp.Header), resp.Body,
		c.policy.MaxResponseBytes)

	switch {
	case errors.Is(err, httpabi.ErrBodyTooLarge):
		return dst, fmt.Errorf("%w: limit %d bytes", ErrResponseTooLarge, c.policy.MaxResponseBytes)
	case err != nil:
		return dst, failure(ctx, hreq.Context(), c.policy.Timeout, err)
	default:
		return out, nil
	}
}

// request decodes and checks the guest's request and builds the one the transport sends. Its context ends at
// the policy's timeout or with ctx.
func (c *Client) request(ctx context.Context, xch *exchange, raw []byte) (*http.Request, context.CancelFunc, error) {
	decoded, err := httpabi.DecodeRequest(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}

	xch.method = decoded.Method

	target, err := url.Parse(decoded.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}

	xch.target = (&url.URL{Scheme: target.Scheme, Host: target.Host, Path: target.Path}).String()

	switch {
	case target.Scheme != "http" && target.Scheme != "https":
		return nil, nil, fmt.Errorf("%w %q", ErrScheme, target.Scheme)
	case decoded.Method == http.MethodConnect:
		return nil, nil, fmt.Errorf("%w: %s", ErrMethod, decoded.Method)
	case !c.allowed.allows(target):
		return nil, nil, fmt.Errorf("%w: %s", ErrHostNotAllowed, hostPort(target))
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.policy.Timeout)

	// The body is copied: the transport may still read it after RoundTrip returns, when guest memory is no
	// longer the host's to read.
	hreq, err := http.NewRequestWithContext(reqCtx, decoded.Method, target.String(),
		bytes.NewReader(bytes.Clone(decoded.Body)))
	if err != nil {
		cancel()

		return nil, nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}

	hreq.Header = decoded.Header

	return hreq, cancel, nil
}

// failure classifies a transport error: the call's own context ending, the policy's timeout, a refused
// private address or another failure in transit.
func failure(callCtx, reqCtx context.Context, timeout time.Duration, err error) error {
	switch {
	case errors.Is(err, ErrPrivateAddress):
		return err
	case callCtx.Err() != nil:
		return fmt.Errorf("%w: %w", ErrFailed, context.Cause(callCtx))
	case reqCtx.Err() != nil:
		return fmt.Errorf("%w after %s", ErrTimeout, timeout)
	default:
		return fmt.Errorf("%w: %w", ErrFailed, err)
	}
}

// message is what the plugin is told: the refusal without addresses the plugin should not learn (a private
// address a name resolved to stays in the host's log).
func message(err error) string {
	for _, known := range []error{ErrPrivateAddress, ErrTimeout} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}

	return err.Error()
}

func (c *Client) log(ctx context.Context, key string, xch *exchange, err error) {
	attrs := []slog.Attr{
		slog.String("key", key),
		slog.String("method", xch.method),
		slog.String("url", xch.target),
		slog.Duration("duration", time.Since(xch.start)),
		slog.Int("request_bytes", xch.sent),
	}

	if err != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: http request refused or failed",
			append(attrs, slog.String("error", err.Error()))...)

		return
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "plg: http request",
		append(attrs, slog.Int("status", xch.status), slog.Int("response_bytes", xch.received))...)
}
