package httpcap_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/httpabi"
	"github.com/heynemann/go-plg/internal/httpcap"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// server is a loopback API answering "<method> <path> <X-Test> <body length>" with an X-Reply header, or what
// its handler says, and counting the requests it got.
type server struct {
	*httptest.Server

	hits atomic.Int64
}

func newServer(t *testing.T, handler http.HandlerFunc) *server {
	t.Helper()

	srv := &server{}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		srv.hits.Add(1)

		if handler != nil {
			handler(writer, req)

			return
		}

		body, err := io.ReadAll(req.Body)
		assert.NoError(t, err)

		writer.Header().Set("X-Reply", "yes")

		reply := req.Method + " " + req.URL.Path + " " + req.Header.Get("X-Test") + " " + strconv.Itoa(len(body))
		_, err = io.WriteString(writer, reply) //nolint:gosec // G705: a test server echoing its own test's request
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func (s *server) host(t *testing.T) string {
	t.Helper()

	u, err := url.Parse(s.URL)
	require.NoError(t, err)

	return u.Host
}

// newClient returns a client for policy, logging as JSON to the returned buffer.
func newClient(t *testing.T, policy httpcap.Policy) (*httpcap.Client, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer

	client, err := httpcap.New(policy, slog.New(slog.NewJSONHandler(&logs, nil)))
	require.NoError(t, err)
	t.Cleanup(client.Close)

	return client, &logs
}

// request encodes a request.
func request(method, rawURL string, header http.Header, body string) []byte {
	return append(httpabi.AppendRequestHead(nil, method, rawURL, header), body...)
}

// answer is the decoded answer to one request: a response, or the reason the host refused or failed it, and the
// request's report.
type answer struct {
	refused string
	resp    httpabi.Response
	report  httpcap.Report
}

// unexpectedPanic is the panicked function of handlers whose capability must not panic.
func unexpectedPanic(tb testing.TB) func(context.Context, any) {
	tb.Helper()

	return func(_ context.Context, v any) { tb.Errorf("the capability panicked: %v", v) }
}

// serve runs one request through the client's handler for plugin "web/p", as the nth of its call, behind a
// prefix that must survive, and decodes the answer.
func serve(ctx context.Context, t *testing.T, client *httpcap.Client, req []byte, n int) answer {
	t.Helper()

	return serveWith(ctx, t, client, unexpectedPanic(t), req, n)
}

// serveWith is serve passing the capability's panics to panicked. Every request is reported once, with the
// bytes the plugin sent and, for a response, the bytes it received.
func serveWith(ctx context.Context, t *testing.T, client *httpcap.Client, panicked func(context.Context, any),
	req []byte, n int,
) answer {
	t.Helper()

	var reports []httpcap.Report

	handler := client.Handler("web/p", httpcap.Hooks{
		Panicked: panicked,
		Done:     func(_ context.Context, r httpcap.Report) { reports = append(reports, r) },
	})

	prefix := []byte("keep")
	out := handler(ctx, bytes.Clone(prefix), req, n)
	require.Equal(t, prefix, out[:len(prefix)], "the answer is appended")
	require.Len(t, reports, 1, "one report per request")

	got := answer{report: reports[0]}
	assert.Positive(t, got.report.Duration)
	assert.Equal(t, len(req), got.report.Sent)

	resp, err := httpabi.DecodeResponse(bytes.Clone(out[len(prefix):]))
	if hostErr, ok := errors.AsType[*httpabi.HostError](err); ok {
		assert.NotEqual(t, httpcap.OutcomeOK, got.report.Outcome)
		assert.Zero(t, got.report.Status)
		assert.Zero(t, got.report.Received)
		assert.Error(t, got.report.Err, "a refusal or failure says why")

		got.refused = hostErr.Message

		return got
	}

	require.NoError(t, err, "a well-formed answer")
	assert.Equal(t, httpcap.OutcomeOK, got.report.Outcome)
	assert.Equal(t, resp.Status, got.report.Status)
	assert.Equal(t, len(out)-len(prefix), got.report.Received, "the encoded response")
	assert.NoError(t, got.report.Err)

	got.resp = resp

	return got
}

func TestPolicyDefaults(t *testing.T) {
	t.Parallel()

	client, _ := newClient(t, httpcap.Policy{AllowedHosts: []string{"a.example"}})
	assert.Equal(t, httpcap.Policy{
		AllowedHosts:       []string{"a.example"},
		Timeout:            10 * time.Second,
		MaxRequestBytes:    1 << 20,
		MaxResponseBytes:   4 << 20,
		MaxRequestsPerCall: 16,
	}, httpcap.EffectivePolicy(client))

	set := httpcap.Policy{
		AllowedHosts: []string{"a.example"}, Timeout: time.Second, MaxRequestBytes: 1, MaxResponseBytes: 2,
		MaxRequestsPerCall: 3, BlockPrivateNetworks: true,
	}
	client, _ = newClient(t, set)
	assert.Equal(t, set, httpcap.EffectivePolicy(client), "set values are kept")
}

func TestInvalidPolicies(t *testing.T) {
	t.Parallel()

	hosts := []string{"a.example"}

	for name, policy := range map[string]httpcap.Policy{
		"no hosts":                    {},
		"bad host":                    {AllowedHosts: []string{"a.example", "http://b.example"}},
		"negative timeout":            {AllowedHosts: hosts, Timeout: -1},
		"negative request size":       {AllowedHosts: hosts, MaxRequestBytes: -1},
		"request size over 1 GiB":     {AllowedHosts: hosts, MaxRequestBytes: httpcap.MaxBytes + 1},
		"negative response size":      {AllowedHosts: hosts, MaxResponseBytes: -1},
		"response size over 1 GiB":    {AllowedHosts: hosts, MaxResponseBytes: httpcap.MaxBytes + 1},
		"negative requests per call":  {AllowedHosts: hosts, MaxRequestsPerCall: -1},
		"a bad host among good hosts": {AllowedHosts: []string{"a.example", "", "b.example"}},
	} {
		_, err := httpcap.New(policy, slog.New(slog.DiscardHandler))
		require.ErrorIs(t, err, httpcap.ErrInvalidPolicy, name)
	}

	limits := httpcap.Policy{AllowedHosts: hosts, MaxRequestBytes: httpcap.MaxBytes, MaxResponseBytes: httpcap.MaxBytes}
	_, err := httpcap.New(limits, slog.New(slog.DiscardHandler))
	require.NoError(t, err, "the bounds themselves are allowed")
}

func TestHandlerMakesAllowedRequests(t *testing.T) {
	t.Parallel()

	srv := newServer(t, nil)
	client, logs := newClient(t, httpcap.Policy{AllowedHosts: []string{srv.host(t)}})

	got := serve(t.Context(), t, client,
		request(http.MethodPost, srv.URL+"/v1/items?secret=1", http.Header{"X-Test": {"on"}}, "12345"), 1)
	require.Empty(t, got.refused)

	resp := got.resp
	assert.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "yes", resp.Header.Get("X-Reply"))
	assert.Equal(t, "POST /v1/items on 5", string(resp.Body))

	line := logs.String()
	assert.Contains(t, line, `"level":"INFO","msg":"plg: http request","key":"web/p","method":"POST","url":"`+
		srv.URL+`/v1/items"`, "logged by key, without the query")
	assert.Contains(t, line, `"request_bytes":`)
	assert.Contains(t, line, `"status":200,"response_bytes":`)
}

func TestHandlerDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "http://169.254.169.254/", http.StatusFound)
	})
	client, _ := newClient(t, httpcap.Policy{AllowedHosts: []string{srv.host(t)}})

	got := serve(t.Context(), t, client, request(http.MethodGet, srv.URL, nil, ""), 1)
	require.Empty(t, got.refused)

	resp := got.resp
	assert.Equal(t, http.StatusFound, resp.Status, "the guest's client decides, and its next hop is checked")
	assert.Equal(t, "http://169.254.169.254/", resp.Header.Get("Location"))
}

func TestHandlerRefusals(t *testing.T) {
	t.Parallel()

	srv := newServer(t, nil)
	host := srv.host(t)
	policy := httpcap.Policy{AllowedHosts: []string{host}, MaxRequestBytes: 100, MaxRequestsPerCall: 2}

	tests := []struct {
		want error
		name string
		req  []byte
		n    int
	}{
		{httpcap.ErrTooManyRequests, "over the per-call count", request("GET", srv.URL, nil, ""), 3},
		{
			httpcap.ErrRequestTooLarge,
			"over the request size",
			request("POST", srv.URL, nil, strings.Repeat("x", 100)),
			1,
		},
		{
			httpcap.ErrRequestTooLarge, "headers count toward the size",
			request("GET", srv.URL, http.Header{"X-Big": {strings.Repeat("x", 80)}}, ""), 1,
		},
		{httpcap.ErrBadRequest, "malformed encoding", []byte{1, 2, 3}, 1},
		{httpcap.ErrBadRequest, "unparsable URL", request("GET", "http://[::1", nil, ""), 1},
		{httpcap.ErrBadRequest, "invalid method", request("BAD METHOD", srv.URL, nil, ""), 1},
		{httpcap.ErrScheme, "file URL", request("GET", "file:///etc/passwd", nil, ""), 1},
		{httpcap.ErrScheme, "relative URL", request("GET", "/x", nil, ""), 1},
		{httpcap.ErrMethod, "CONNECT", request("CONNECT", srv.URL, nil, ""), 1},
		{httpcap.ErrHostNotAllowed, "another host", request("GET", "http://other.example/", nil, ""), 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, logs := newClient(t, policy)
			assert.Equal(t,
				errors.Is(tc.want, httpcap.ErrTooManyRequests) || errors.Is(tc.want, httpcap.ErrRequestTooLarge),
				client.Refuses(tc.n, len(tc.req)), "Refuses tells the refusals of the limits in advance")
			assert.False(t, client.Refuses(policy.MaxRequestsPerCall, policy.MaxRequestBytes), "the limits are allowed")

			got := serve(t.Context(), t, client, tc.req, tc.n)
			assert.True(t, strings.HasPrefix(got.refused, tc.want.Error()), "%q", got.refused)
			assert.Equal(t, httpcap.OutcomeRefused, got.report.Outcome)
			assert.Contains(
				t,
				logs.String(),
				`"level":"WARN","msg":"plg: http request refused or failed","key":"web/p"`,
			)
		})
	}

	t.Cleanup(func() { assert.Zero(t, srv.hits.Load(), "no refused request reached the server") })
}

