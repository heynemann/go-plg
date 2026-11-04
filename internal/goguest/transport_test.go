package goguest_test

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/goguest"
	"github.com/heynemann/go-plg/internal/httpabi"
)

// host is a fake host: it records the requests it is handed and answers with answer.
type host struct {
	answer func(req httpabi.Request) []byte
	seen   []httpabi.Request
}

func (h *host) exchange(raw []byte) []byte {
	req, err := httpabi.DecodeRequest(raw)
	if err != nil {
		panic(err)
	}

	req.Body = append([]byte(nil), req.Body...)
	h.seen = append(h.seen, req)

	return h.answer(req)
}

func ok(status int, header http.Header, body string) func(httpabi.Request) []byte {
	return func(httpabi.Request) []byte {
		return append(httpabi.AppendResponseHead(nil, status, header), body...)
	}
}

// trackedBody is a request body that records whether it was closed.
type trackedBody struct {
	io.Reader

	closed bool
}

func (b *trackedBody) Close() error {
	b.closed = true

	return nil
}

func TestTransportSendsRequestsThroughTheHost(t *testing.T) {
	t.Parallel()

	fake := &host{answer: ok(http.StatusCreated, http.Header{"X-Reply": {"yes"}}, "made")}
	body := &trackedBody{Reader: strings.NewReader("payload")}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://user:pw@api.example/v1/items?x=1",
		body)
	require.NoError(t, err)

	req.Header.Set("X-Test", "1")

	req.Host = "ignored.example"

	resp, err := goguest.Transport{Exchange: fake.exchange}.RoundTrip(req)
	require.NoError(t, err)

	defer func() { assert.NoError(t, resp.Body.Close()) }()

	require.Len(t, fake.seen, 1)
	assert.Equal(t, http.MethodPut, fake.seen[0].Method)
	assert.Equal(t, "https://user:pw@api.example/v1/items?x=1", fake.seen[0].URL)
	assert.Equal(t, http.Header{"X-Test": {"1"}}, fake.seen[0].Header, "the Host override is not sent")
	assert.Equal(t, "payload", string(fake.seen[0].Body))
	assert.True(t, body.closed, "RoundTrip closes the request body")

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "made", string(got))
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "201 Created", resp.Status)
	assert.Equal(t, int64(4), resp.ContentLength)
	assert.Equal(t, "yes", resp.Header.Get("X-Reply"))
	assert.Same(t, req, resp.Request)
}

func TestTransportServesTheStandardClient(t *testing.T) {
	t.Parallel()

	fake := &host{answer: func(req httpabi.Request) []byte {
		if strings.HasSuffix(req.URL, "/old") {
			return httpabi.AppendResponseHead(nil, http.StatusFound, http.Header{"Location": {"/new"}})
		}

		return append(httpabi.AppendResponseHead(nil, http.StatusOK, nil), "moved here"...)
	}}
	client := &http.Client{Transport: goguest.Transport{Exchange: fake.exchange}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.example/old", http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { assert.NoError(t, resp.Body.Close()) }()

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "moved here", string(got))
	require.Len(t, fake.seen, 2, "the client follows the redirect with a second request through the host")
	assert.Equal(t, "http://api.example/new", fake.seen[1].URL)
	assert.Equal(t, http.MethodGet, fake.seen[1].Method)
	assert.Empty(t, fake.seen[1].Body)
}

// credentials are the headers carrying credentials, as a request sets them.
func credentials() http.Header {
	return http.Header{
		"Authorization": {"Bearer t"}, "Cookie": {"c=1"}, "Cookie2": {"c2"}, "Proxy-Authenticate": {"pa"},
		"Proxy-Authorization": {"Basic p"}, "Www-Authenticate": {"wa"},
	}
}

