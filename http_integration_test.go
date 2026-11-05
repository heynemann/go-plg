//go:build integration

package plg_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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

// fetcherIn registers lang's fetcher plugin under name with opts in ctr, and returns it: its key is
// name+"/fetch".
//
//nolint:ireturn // the plugin is served as the contract interface under test
func fetcherIn(t *testing.T, lang contractLanguage, ctr *plg.Container, name string, opts ...plg.RegisterOption,
) testcontract.Fooer {
	t.Helper()

	opts = append(opts, plg.WithStorage(lang.storage(t, name, map[string]string{"fetch": "fetcher"})))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), lang.binder(ctr), name, opts...))

	return getFooer(t, ctr, name+"/fetch")
}

// fetcher registers lang's fetcher plugin as "web/fetch" in a container of its own logging to logs.
//
//nolint:ireturn // the plugin is served as the contract interface under test
func fetcher(t *testing.T, lang contractLanguage, logs io.Writer, opts ...plg.RegisterOption) testcontract.Fooer {
	t.Helper()

	return fetcherIn(t, lang, httpContainer(t, logs), "web", opts...)
}

func TestPluginHTTPReachesAllowedHosts(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		api := newAPIServer(t)

		var logs bytes.Buffer

		plug := fetcher(t, lang, &logs, plg.WithHTTP(openPolicy(hostOf(t, api.URL))))

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
	})
}

// TestGoPluginHTTPRequestsWithACancelledContextAreNotSent: a Go plugin's request whose own context is already
// cancelled fails with the context's error, closes its body and never reaches the server.
func TestGoPluginHTTPRequestsWithACancelledContextAreNotSent(t *testing.T) {
	t.Parallel()

	api := newAPIServer(t)
	plug := fetcher(t, golang(), io.Discard, plg.WithHTTP(openPolicy(hostOf(t, api.URL))))

	got, err := plug.Foo(t.Context(), "CANCELLED "+api.URL+"/v1/items")
	require.NoError(t, err)
	assert.Equal(t, `Post "`+api.URL+`/v1/items": context canceled|true`, got)
	assert.Zero(t, api.hits.Load(), "the server saw nothing")
}

func TestPluginHTTPRefusals(t *testing.T) {
	t.Parallel()

	eachLanguage(t, httpRefusals)
}

func httpRefusals(t *testing.T, lang contractLanguage) {
	t.Helper()

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

			_, err := fetcherIn(t, lang, ctr, "r"+strconv.Itoa(i), tc.opts...).Foo(t.Context(), "GET "+tc.url)
			require.ErrorContains(t, err, tc.want.Error())
			require.NotErrorIs(t, err, plg.ErrCall, "a refusal is the plugin's own error, the instance lives on")
		})
	}
}

// TestPluginHTTPUnparsableURLsNeverLogCredentials: a plugin's URL the host cannot parse is refused without its
// credentials or query reaching the host's log or the refusal the plugin gets.
func TestPluginHTTPUnparsableURLsNeverLogCredentials(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		var logs bytes.Buffer

		plug := fetcher(t, lang, &logs, plg.WithHTTP(openPolicy("api.example")))

		_, err := plug.Foo(t.Context(), "GET https://alice:hunter2pw@api.example/%zz?access_token=s3cr3tTok")
		require.Error(t, err)

		for _, secret := range []string{"hunter2pw", "s3cr3tTok"} {
			assert.NotContains(t, logs.String(), secret)

			if lang.lang != plg.LanguageGo { // Go's net/http in the guest refuses the URL itself, in the plugin's words
				assert.NotContains(t, err.Error(), secret)
			}
		}
	})
}

func TestPluginHTTPRedirectsAreCheckedHopByHop(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		api := newAPIServer(t)
		plug := fetcher(t, lang, io.Discard, plg.WithHTTP(openPolicy(hostOf(t, api.URL))))

		got, err := plug.Foo(t.Context(), "GET "+api.URL+"/redirect?to=/landed")
		require.NoError(t, err)
		assert.Equal(t, "200|from-api|GET /landed fetcher "+hostOf(t, api.URL)+" 0", got)

		_, err = plug.Foo(t.Context(), "GET "+api.URL+"/redirect?to=http://169.254.169.254/latest/meta-data")
		require.ErrorContains(t, err, httpcap.ErrHostNotAllowed.Error(), "a redirect to the metadata service")
	})
}

