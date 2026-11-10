package harness

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// fakeAPI is what the fake APIs (Wallet, Vendor) share: an httptest server requiring a bearer token, counting
// requests and refusals, and the faults a test asks for, failing the next requests with 503 (FailNext) or holding
// their answer until the caller gives up (HangNext). Each fake decides at which point of a request it fails or
// holds it. Safe for concurrent use.
type fakeAPI struct {
	server      *httptest.Server
	done        chan struct{} // closed when the test ends, releasing the requests held
	token       string
	requests    atomic.Int64
	refused     atomic.Int64
	failing     atomic.Int64
	hanging     atomic.Int64
	unavailable atomic.Int64
	held        atomic.Int64
}

// URL is the API's base URL.
func (f *fakeAPI) URL() string {
	return f.server.URL
}

// Host is the API's host:port, for an HTTP policy's allowed hosts.
func (f *fakeAPI) Host() string {
	u, _ := url.Parse(f.server.URL) //nolint:errcheck // httptest's URL parses

	return u.Host
}

// FailNext makes the next n requests that reach the fake's failing point fail with 503.
func (f *fakeAPI) FailNext(n int) {
	f.failing.Store(int64(n))
}

// HangNext makes the next n requests that reach the fake's holding point hold their answer until the caller
// gives up.
func (f *fakeAPI) HangNext(n int) {
	f.hanging.Store(int64(n))
}

// Requests is how many requests the API received.
func (f *fakeAPI) Requests() int64 {
	return f.requests.Load()
}

// Refused is how many requests the API refused for a missing or wrong token.
func (f *fakeAPI) Refused() int64 {
	return f.refused.Load()
}

// Unavailable is how many requests the API failed with 503.
func (f *fakeAPI) Unavailable() int64 {
	return f.unavailable.Load()
}

// Held is how many requests the API held until their caller gave up.
func (f *fakeAPI) Held() int64 {
	return f.held.Load()
}

// start serves handler, requiring token, until tb ends.
func (f *fakeAPI) start(tb testing.TB, token string, handler http.Handler) {
	tb.Helper()

	f.token, f.done = token, make(chan struct{})
	f.server = httptest.NewServer(handler)

	tb.Cleanup(func() {
		close(f.done) // releases the requests HangNext holds, so that Close does not wait for them
		f.server.Close()
	})
}

// authorize counts the request and reports whether it carries the bearer token, answering 401 when it does not.
func (f *fakeAPI) authorize(response http.ResponseWriter, request *http.Request) bool {
	f.requests.Add(1)

	if request.Header.Get("Authorization") == "Bearer "+f.token {
		return true
	}

	f.refused.Add(1)
	http.Error(response, "a valid bearer token is required", http.StatusUnauthorized)

	return false
}

// fail answers 503 with message, reporting whether it did: for a request FailNext asks for, or when also (nil for
// none) says so.
func (f *fakeAPI) fail(response http.ResponseWriter, message string, also func() bool) bool {
	if !take(&f.failing) && (also == nil || !also()) {
		return false
	}

	f.unavailable.Add(1)
	http.Error(response, message, http.StatusServiceUnavailable)

	return true
}

// hold holds the request until its caller gives up or the test ends, reporting whether it did: for a request
// HangNext asks for, or when also (nil for none) says so.
func (f *fakeAPI) hold(request *http.Request, also func() bool) bool {
	if !take(&f.hanging) && (also == nil || !also()) {
		return false
	}

	f.held.Add(1)

	select {
	case <-request.Context().Done():
	case <-f.done:
	}

	return true
}

// take takes one from n while it is positive, reporting whether it did.
func take(n *atomic.Int64) bool {
	for {
		left := n.Load()
		if left <= 0 {
			return false
		}

		if n.CompareAndSwap(left, left-1) {
			return true
		}
	}
}

// oneIn counts one more in count and reports whether it is a multiple of n, when n is positive.
func oneIn(n, count *atomic.Int64) bool {
	every := n.Load()

	return every > 0 && count.Add(1)%every == 0
}