func TestTransportKeepsCredentialsWithinTheFirstOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hops []string // the first request's URL, then each redirect's Location
		kept []bool   // whether each request carried the credentials
	}{
		{"same origin", []string{"http://api.example/a", "/b"}, []bool{true, true}},
		{
			"same origin spelled otherwise",
			[]string{"http://api.example/a", "HTTP://API.Example:80/b"},
			[]bool{true, true},
		},
		{"https default port", []string{"https://api.example/a", "https://api.example:443/b"}, []bool{true, true}},
		{"IPv6 default port", []string{"http://[::1]:80/a", "http://[::1]/b"}, []bool{true, true}},
		{"other port", []string{"http://api.example/a", "http://api.example:8080/b"}, []bool{true, false}},
		{"https to http", []string{"https://api.example/a", "http://api.example/b"}, []bool{true, false}},
		{"http to https", []string{"http://api.example/a", "https://api.example/b"}, []bool{true, false}},
		{"other scheme", []string{"http://api.example/a", "ftp://api.example/b"}, []bool{true, false}},
		{"subdomain", []string{"http://api.example/a", "http://sub.api.example/b"}, []bool{true, false}},
		{"other host", []string{"http://api.example/a", "http://other.example/b"}, []bool{true, false}},
		{
			"same origin, then another",
			[]string{"http://api.example/a", "/b", "http://api.example:8080/c"},
			[]bool{true, true, false},
		},
		{
			"left and back",
			[]string{"http://api.example/a", "http://api.example:8080/b", "http://api.example/c"},
			[]bool{true, false, false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &host{}
			fake.answer = func(httpabi.Request) []byte {
				if len(fake.seen) < len(tc.hops) {
					return httpabi.AppendResponseHead(nil, http.StatusFound,
						http.Header{"Location": {tc.hops[len(fake.seen)]}})
				}

				return httpabi.AppendResponseHead(nil, http.StatusOK, nil)
			}
			client := &http.Client{Transport: goguest.Transport{Exchange: fake.exchange}}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.hops[0], http.NoBody)
			require.NoError(t, err)

			req.Header = credentials()
			req.Header.Set("X-Test", "kept")

			resp, err := client.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			require.Len(t, fake.seen, len(tc.kept))

			for i, kept := range tc.kept {
				want := http.Header{"X-Test": {"kept"}}
				if kept {
					want = credentials()
					want.Set("X-Test", "kept")
				}

				got := fake.seen[i].Header.Clone()
				got.Del("Referer") // the client's own, on some hops
				assert.Equal(t, want, got, "request %d (%s)", i, fake.seen[i].URL)
			}

			want := credentials()
			want.Set("X-Test", "kept")
			assert.Equal(t, want, req.Header, "the caller's header is left as it was")
		})
	}
}

func TestTransportDropsCredentialsAfterAHopWithoutURL(t *testing.T) {
	t.Parallel()

	target, err := url.Parse("http://api.example/")
	require.NoError(t, err)

	fake := &host{answer: ok(http.StatusOK, nil, "")}
	req := &http.Request{URL: target, Header: credentials(), Response: &http.Response{Request: &http.Request{}}}

	resp, err := goguest.Transport{Exchange: fake.exchange}.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Empty(t, fake.seen[0].Header, "a hop after one without an origin is another origin's")
}

func TestTransportDefaultsToGET(t *testing.T) {
	t.Parallel()

	fake := &host{answer: ok(http.StatusNoContent, nil, "")}
	u, err := url.Parse("http://api.example/")
	require.NoError(t, err)

	resp, err := goguest.Transport{Exchange: fake.exchange}.RoundTrip(&http.Request{URL: u})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.MethodGet, fake.seen[0].Method)
}

var errRead = errors.New("disk on fire")

func TestTransportErrors(t *testing.T) {
	t.Parallel()

	target, err := url.Parse("http://api.example/")
	require.NoError(t, err)

	refusal := &httpabi.HostError{Message: "plg http: host not allowed"}

	tests := []struct {
		answer func(httpabi.Request) []byte
		body   io.Reader
		url    *url.URL
		want   error
		name   string
	}{
		{ok(http.StatusOK, nil, ""), strings.NewReader("x"), nil, goguest.ErrNoURL, "no URL"},
		{ok(http.StatusOK, nil, ""), iotest.ErrReader(errRead), target, errRead, "unreadable body"},
		{
			func(httpabi.Request) []byte { return httpabi.AppendFailure(nil, refusal.Message) },
			strings.NewReader("x"), target, refusal, "host refusal",
		},
		{func(httpabi.Request) []byte { return []byte{0} }, nil, target, httpabi.ErrMalformed, "malformed answer"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var body *trackedBody

			req := &http.Request{Method: http.MethodPost, URL: tc.url}
			if tc.body != nil {
				body = &trackedBody{Reader: tc.body}
				req.Body = body
			}

			fake := &host{answer: tc.answer}

			resp, err := goguest.Transport{Exchange: fake.exchange}.RoundTrip(req) //nolint:bodyclose // nil, see below
			require.Nil(t, resp)

			if errors.Is(tc.want, refusal) {
				hostErr, ok := errors.AsType[*httpabi.HostError](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, refusal, hostErr, "the host's refusal is the round trip's error")
			} else {
				require.ErrorIs(t, err, tc.want)
			}

			if body != nil {
				assert.True(t, body.closed, "the body is closed on errors too")
			}
		})
	}
}