// tlsHandshake is the first byte of a TLS connection: a handshake record.
const tlsHandshake = 0x16

// sniffedConn is a server connection speaking TLS if its client's first byte opens a TLS handshake, plain HTTP
// otherwise. It decides on its first Read, which the server makes before writing anything.
type sniffedConn struct {
	net.Conn // the raw connection: addresses, deadlines, Close

	config *tls.Config
	rw     io.ReadWriter // nil until the first Read
}

func (c *sniffedConn) Read(buf []byte) (int, error) {
	if c.rw == nil {
		peeked := peekedConn{Conn: c.Conn, reader: bufio.NewReader(c.Conn)}

		c.rw = peeked

		first, err := peeked.reader.Peek(1)
		if err == nil && first[0] == tlsHandshake {
			c.rw = tls.Server(peeked, c.config)
		}
	}

	return c.rw.Read(buf) //nolint:wrapcheck // a connection's own error
}

func (c *sniffedConn) Write(buf []byte) (int, error) {
	return c.rw.Write(buf) //nolint:wrapcheck // a connection's own error
}

// peekedConn reads its connection through reader, which may hold bytes already read from it.
type peekedConn struct {
	net.Conn

	reader *bufio.Reader
}

func (c peekedConn) Read(buf []byte) (int, error) {
	return c.reader.Read(buf) //nolint:wrapcheck // a connection's own error
}

// sniffing is a listener whose connections speak https or http, as each client chooses.
type sniffing struct {
	net.Listener

	config *tls.Config
}

func (l sniffing) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err //nolint:wrapcheck // the listener's own error
	}

	return &sniffedConn{Conn: conn, config: l.config}, nil
}

// credentialHeaders are the request headers carrying credentials (net/http's list).
func credentialHeaders() []string {
	return []string{
		"Authorization", "Cookie", "Cookie2", "Proxy-Authenticate", "Proxy-Authorization", "Www-Authenticate",
	}
}

// credentialServer serves one loopback port over both http and https, so two of its URLs can differ by scheme
// alone; roots trusts its certificate. It redirects /redirect?to=<url> and answers any other path with "<how
// many of the credentialHeaders it got> <X-Test header>".
type credentialServer struct {
	*httptest.Server

	roots *x509.CertPool
}

func newCredentialServer(t *testing.T) *credentialServer {
	t.Helper()

	lender := httptest.NewTLSServer(http.NotFoundHandler()) // lends httptest's certificate for 127.0.0.1
	config := lender.TLS.Clone()
	roots := x509.NewCertPool()
	roots.AddCert(lender.Certificate())
	lender.Close()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if to := req.URL.Query().Get("to"); to != "" {
			http.Redirect(writer, req, to, http.StatusFound) //nolint:gosec // G710: the test chooses the target

			return
		}

		got := 0

		for _, name := range credentialHeaders() {
			if req.Header.Get(name) != "" {
				got++
			}
		}

		reply := strconv.Itoa(got) + " " + req.Header.Get("X-Test")
		_, err := io.WriteString(writer, reply) //nolint:gosec // G705: a test server echoing its own test's request
		assert.NoError(t, err)
	}))
	srv.Listener = sniffing{Listener: srv.Listener, config: config}
	srv.Start()
	t.Cleanup(srv.Close)

	return &credentialServer{Server: srv, roots: roots}
}

