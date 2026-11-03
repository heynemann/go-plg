//go:build integration

package plg_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
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

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/httpcap"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// apiServer is a loopback test API. It replies "<method> <path> <X-Test header> <Host> <body length>" with an
// X-Reply header, redirects /redirect?to=<url> and counts the requests it served.
type apiServer struct {
	*httptest.Server

	hits atomic.Int64
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()

	api := &apiServer{}
	api.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		api.hits.Add(1)

		to := req.URL.Query().Get("to")
		if to != "" {
			http.Redirect(writer, req, to, http.StatusFound) //nolint:gosec // G710: the test chooses the target

			return
		}

		body, err := io.ReadAll(req.Body)
		assert.NoError(t, err)

		writer.Header().Set("X-Reply", "from-api")

		reply := strings.Join([]string{
			req.Method, req.URL.Path, req.Header.Get("X-Test"), req.Host, strconv.Itoa(len(body)),
		}, " ")
		_, err = io.WriteString(writer, reply) //nolint:gosec // G705: a test server echoing its own test's request
		assert.NoError(t, err)
	}))
	t.Cleanup(api.Close)

	return api
}

// hostOf returns the host and port of a server URL.
func hostOf(tb testing.TB, rawURL string) string {
	tb.Helper()

	u, err := url.Parse(rawURL)
	require.NoError(tb, err)

	return u.Host
}

// openPolicy allows the loopback test servers: they live on a private address, so the policy must opt out of
// private network blocking explicitly.
func openPolicy(hosts ...string) plg.HTTPPolicy {
	return plg.HTTPPolicy{AllowedHosts: hosts, BlockPrivateNetworks: new(false)}
}

// httpContainer returns a container logging to logs. Tests share one per test function: each container loads
// the large Go guest module once, so a container per subtest would multiply the memory of a parallel run.
func httpContainer(t *testing.T, logs io.Writer) *plg.Container {
	t.Helper()

	ctr := plg.New(plg.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	return ctr
}

// fetcherIn registers the fetcher plugin under name with opts in ctr, and returns it: its key is name+"/fetch".
//
//nolint:ireturn // the plugin is served as the contract interface under test
func fetcherIn(t *testing.T, ctr *plg.Container, name string, opts ...plg.RegisterOption) testcontract.Fooer {
	t.Helper()

	opts = append(opts, plg.WithStorage(goStorage(t, name, map[string]string{"fetch": "fetcher"})))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, name, opts...))

	return getFooer(t, ctr, name+"/fetch")
}

// fetcher registers the fetcher plugin as "web/fetch" in a container of its own logging to logs.
//
//nolint:ireturn // the plugin is served as the contract interface under test
func fetcher(t *testing.T, logs io.Writer, opts ...plg.RegisterOption) testcontract.Fooer {
	t.Helper()

	return fetcherIn(t, httpContainer(t, logs), "web", opts...)
}

func TestPluginHTTPReachesAllowedHosts(t *testing.T) {
	t.Parallel()

	api := newAPIServer(t)

	var logs bytes.Buffer

	plug := fetcher(t, &logs, plg.WithHTTP(openPolicy(hostOf(t, api.URL))))

	got, err := plug.Foo(t.Context(), "GET "+api.URL+"/v1/items")
	require.NoError(t, err)
	assert.Equal(t, "200|from-api|GET /v1/items fetcher "+hostOf(t, api.URL)+" 0", got,
		"status, response header and body reach the plugin; its header reaches the server")

	got, err = plug.Foo(t.Context(), "POST "+api.URL+"/v1/items 5000")
	require.NoError(t, err)
	assert.Equal(t, "200|from-api|POST /v1/items  "+hostOf(t, api.URL)+" 5000", got, "request bodies too")

	got, err = plug.Foo(t.Context(), "HOST "+api.URL+"/v1/x internal.service")
	require.NoError(t, err)
	assert.Equal(t, "200|from-api|GET /v1/x fetcher "+hostOf(t, api.URL)+" 0", got,
		"a Host override never reaches another virtual host than the allowed one")

	assert.Contains(t, logs.String(), `"msg":"plg: http request","key":"web/fetch","method":"POST","url":"`+
		api.URL+`/v1/items"`, "requests are logged by plugin key")
	assert.Contains(t, logs.String(), `"status":200,"response_bytes":`)
}