// TestRefusedURLsNeverLogCredentials: a URL the handler cannot parse is refused with a reason naming neither its
// credentials nor its query, in the log or in the plugin's answer.
func TestRefusedURLsNeverLogCredentials(t *testing.T) {
	t.Parallel()

	srv := newServer(t, nil)

	for _, rawURL := range []string{
		"https://alice:hunter2pw@api.example/%zz?access_token=s3cr3tTok",
		"https://alice:hunter2pw@api.example:bad/?access_token=s3cr3tTok",
		"http://alice:hunter2pw@[::1/?access_token=s3cr3tTok",
		"https://alice:hunter2pw@api.example/\x7f?access_token=s3cr3tTok",
	} {
		client, logs := newClient(t, httpcap.Policy{AllowedHosts: []string{srv.host(t), "api.example"}})

		got := serve(t.Context(), t, client, request("GET", rawURL, nil, ""), 1)
		assert.True(t, strings.HasPrefix(got.refused, httpcap.ErrBadRequest.Error()+": "), "%q", got.refused)
		assert.Contains(t, got.refused, "URL", "the reason is still clear")
		assert.Contains(t, logs.String(), `"msg":"plg: http request refused or failed"`)

		for _, secret := range []string{"hunter2pw", "s3cr3tTok", "alice"} {
			assert.NotContains(t, got.refused, secret, rawURL)
			assert.NotContains(t, logs.String(), secret, rawURL)
		}
	}
}

func TestDisabledRefusesEverything(t *testing.T) {
	t.Parallel()

	srv := newServer(t, nil)

	var logs bytes.Buffer

	client := httpcap.Disabled(slog.New(slog.NewJSONHandler(&logs, nil)))
	client.Close()
	assert.True(t, client.Refuses(1, 0))

	got := serve(t.Context(), t, client, request("GET", srv.URL, nil, ""), 1)
	assert.Equal(t, httpcap.ErrDisabled.Error(), got.refused)
	assert.Equal(t, httpcap.OutcomeRefused, got.report.Outcome)
	assert.Contains(t, logs.String(), `"key":"web/p"`, "refusals are logged too")
	assert.Zero(t, srv.hits.Load())
}

