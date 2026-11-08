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

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/heynemann/go-plg/internal/httpabi"
)

// Hooks are the functions a [Client.Handler] reports to.
type Hooks struct {
	// Panicked receives a panic in the capability (or net/http), from the recovering goroutine (so it may record
	// the panicking stack); the request is answered with [ErrInternal].
	Panicked func(ctx context.Context, value any)
	// Done receives the report of every request, made, refused or failed, once it is answered.
	Done func(ctx context.Context, report Report)
	// Attr is logged with every request, after the key (the plugin's owner); a zero Attr, or an empty group, is
	// left out by slog's handlers.
	Attr slog.Attr
	// Mark, when not nil, returns an attribute logged with a request after Attr, from the request's context (the
	// plugin call's); a zero Attr is left out.
	Mark func(ctx context.Context) slog.Attr
	// Trace, when not nil, starts the span of a request from the call's context: it returns the context holding
	// it, which the request is made with and whose span context its traceparent header carries, and the function
	// ending it, or nil when it traces nothing.
	Trace func(ctx context.Context) (context.Context, SpanEnd)
	// Scrub, when not nil, scrubs what a request's log record and report say of it beyond its method and sizes
	// (its URL and why it was refused or failed), such as the plugin's secrets; what the plugin is told is not
	// scrubbed.
	Scrub func(string) string
}

// SpanEnd ends a request's span with the request's report.
type SpanEnd func(Report)

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

// Report describes one request, for metrics and traces.
type Report struct {
	Err      error         // why the request was refused or failed; nil for OutcomeOK
	Method   string        // the request's method; empty when it was refused before it was decoded
	URL      string        // scheme://host/path: no credentials, no query; empty as Method
	Duration time.Duration // from the request reaching the host to its answer
	Status   int           // the response's status code; 0 unless the outcome is OutcomeOK
	Sent     int           // bytes of the encoded request (URL, header and body) the plugin handed over
	Received int           // bytes of the encoded response (status, header and body); 0 unless OutcomeOK
	Outcome  Outcome
}

// Handler returns the function serving the http_roundtrip requests of the plugin with the given key (the
// shape of engine.HTTPFunc). It appends the encoded answer to dst; req is the encoded request and n counts the
// requests of the current call. Each request is reported to hooks.Done (and its span ended) once answered; a
// panic in the capability (or net/http) is passed to hooks.Panicked and answered with [ErrInternal].
func (c *Client) Handler(key string, hooks *Hooks) func(ctx context.Context, dst, req []byte, n int) []byte {
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

// serve answers one request, turning a panic in the capability (or net/http) into an internal error. The
// request's span (hooks.Trace) starts before the request reaches the host; the hook guards itself.
func (c *Client) serve(ctx context.Context, key string, hooks *Hooks, dst, req []byte, n int) []byte {
	var (
		out   []byte
		ended SpanEnd
	)

	if hooks.Trace != nil {
		ctx, ended = hooks.Trace(ctx)
	}

	start := time.Now()

	func() {
		defer func() {
			if v := recover(); v != nil {
				hooks.Panicked(ctx, v)
				report(ctx, hooks, ended, &Report{
					Err: ErrInternal, Duration: time.Since(start), Sent: len(req), Outcome: OutcomeFailed,
				})

				out = httpabi.AppendFailure(dst, ErrInternal.Error())
			}
		}()

		out = c.answer(ctx, key, hooks, ended, start, dst, req, n)
	}()

	return out
}

// report reports a request to hooks.Done and ends its span (ended, if not nil).
func report(ctx context.Context, hooks *Hooks, ended SpanEnd, rep *Report) {
	hooks.Done(ctx, *rep)

	if ended != nil {
		ended(*rep)
	}
}

// answer makes or refuses one request, logs it and reports it (see report).
func (c *Client) answer(ctx context.Context, key string, hooks *Hooks, ended SpanEnd, start time.Time, dst,
	req []byte, n int,
) []byte {
	xch := exchange{start: start, sent: len(req)}
	head := len(dst)

	out, err := c.do(ctx, &xch, dst, req, n)
	if hooks.Scrub != nil {
		xch.target = hooks.Scrub(xch.target)
	}

	if err != nil {
		reason := message(err)
		err = scrubbed(hooks.Scrub, err)

		c.log(ctx, key, hooks, &xch, err)
		report(ctx, hooks, ended, &Report{
			Err: err, Method: xch.method, URL: xch.target, Duration: time.Since(start), Sent: xch.sent,
			Outcome: outcome(err),
		})

		return httpabi.AppendFailure(out[:head], reason)
	}

	xch.received = len(out) - head
	c.log(ctx, key, hooks, &xch, nil)
	report(ctx, hooks, ended, &Report{
		Method: xch.method, URL: xch.target, Duration: time.Since(start), Status: xch.status, Sent: xch.sent,
		Received: xch.received, Outcome: OutcomeOK,
	})

	return out
}

// scrubbed returns err with its message scrubbed by scrub (nil: nothing to scrub): err itself when that changes
// nothing, else an error with the scrubbed message wrapping err.
func scrubbed(scrub func(string) string, err error) error {
	if scrub == nil {
		return err
	}

	if msg := scrub(err.Error()); msg != err.Error() {
		return &scrubbedError{error: err, msg: msg}
	}

	return err
}

// scrubbedError is an error whose message is scrubbed; it wraps the original.
type scrubbedError struct {
	error

	msg string
}

func (e *scrubbedError) Error() string { return e.msg }

func (e *scrubbedError) Unwrap() error { return e.error }

// outcome classifies a request's failure: in transit (a timeout and a too large response included) or refused
// by the policy.
func outcome(err error) Outcome {
	for _, failed := range []error{ErrFailed, ErrTimeout, ErrResponseTooLarge, ErrUpgrade} {
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

	// A 101 hands the connection over: the request's timeout and cancellation no longer reach it, so a silent
	// server would hold the call forever. The deferred Close closes the upgraded connection.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return dst, ErrUpgrade
	}

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
		return nil, nil, errUnparsableURL
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

		return nil, nil, fmt.Errorf("%w: %w", ErrBadRequest, withoutURL(err))
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
	propagate(reqCtx, hreq.Header)

	return hreq, cancel, nil
}

// propagate sets the traceparent and tracestate headers of the span context in ctx (its span's, or the
// caller's), replacing the plugin's own; without one, the plugin's go through.
func propagate(ctx context.Context, header http.Header) {
	if trace.SpanContextFromContext(ctx).IsValid() {
		header.Del("Tracestate")
		propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(header))
	}
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
		return fmt.Errorf("%w: %w", ErrFailed, withoutURL(err))
	}
}

// errUnparsableURL refuses a URL that does not parse. The parser's own error quotes the whole URL, credentials
// and query included, and its details quote parts of it: neither reaches the log or the plugin.
var errUnparsableURL = fmt.Errorf("%w: unparsable URL", ErrBadRequest)

// withoutURL is err without the URL a *url.Error quotes, credentials and query included.
func withoutURL(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s: %w", urlErr.Op, withoutURL(urlErr.Err))
	}

	return err
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

func (c *Client) log(ctx context.Context, key string, hooks *Hooks, xch *exchange, err error) {
	var mark slog.Attr

	if hooks.Mark != nil {
		mark = hooks.Mark(ctx)
	}

	attrs := []slog.Attr{
		slog.String("key", key),
		hooks.Attr,
		mark,
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