func TestPluginHTTPRefusals(t *testing.T) {
	t.Parallel()

	api := newAPIServer(t)
	_, port, err := net.SplitHostPort(hostOf(t, api.URL))
	require.NoError(t, err)

	policy := func(p plg.HTTPPolicy) []plg.RegisterOption { return []plg.RegisterOption{plg.WithHTTP(p)} }

	tests := []struct {
		want error
		name string
		url  string
		opts []plg.RegisterOption
	}{
		{want: httpcap.ErrDisabled, name: "without WithHTTP", url: api.URL},
		{
			want: httpcap.ErrHostNotAllowed, name: "host not allowed", url: api.URL,
			opts: policy(openPolicy("api.example.com")),
		},
		{
			want: httpcap.ErrHostNotAllowed, name: "other port", url: api.URL,
			opts: policy(openPolicy("127.0.0.1:1")),
		},
		{
			want: httpcap.ErrPrivateAddress, name: "loopback blocked by default", url: api.URL,
			opts: policy(plg.HTTPPolicy{AllowedHosts: []string{"127.0.0.1"}}),
		},
		{
			want: httpcap.ErrPrivateAddress, name: "a name resolving to loopback is blocked at dial time",
			url:  "http://localhost:" + port + "/",
			opts: policy(plg.HTTPPolicy{AllowedHosts: []string{"localhost"}}),
		},
		{
			want: httpcap.ErrPrivateAddress, name: "explicitly blocked", url: api.URL,
			opts: policy(plg.HTTPPolicy{AllowedHosts: []string{"127.0.0.1"}, BlockPrivateNetworks: new(true)}),
		},
		{
			want: httpcap.ErrScheme, name: "not http", url: "ftp://127.0.0.1/",
			opts: policy(openPolicy("127.0.0.1")),
		},
	}

	var logs bytes.Buffer

	ctr := httpContainer(t, &logs)

	// Runs after every subtest, before the container closes: the log is complete and no longer written.
	t.Cleanup(func() {
		assert.Zero(t, api.hits.Load(), "no refused request reached the server")

		for i := range tests {
			assert.Contains(t, logs.String(), `"msg":"plg: http request refused or failed","key":"r`+
				strconv.Itoa(i)+`/fetch"`, "refusals are logged by plugin key")
		}
	})

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := fetcherIn(t, ctr, "r"+strconv.Itoa(i), tc.opts...).Foo(t.Context(), "GET "+tc.url)
			require.ErrorContains(t, err, tc.want.Error())
			require.NotErrorIs(t, err, plg.ErrCall, "a refusal is the plugin's own error, the instance lives on")
		})
	}
}

func TestPluginHTTPRedirectsAreCheckedHopByHop(t *testing.T) {
	t.Parallel()

	api := newAPIServer(t)
	plug := fetcher(t, io.Discard, plg.WithHTTP(openPolicy(hostOf(t, api.URL))))

	got, err := plug.Foo(t.Context(), "GET "+api.URL+"/redirect?to=/landed")
	require.NoError(t, err)
	assert.Equal(t, "200|from-api|GET /landed fetcher "+hostOf(t, api.URL)+" 0", got)

	_, err = plug.Foo(t.Context(), "GET "+api.URL+"/redirect?to=http://169.254.169.254/latest/meta-data")
	require.ErrorContains(t, err, httpcap.ErrHostNotAllowed.Error(), "a redirect to the metadata service")
}