// TestRequestLogsCarryTheHooksAttr: the Attr of a handler's hooks (the plugin's owner) is logged with every
// request after the key, a refusal's included.
func TestRequestLogsCarryTheHooksAttr(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	client := httpcap.Disabled(slog.New(slog.NewJSONHandler(&logs, nil)))
	client.Close()

	handler := client.Handler("web/p", httpcap.Hooks{
		Panicked: unexpectedPanic(t),
		Done:     func(context.Context, httpcap.Report) {},
		Attr:     slog.Group("owner", slog.String("team", "payments")),
	})
	handler(t.Context(), nil, request("GET", "https://a.example/", nil, ""), 1)

	assert.Contains(t, logs.String(), `"msg":"plg: http request refused or failed","key":"web/p",`+
		`"owner":{"team":"payments"},"method":`)
}

func TestPrivateNetworksAreBlockedAtDialTime(t *testing.T) {
	t.Parallel()

	srv := newServer(t, nil)
	_, port, found := strings.Cut(srv.host(t), ":")
	require.True(t, found)

	client, logs := newClient(t, httpcap.Policy{
		AllowedHosts: []string{"127.0.0.1", "localhost"}, BlockPrivateNetworks: true,
	})

	for _, target := range []string{srv.URL, "http://localhost:" + port + "/"} {
		got := serve(t.Context(), t, client, request("GET", target, nil, ""), 1)
		assert.Equal(t, httpcap.ErrPrivateAddress.Error(), got.refused, "%s: the plugin does not learn the address",
			target)
		assert.Equal(t, httpcap.OutcomeRefused, got.report.Outcome, "the policy refused it, at dial time")
	}

	assert.Contains(t, logs.String(), "127.0.0.1", "the log has the refused address")
	assert.Zero(t, srv.hits.Load())
}

func TestLocalUseNAT64IsBlockedWithPrivateNetworks(t *testing.T) {
	t.Parallel()

	const target = "http://[64:ff9b:1:0:c0:a801:800:0]:1/" // 192.168.1.8 under the /64 layout

	hosts := []string{"64:ff9b:1:0:c0:a801:800:0"}

	blocked, _ := newClient(t, httpcap.Policy{AllowedHosts: hosts, BlockPrivateNetworks: true})
	got := serve(t.Context(), t, blocked, request("GET", target, nil, ""), 1)
	assert.Equal(t, httpcap.ErrPrivateAddress.Error(), got.refused, "refused before connecting")
	assert.Equal(t, httpcap.OutcomeRefused, got.report.Outcome)

	// Unblocked, the dial is made: it fails in transit (unreachable, or its 1ms timeout), never as private.
	open, _ := newClient(t, httpcap.Policy{AllowedHosts: hosts, Timeout: time.Millisecond})
	got = serve(t.Context(), t, open, request("GET", target, nil, ""), 1)
	assert.NotEqual(t, httpcap.ErrPrivateAddress.Error(), got.refused)
	assert.Equal(t, httpcap.OutcomeFailed, got.report.Outcome, "%s", got.refused)
}

func TestResponseLimits(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(writer http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/short" { // promises more than it sends
			writer.Header().Set("Content-Length", "100")
		}

		size, err := strconv.Atoi(strings.TrimPrefix(req.URL.Path, "/"))
		if err != nil {
			size = 9
		}

		_, err = writer.Write(bytes.Repeat([]byte("x"), size))
		assert.NoError(t, err)
	})
	client, _ := newClient(t, httpcap.Policy{AllowedHosts: []string{srv.host(t)}, MaxResponseBytes: 4096})

	got := serve(t.Context(), t, client, request("GET", srv.URL+"/4096", nil, ""), 1)
	require.Empty(t, got.refused)
	assert.Len(t, got.resp.Body, 4096, "exactly the limit")

	got = serve(t.Context(), t, client, request("GET", srv.URL+"/4097", nil, ""), 1)
	assert.Contains(t, got.refused, httpcap.ErrResponseTooLarge.Error())
	assert.Equal(t, httpcap.OutcomeFailed, got.report.Outcome, "the request was made")

	got = serve(t.Context(), t, client, request("GET", srv.URL+"/short", nil, ""), 1)
	assert.Contains(t, got.refused, httpcap.ErrFailed.Error(), "a truncated body fails the request")
	assert.Equal(t, httpcap.OutcomeFailed, got.report.Outcome)
}