// TestPluginHTTPRedirectsKeepCredentialsWithinTheFirstOrigin holds the three languages' redirects to one rule: once
// a redirect leaves the first request's origin (scheme, host and port), that hop and every later one go without
// the headers carrying credentials, and the plugin's own headers are left as they were.
func TestPluginHTTPRedirectsKeepCredentialsWithinTheFirstOrigin(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		srv, other := newCredentialServer(t), newCredentialServer(t)
		_, port, err := net.SplitHostPort(hostOf(t, srv.URL))
		require.NoError(t, err)

		plug := fetcher(t, lang, io.Discard, plg.WithHTTP(openPolicy("127.0.0.1", "localhost")),
			plg.WithHTTPRoots(srv.roots))

		plain, secure, named := "http://127.0.0.1:"+port, "https://127.0.0.1:"+port, "http://localhost:"+port
		via := func(from, to string) string { return from + "/redirect?to=" + url.QueryEscape(to) }

		tests := []struct {
			name string
			url  string
			kept bool
		}{
			{"same origin", via(plain, "/seen"), true},
			{"same origin spelled otherwise", via(named, "HTTP://LOCALHOST:"+port+"/seen"), true},
			{"https, same origin", via(secure, "/seen"), true},
			{"other port", via(plain, other.URL+"/seen"), false},
			{"other host", via(plain, named+"/seen"), false},
			{"https to http", via(secure, plain+"/seen"), false},
			{"left and back", via(plain, via(named, plain+"/seen")), false},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				want := "200||0 fetcher|6"
				if tc.kept {
					want = "200||6 fetcher|6"
				}

				got, err := plug.Foo(t.Context(), "CREDS "+tc.url)
				require.NoError(t, err)
				assert.Equal(t, want, got, "the last hop's credentials and other header, then the plugin's credentials")
			})
		}
	})
}

func TestPluginHTTPLimits(t *testing.T) {
	t.Parallel()

	eachLanguage(t, httpLimits)
}

