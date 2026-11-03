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

// Hooks are the functions a [Client.Handler] reports to.
type Hooks struct {
	// Panicked receives a panic in the capability (or net/http), from the recovering goroutine (so it may record
	// the panicking stack); the request is answered with [ErrInternal].
	Panicked func(ctx context.Context, value any)
	// Done receives the report of every request, made, refused or failed, once it is answered.
	Done func(ctx context.Context, report Report)
}

// Outcome is how a request ended.
type Outcome uint8

// Outcomes of a request.
const (
	// OutcomeOK is a request that got a response, whatever its status code.
	OutcomeOK Outcome = iota
	// OutcomeRefused is a request the policy refused: HTTP disabled, a limit, a host not allowed, a private
	// address, an invalid request.
	OutcomeRefused
	// OutcomeFailed is a request that failed in transit, timed out, had too large a response, or met a panic.
	OutcomeFailed
)

// Report describes one request, for metrics.
type Report struct {
	Duration time.Duration // from the request reaching the host to its answer
	Status   int           // the response's status code; 0 unless the outcome is OutcomeOK
	Sent     int           // bytes of the encoded request (URL, header and body) the plugin handed over
	Received int           // bytes of the encoded response (status, header and body); 0 unless OutcomeOK
	Outcome  Outcome
}

// Handler returns the function serving the http_roundtrip requests of the plugin with the given key (the
// shape of engine.HTTPFunc). It appends the encoded answer to dst; req is the encoded request and n counts the
// requests of the current call. Each request is reported to hooks.Done once answered; a panic in the capability
// (or net/http) is passed to hooks.Panicked and answered with [ErrInternal].
func (c *Client) Handler(key string, hooks Hooks) func(ctx context.Context, dst, req []byte, n int) []byte {
	return func(ctx context.Context, dst, req []byte, n int) []byte {
		return c.serve(ctx, key, hooks, dst, req, n)
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
func (c *Client) serve(ctx context.Context, key string, hooks Hooks, dst, req []byte, n int) []byte {
	var out []byte

	start := time.Now()

	func() {
		defer func() {
			if v := recover(); v != nil {
				hooks.Panicked(ctx, v)
				hooks.Done(ctx, Report{Duration: time.Since(start), Sent: len(req), Outcome: OutcomeFailed})

				out = httpabi.AppendFailure(dst, ErrInternal.Error())
			}
		}()

		out = c.answer(ctx, key, hooks.Done, start, dst, req, n)
	}()

	return out
}

// answer makes or refuses one request, logs it and reports it to done.
func (c *Client) answer(ctx context.Context, key string, done func(context.Context, Report), start time.Time,
	dst, req []byte, n int,
) []byte {
	xch := exchange{start: start, sent: len(req)}
	head := len(dst)

	out, err := c.do(ctx, &xch, dst, req, n)
	if err != nil {
		c.log(ctx, key, &xch, err)
		done(ctx, Report{Duration: time.Since(start), Sent: xch.sent, Outcome: outcome(err)})

		return httpabi.AppendFailure(out[:head], message(err))
	}

	xch.received = len(out) - head
	c.log(ctx, key, &xch, nil)
	done(ctx, Report{
		Duration: time.Since(start), Status: xch.status, Sent: xch.sent, Received: xch.received, Outcome: OutcomeOK,
	})

	return out
}

// outcome classifies a request's failure: in transit (a timeout and a too large response included) or refused
// by the policy.
func outcome(err error) Outcome {
	for _, failed := range []error{ErrFailed, ErrTimeout, ErrResponseTooLarge} {
		if errors.Is(err, failed) {
			return OutcomeFailed
		}
	}

	return OutcomeRefused
}

// do checks and makes one request, appending the encoded response to dst.
func (c *Client) do(ctx context.Context, xch *exchange, dst, raw []byte, n int) ([]byte, error) {
	err := c.refusal(n, len(raw))
	if err != nil {
		return dst, err
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

// Refuses reports whether the policy refuses a call's n-th request of size bytes whatever it holds: HTTP is not
// enabled, the request is past MaxRequestsPerCall or over MaxRequestBytes. A handler refuses such a request
// without making it, so a caller may refuse it at once through the handler, instead of keeping a copy for later.
func (c *Client) Refuses(n, size int) bool {
	return !c.enabled || n > c.policy.MaxRequestsPerCall || size > c.policy.MaxRequestBytes
}

// refusal returns the error refusing a call's n-th request of size bytes whatever it holds (see Refuses), or nil.
func (c *Client) refusal(n, size int) error {
	switch {
	case !c.enabled:
		return ErrDisabled
	case n > c.policy.MaxRequestsPerCall:
		return fmt.Errorf("%w: limit %d", ErrTooManyRequests, c.policy.MaxRequestsPerCall)
	case size > c.policy.MaxRequestBytes:
		return fmt.Errorf("%w: %d bytes, limit %d", ErrRequestTooLarge, size, c.policy.MaxRequestBytes)
	default:
		return nil
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

	// The transport writes Host, Content-Length and Transfer-Encoding itself and skips the header's entries of
	// those names, but only under their canonical names: canonicalized, a plugin's own (in any case, as a
	// script's fetch sends them) can neither reach another virtual host nor frame the request differently.
	for name, values := range decoded.Header {
		if canonical := http.CanonicalHeaderKey(name); canonical != name {
			delete(decoded.Header, name)

			decoded.Header[canonical] = append(decoded.Header[canonical], values...)
		}
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