func TestRequestsEndWithTheirContext(t *testing.T) {
	t.Parallel()

	canceled := make(chan struct{}, 1)
	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/canceled" {
			canceled <- struct{}{}
		}

		<-r.Context().Done()
	})
	short, _ := newClient(t, httpcap.Policy{AllowedHosts: []string{srv.host(t)}, Timeout: 50 * time.Millisecond})

	// The timeout ends the request wherever it is: on a loaded machine it may not have reached the server yet.
	got := serve(t.Context(), t, short, request("GET", srv.URL+"/timeout", nil, ""), 1)
	assert.Equal(t, httpcap.ErrTimeout.Error(), got.refused, "the policy's timeout")
	assert.Equal(t, httpcap.OutcomeFailed, got.report.Outcome)

	client, _ := newClient(t, httpcap.Policy{AllowedHosts: []string{srv.host(t)}, Timeout: time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan []byte)
	reports := make(chan httpcap.Report, 1)
	handler := client.Handler("web/p", httpcap.Hooks{
		Panicked: unexpectedPanic(t),
		Done:     func(_ context.Context, r httpcap.Report) { reports <- r },
	})

	go func() {
		done <- handler(ctx, nil, request("GET", srv.URL+"/canceled", nil, ""), 1)
	}()

	<-canceled // the server holds the request, which only the call's context can end within the hour
	cancel()

	_, err := httpabi.DecodeResponse(<-done)

	hostErr, ok := errors.AsType[*httpabi.HostError](err)
	require.True(t, ok, "%v", err)

	refused := hostErr.Message
	assert.Contains(t, refused, httpcap.ErrFailed.Error())
	assert.Contains(t, refused, context.Canceled.Error(), "the call's own context")
	assert.Equal(t, httpcap.OutcomeFailed, (<-reports).Outcome)
}

func TestTransportFailures(t *testing.T) {
	t.Parallel()

	// The server hangs up without answering, so the request fails in transit. Dialing the port of a closed server
	// would not do: a server started meanwhile, by another test or process, may be listening on it again.
	srv := newServer(t, func(writer http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(writer).Hijack()
		if assert.NoError(t, err) {
			assert.NoError(t, conn.Close())
		}
	})
	client, _ := newClient(t, httpcap.Policy{AllowedHosts: []string{srv.host(t)}})

	got := serve(t.Context(), t, client, request("GET", srv.URL, nil, ""), 1)
	assert.Contains(t, got.refused, httpcap.ErrFailed.Error(), "the server hung up")
	assert.Equal(t, httpcap.OutcomeFailed, got.report.Outcome)
}

type panickingTransport struct{}

func (panickingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("transport bug")
}

func TestHandlerRecoversPanics(t *testing.T) {
	t.Parallel()

	client, _ := newClient(t, httpcap.Policy{AllowedHosts: []string{"a.example"}})
	httpcap.SetTransport(client, panickingTransport{})

	var recovered []any

	got := serveWith(t.Context(), t, client, func(_ context.Context, v any) { recovered = append(recovered, v) },
		request("GET", "http://a.example/", nil, ""), 1)
	assert.Equal(t, httpcap.ErrInternal.Error(), got.refused, "the plugin is told about an internal error")
	assert.Equal(t, []any{"transport bug"}, recovered, "the panic went to the panicked function, once")
	assert.Equal(t, httpcap.OutcomeFailed, got.report.Outcome)
	require.ErrorIs(t, got.report.Err, httpcap.ErrInternal)
}

// spanContext returns a sampled span context with the given span ID and tracestate.
func spanContext(t *testing.T, span byte, state string) trace.SpanContext {
	t.Helper()

	ts, err := trace.ParseTraceState(state)
	require.NoError(t, err)

	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x0a, 15: 0x01}, SpanID: trace.SpanID{7: span}, TraceFlags: trace.FlagsSampled,
		TraceState: ts,
	})
}