func httpLimits(t *testing.T, lang contractLanguage) {
	t.Helper()

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
	plug := fetcher(t, lang, io.Discard, plg.WithHTTP(policy))

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

// expiring is a call context whose deadline passes when the test closes it rather than at a set time: closed
// once the plugin's request is in flight, it is a deadline that cannot race the request. It carries no values
// and reports no deadline time.
type expiring chan struct{}

func (expiring) Deadline() (time.Time, bool) { return time.Time{}, false }

func (e expiring) Done() <-chan struct{} { return e }

func (e expiring) Err() error {
	select {
	case <-e:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (expiring) Value(any) any { return nil }

func TestPluginHTTPEndsWithTheCall(t *testing.T) {
	t.Parallel()

	eachLanguage(t, httpEndsWithTheCall)
}

func httpEndsWithTheCall(t *testing.T, lang contractLanguage) {
	t.Helper()

	ctr := httpContainer(t, io.Discard)

	t.Run("cancelled call", func(t *testing.T) {
		t.Parallel()

		srv := stallingServer(t)
		plug := fetcherIn(t, lang, ctr, "cancel", plg.WithHTTP(openPolicy(hostOf(t, srv.URL))))

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
		plug := fetcherIn(t, lang, ctr, "deadline", plg.WithHTTP(openPolicy(hostOf(t, srv.URL))))

		ctx := make(expiring)
		done := make(chan error)

		go func() {
			_, err := plug.Foo(ctx, "GET "+srv.URL)
			done <- err
		}()

		<-srv.started
		close(ctx) // the call's deadline passes while its request is in flight, well within the policy's 10s

		err := <-done
		require.ErrorContains(t, err, context.DeadlineExceeded.Error(),
			"the 10s default timeout never outlives the call")
		require.NotContains(t, err.Error(), httpcap.ErrTimeout.Error(), "the call's deadline, not the policy's")
		<-srv.abandoned
	})

	t.Run("policy timeout", func(t *testing.T) {
		t.Parallel()

		srv := stallingServer(t)
		policy := openPolicy(hostOf(t, srv.URL))
		policy.Timeout = 50 * time.Millisecond
		plug := fetcherIn(t, lang, ctr, "timeout", plg.WithHTTP(policy))

		_, err := plug.Foo(t.Context(), "GET "+srv.URL)
		require.ErrorContains(t, err, httpcap.ErrTimeout.Error())
		require.NotErrorIs(t, err, plg.ErrCall, "the call itself goes on: the plugin sees a failed request")

		select {
		case <-srv.started: // the request reached the server before its timeout: it was aborted there
			<-srv.abandoned

		default: // under load, the 50ms ran out before the request reached the server: nothing runs there
		}
	})
}

// upgrading is a server answering every request with 101 Switching Protocols and then staying silent, holding
// the connection until the client closes it; closed receives a value as each connection is closed.
type upgrading struct {
	*httptest.Server

	closed chan struct{}
}

func upgradingServer(t *testing.T) *upgrading {
	t.Helper()

	srv := &upgrading{closed: make(chan struct{}, 16)}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if !assert.NoError(t, err) {
			return
		}

		defer conn.Close() //nolint:errcheck // the test only waits for the client to close it

		_, err = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: silent\r\nConnection: Upgrade\r\n\r\n")
		assert.NoError(t, err)
		assert.NoError(t, buf.Flush())

		_, err = io.Copy(io.Discard, conn)
		assert.NoError(t, err, "the client closed the connection")

		srv.closed <- struct{}{}
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestPluginHTTPUpgradesAreRefused(t *testing.T) {
	t.Parallel()

	t.Run("blocking", func(t *testing.T) {
		t.Parallel()

		eachLanguage(t, func(t *testing.T, lang contractLanguage) {
			t.Helper()

			srv := upgradingServer(t)
			ctr := plg.New(plg.WithLogger(slog.New(slog.DiscardHandler)))
			plug := fetcherIn(t, lang, ctr, "upgrade", plg.WithHTTP(openPolicy(hostOf(t, srv.URL))))

			_, err := plug.Foo(t.Context(), "GET "+srv.URL)
			require.ErrorContains(t, err, httpcap.ErrUpgrade.Error())
			require.NotErrorIs(t, err, plg.ErrCall, "a refused upgrade is a failed request, the instance lives on")
			<-srv.closed // the upgraded connection was closed, not left to the server
			require.NoError(t, ctr.Close(t.Context()))
		})
	})

	t.Run("multiplexed", func(t *testing.T) {
		t.Parallel()

		eachScript(t, func(t *testing.T, lang contractLanguage) {
			t.Helper()

			srv := upgradingServer(t)
			ctr := plg.New(plg.WithLogger(slog.New(slog.DiscardHandler)))
			plug := muxFetcher(t, lang, ctr, "upgrade", srv.Server, 2)

			first, second := callLater(t.Context(), plug, "GET "+srv.URL), callLater(t.Context(), plug, "GET "+srv.URL)
			for _, done := range []<-chan callResult{first, second} {
				res := <-done
				require.ErrorContains(t, res.err, httpcap.ErrUpgrade.Error())
				require.NotErrorIs(t, res.err, plg.ErrCall)
				<-srv.closed
			}

			require.NoError(t, ctr.Close(t.Context()))
		})
	})
}

// benchTarget is the fetcher plugin allowed to reach a loopback server answering "ok", and the GET command for
// that server.
type benchTarget struct {
	plug testcontract.Fooer
	cmd  string
}

func benchFetcher(b *testing.B, lang contractLanguage) benchTarget {
	b.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(w, "ok")
		assert.NoError(b, err)
	}))
	b.Cleanup(srv.Close)

	ctr := plg.New()

	b.Cleanup(func() { assert.NoError(b, ctr.Close(context.Background())) })

	store := lang.storage(b, "web", map[string]string{"fetch": "fetcher"})
	require.NoError(b, plg.Register[testcontract.Fooer](b.Context(), ctr, "web", plg.WithStorage(store),
		plg.WithHTTP(openPolicy(hostOf(b, srv.URL)))))

	return benchTarget{plug: getFooer(b, ctr, "web/fetch"), cmd: "GET " + srv.URL}
}

// BenchmarkHTTPRoundTripGo is one plugin call making one http.Get through the host, every protection on.
func BenchmarkHTTPRoundTripGo(b *testing.B) {
	benchHTTP(b, golang())
}

func BenchmarkHTTPRoundTripGoParallel(b *testing.B) {
	benchHTTPParallel(b, golang())
}

func benchHTTP(b *testing.B, lang contractLanguage) {
	b.Helper()

	target := benchFetcher(b, lang)

	b.ReportAllocs()

	for b.Loop() {
		_, err := target.plug.Foo(b.Context(), target.cmd)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchHTTPParallel(b *testing.B, lang contractLanguage) {
	b.Helper()

	target := benchFetcher(b, lang)

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
