package harness

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/puzpuzpuz/xsync/v4"
)

// Credit is what the wallet applied for one idempotency key.
type Credit struct {
	User     string `json:"user"`
	Campaign string `json:"campaign"`
	Memo     string `json:"memo"`
	Points   int64  `json:"points"`
}

// applied is a credit the wallet applied, with what it answers a repeat of its key and how it was asked.
type applied struct {
	traceparent string
	id          string
	credit      Credit
}

// Wallet is a fake of the wallet API the payments team's deliverer calls: POST /credits with the bearer token,
// the reward's Idempotency-Key header and a JSON Credit. It applies each key once (201 and the credit's id) and
// answers a repeat with 200 and the first credit's id, so exactly-once crediting is observable in Credits. It can
// be told to fail the next requests with 503 (FailNext) or to apply the next ones and then hold the answer until
// the caller gives up (HangNext), past the deliverer's timeout. Safe for concurrent use.
type Wallet struct {
	server   *httptest.Server
	credits  *xsync.Map[string, applied]
	done     chan struct{}
	token    string
	requests atomic.Int64
	repeats  atomic.Int64
	refused  atomic.Int64
	failing  atomic.Int64
	hanging  atomic.Int64
	ids      atomic.Int64
}

// NewWallet starts a wallet requiring token, closed when tb ends.
func NewWallet(tb testing.TB, token string) *Wallet {
	tb.Helper()

	wallet := &Wallet{credits: xsync.NewMap[string, applied](), token: token, done: make(chan struct{})}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /credits", wallet.credit)

	wallet.server = httptest.NewServer(mux)

	tb.Cleanup(func() {
		close(wallet.done) // releases the requests HangNext holds, so that Close does not wait for them
		wallet.server.Close()
	})

	return wallet
}

// URL is the wallet's base URL, the platform's WalletURL.
func (w *Wallet) URL() string {
	return w.server.URL
}

// Host is the wallet's host:port, for an HTTP policy's allowed hosts.
func (w *Wallet) Host() string {
	u, _ := url.Parse(w.server.URL) //nolint:errcheck // httptest's URL parses

	return u.Host
}

// FailNext makes the next n requests fail with 503 without applying anything.
func (w *Wallet) FailNext(n int) {
	w.failing.Store(int64(n))
}

// HangNext makes the next n requests that apply a credit (or find it applied) hold their answer until the caller
// gives up: the credit is applied, as when a deliverer's deadline cuts a request the wallet served.
func (w *Wallet) HangNext(n int) {
	w.hanging.Store(int64(n))
}

// Credits returns the credits applied, by idempotency key.
func (w *Wallet) Credits() map[string]Credit {
	credits := make(map[string]Credit, w.credits.Size())

	for key, credit := range w.credits.All() {
		credits[key] = credit.credit
	}

	return credits
}

// CreditsOf returns the credits applied for the campaign of the ID, by idempotency key.
func (w *Wallet) CreditsOf(campaign string) map[string]Credit {
	credits := map[string]Credit{}

	for key, credit := range w.credits.All() {
		if credit.credit.Campaign == campaign {
			credits[key] = credit.credit
		}
	}

	return credits
}

// Traceparent returns the traceparent header of the request that applied the credit of key ("" for none).
func (w *Wallet) Traceparent(key string) string {
	credit, _ := w.credits.Load(key)

	return credit.traceparent
}

// Requests is how many requests the wallet received.
func (w *Wallet) Requests() int64 {
	return w.requests.Load()
}

// Repeats is how many requests repeated a key already applied, and were answered without applying it again.
func (w *Wallet) Repeats() int64 {
	return w.repeats.Load()
}

// Refused is how many requests the wallet refused for a missing or wrong token.
func (w *Wallet) Refused() int64 {
	return w.refused.Load()
}

func (w *Wallet) credit(response http.ResponseWriter, request *http.Request) {
	w.requests.Add(1)

	if request.Header.Get("Authorization") != "Bearer "+w.token {
		w.refused.Add(1)
		http.Error(response, "a valid bearer token is required", http.StatusUnauthorized)

		return
	}

	key := request.Header.Get("Idempotency-Key")
	if key == "" {
		http.Error(response, "an Idempotency-Key is required", http.StatusBadRequest)

		return
	}

	var credit Credit

	body, err := io.ReadAll(request.Body)
	if err == nil {
		err = sonic.Unmarshal(body, &credit)
	}

	if err != nil {
		http.Error(response, "a JSON credit is required", http.StatusBadRequest)

		return
	}

	if take(&w.failing) {
		http.Error(response, "wallet unavailable", http.StatusServiceUnavailable)

		return
	}

	w.apply(response, request, key, credit)
}

// apply applies credit under key unless a credit was, and answers with its id.
func (w *Wallet) apply(response http.ResponseWriter, request *http.Request, key string, credit Credit) {
	first, repeated := w.credits.LoadOrCompute(key, func() (applied, bool) {
		return applied{
			credit: credit, traceparent: request.Header.Get("Traceparent"),
			id: "c-" + strconv.FormatInt(w.ids.Add(1), 10),
		}, false
	})

	status := http.StatusCreated

	if repeated {
		w.repeats.Add(1)

		status = http.StatusOK
	}

	if take(&w.hanging) {
		select {
		case <-request.Context().Done():
		case <-w.done:
		}

		return
	}

	answer, err := sonic.Marshal(map[string]string{"id": first.id})
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)

		return
	}

	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)

	_, _ = response.Write(answer) //nolint:errcheck // the deliverer reports a cut answer
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