// TestRequestsCarryTheirSpanContext: a request carries the span context of its context in traceparent and
// tracestate, replacing the plugin's own, which go through when the context holds none. With a Trace hook, the
// request is made with the hook's context and its span ended once with the request's report, refusals included.
func TestRequestsCarryTheirSpanContext(t *testing.T) {
	t.Parallel()

	type propagated struct{ parent, state string }

	got := make(chan propagated, 1)
	srv := newServer(t, func(_ http.ResponseWriter, req *http.Request) {
		got <- propagated{req.Header.Get("Traceparent"), req.Header.Get("Tracestate")}
	})
	client, _ := newClient(t, httpcap.Policy{AllowedHosts: []string{srv.host(t)}})

	const pluginParent = "00-0b000000000000000000000000000002-0000000000000009-01"

	plugins := http.Header{"Tracestate": {"p=1"}}
	plugins["traceparent"] = []string{pluginParent} // as a script plugin may write it

	serve(t.Context(), t, client, request("GET", srv.URL, plugins.Clone(), ""), 1)
	assert.Equal(t, propagated{pluginParent, "p=1"}, <-got, "no span context: the plugin's own")

	caller := trace.ContextWithSpanContext(t.Context(), spanContext(t, 1, "c=1"))
	serve(caller, t, client, request("GET", srv.URL, plugins.Clone(), ""), 1)
	assert.Equal(t, propagated{"00-0a000000000000000000000000000001-0000000000000001-01", "c=1"}, <-got,
		"the caller's span context replaces the plugin's")

	var ended []httpcap.Report

	handler := client.Handler("web/p", httpcap.Hooks{
		Panicked: unexpectedPanic(t),
		Done:     func(context.Context, httpcap.Report) {},
		Trace: func(ctx context.Context) (context.Context, httpcap.SpanEnd) {
			assert.Equal(t, caller, ctx, "the hook starts from the call's context")

			return trace.ContextWithSpanContext(ctx, spanContext(t, 2, "")),
				func(report httpcap.Report) { ended = append(ended, report) }
		},
	})

	handler(caller, nil, request("GET", srv.URL+"/items?token=secret", plugins.Clone(), ""), 1)
	assert.Equal(t, propagated{"00-0a000000000000000000000000000001-0000000000000002-01", ""}, <-got,
		"the hook's span, and no plugin tracestate beside it")
	require.Len(t, ended, 1, "the span is ended once")
	assert.Equal(t, "GET", ended[0].Method)
	assert.Equal(t, srv.URL+"/items", ended[0].URL, "no query")
	assert.Equal(t, http.StatusOK, ended[0].Status)
	assert.Equal(t, httpcap.OutcomeOK, ended[0].Outcome)
	require.NoError(t, ended[0].Err)

	handler(caller, nil, request("POST", "http://refused.example/x", nil, ""), 1)
	require.Len(t, ended, 2, "a refused request's span is ended too")
	assert.Equal(t, "POST", ended[1].Method)
	assert.Equal(t, httpcap.OutcomeRefused, ended[1].Outcome)
	require.ErrorIs(t, ended[1].Err, httpcap.ErrHostNotAllowed)
}

// benchTarget is a handler allowed to reach a loopback server answering "ok", and a GET for it.
type benchTarget struct {
	handler func(ctx context.Context, dst, req []byte, n int) []byte
	req     []byte
}

func benchClient(b *testing.B) benchTarget {
	b.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(w, "ok")
		assert.NoError(b, err)
	}))
	b.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(b, err)

	client, err := httpcap.New(httpcap.Policy{AllowedHosts: []string{u.Host}}, slog.New(slog.DiscardHandler))
	require.NoError(b, err)
	b.Cleanup(client.Close)

	hooks := httpcap.Hooks{Panicked: unexpectedPanic(b), Done: func(context.Context, httpcap.Report) {}}

	return benchTarget{handler: client.Handler("bench/p", hooks), req: request(http.MethodGet, srv.URL, nil, "")}
}

// BenchmarkRoundTrip is the host side of one plugin request (checks, request, response encoding) against a
// loopback server, without the guest.
func BenchmarkRoundTrip(b *testing.B) {
	target := benchClient(b)

	var dst []byte

	b.ReportAllocs()

	for b.Loop() {
		dst = target.handler(b.Context(), dst[:0], target.req, 1)
	}
}

func BenchmarkRoundTripParallel(b *testing.B) {
	target := benchClient(b)

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var dst []byte

		for pb.Next() {
			dst = target.handler(b.Context(), dst[:0], target.req, 1)
		}
	})
}