func TestPluginHTTPLimits(t *testing.T) {
	t.Parallel()

	api := newAPIServer(t)
	endless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for r.Context().Err() == nil {
			_, err := w.Write(chunk)
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(endless.Close)

	policy := openPolicy(hostOf(t, api.URL), hostOf(t, endless.URL))
	policy.MaxRequestBytes = 4 << 10
	policy.MaxResponseBytes = 64 << 10
	policy.MaxRequestsPerCall = 3
	plug := fetcher(t, io.Discard, plg.WithHTTP(policy))

	t.Run("request size", func(t *testing.T) {
		t.Parallel()

		_, err := plug.Foo(t.Context(), "POST "+api.URL+"/ 3000")
		require.NoError(t, err, "under the limit")

		_, err = plug.Foo(t.Context(), "POST "+api.URL+"/ 4096")
		require.ErrorContains(t, err, httpcap.ErrRequestTooLarge.Error(), "URL, header and body count")
	})

	t.Run("endless response", func(t *testing.T) {
		t.Parallel()

		_, err := plug.Foo(t.Context(), "GET "+endless.URL)
		require.ErrorContains(t, err, httpcap.ErrResponseTooLarge.Error(),
			"the host stops reading at the limit instead of buffering an endless body")
	})

	t.Run("requests per call", func(t *testing.T) {
		t.Parallel()

		_, err := plug.Foo(t.Context(), "REPEAT 3 "+api.URL)
		require.NoError(t, err)

		_, err = plug.Foo(t.Context(), "REPEAT 4 "+api.URL)
		require.ErrorContains(t, err, httpcap.ErrTooManyRequests.Error())

		_, err = plug.Foo(t.Context(), "REPEAT 3 "+api.URL)
		require.NoError(t, err, "the count starts over with every call")
	})
}

// stalling is a server that accepts requests and holds them until they are abandoned, reporting each request's
// start on started and its abandonment on abandoned.
type stalling struct {
	*httptest.Server

	started, abandoned chan struct{}
}

func stallingServer(t *testing.T) *stalling {
	t.Helper()

	srv := &stalling{started: make(chan struct{}, 1), abandoned: make(chan struct{}, 1)}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		srv.started <- struct{}{}

		<-r.Context().Done()

		srv.abandoned <- struct{}{}
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestPluginHTTPEndsWithTheCall(t *testing.T) {
	t.Parallel()

	ctr := httpContainer(t, io.Discard)

	t.Run("cancelled call", func(t *testing.T) {
		t.Parallel()

		srv := stallingServer(t)
		plug := fetcherIn(t, ctr, "cancel", plg.WithHTTP(openPolicy(hostOf(t, srv.URL))))

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)

		go func() {
			_, err := plug.Foo(ctx, "GET "+srv.URL)
			done <- err
		}()

		<-srv.started
		cancel()

		// Either the guest is stopped once the request fails or it reports the failed request first: both say why.
		require.ErrorContains(t, <-done, context.Canceled.Error())
		<-srv.abandoned // the in-flight request was aborted, not left running
	})

	t.Run("call deadline before the policy timeout", func(t *testing.T) {
		t.Parallel()

		srv := stallingServer(t)
		plug := fetcherIn(t, ctr, "deadline", plg.WithHTTP(openPolicy(hostOf(t, srv.URL))))

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		_, err := plug.Foo(ctx, "GET "+srv.URL)
		require.ErrorContains(t, err, context.DeadlineExceeded.Error(),
			"the 10s default timeout never outlives the call")
		<-srv.abandoned
	})

	t.Run("policy timeout", func(t *testing.T) {
		t.Parallel()

		srv := stallingServer(t)
		policy := openPolicy(hostOf(t, srv.URL))
		policy.Timeout = 50 * time.Millisecond
		plug := fetcherIn(t, ctr, "timeout", plg.WithHTTP(policy))

		_, err := plug.Foo(t.Context(), "GET "+srv.URL)
		require.ErrorContains(t, err, httpcap.ErrTimeout.Error())
		require.NotErrorIs(t, err, plg.ErrCall, "the call itself goes on: the plugin sees a failed request")
		<-srv.abandoned
	})
}

// benchTarget is the fetcher plugin allowed to reach a loopback server answering "ok", and the GET command for
// that server.
type benchTarget struct {
	plug testcontract.Fooer
	cmd  string
}

func benchFetcher(b *testing.B) benchTarget {
	b.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(w, "ok")
		assert.NoError(b, err)
	}))
	b.Cleanup(srv.Close)

	ctr := plg.New()

	b.Cleanup(func() { assert.NoError(b, ctr.Close(context.Background())) })

	store := goStorage(b, "web", map[string]string{"fetch": "fetcher"})
	require.NoError(b, plg.Register[testcontract.Fooer](b.Context(), ctr, "web", plg.WithStorage(store),
		plg.WithHTTP(openPolicy(hostOf(b, srv.URL)))))

	return benchTarget{plug: getFooer(b, ctr, "web/fetch"), cmd: "GET " + srv.URL}
}

// BenchmarkHTTPRoundTripGo is one plugin call making one http.Get through the host, every protection on.
func BenchmarkHTTPRoundTripGo(b *testing.B) {
	target := benchFetcher(b)

	b.ReportAllocs()

	for b.Loop() {
		_, err := target.plug.Foo(b.Context(), target.cmd)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHTTPRoundTripGoParallel(b *testing.B) {
	target := benchFetcher(b)

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := target.plug.Foo(b.Context(), target.cmd)
			if err != nil {
				b.Error(err)

				return
			}
		}
	})
}
