package harness

import (
	"io"
	"net/http"
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
// the caller gives up (HangNext), past the deliverer's timeout, and to do either to a steady fraction of its
// requests (FailOneIn, HangOneIn), as a loaded API does. Safe for concurrent use.
type Wallet struct {
	*fakeAPI

	credits   *xsync.Map[string, applied]
	repeats   atomic.Int64
	ids       atomic.Int64
	failOneIn atomic.Int64
	hangOneIn atomic.Int64
	failCount atomic.Int64 // the requests FailOneIn counts
	hangCount atomic.Int64 // the requests HangOneIn counts
}

// NewWallet starts a wallet requiring token, closed when tb ends.
func NewWallet(tb testing.TB, token string) *Wallet {
	tb.Helper()

	wallet := &Wallet{fakeAPI: &fakeAPI{}, credits: xsync.NewMap[string, applied]()}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /credits", wallet.credit)

	wallet.start(tb, token, mux)

	return wallet
}

// FailOneIn makes one request in every n fail with 503 without applying anything (the n-th, the 2n-th, … from
// now), until it is set to 0: the steady trickle of failures a loaded API answers, which retries absorb.
func (w *Wallet) FailOneIn(n int) {
	w.failCount.Store(0)
	w.failOneIn.Store(int64(n))
}

// HangOneIn makes one in every n requests that apply a credit (or find it applied) hold its answer until the
// caller gives up, as HangNext does (the n-th, the 2n-th, … from now), until it is set to 0.
func (w *Wallet) HangOneIn(n int) {
	w.hangCount.Store(0)
	w.hangOneIn.Store(int64(n))
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

// Forget drops the credits applied for the campaign of the ID, once a test has checked them, so that a long test's
// wallet holds only the campaigns it has not checked yet. A repeat of a forgotten key is applied anew.
func (w *Wallet) Forget(campaign string) {
	w.credits.DeleteMatching(func(_ string, credit applied) (bool, bool) {
		return credit.credit.Campaign == campaign, false
	})
}

// Traceparent returns the traceparent header of the request that applied the credit of key ("" for none).
func (w *Wallet) Traceparent(key string) string {
	credit, _ := w.credits.Load(key)

	return credit.traceparent
}

// Repeats is how many requests repeated a key already applied, and were answered without applying it again.
func (w *Wallet) Repeats() int64 {
	return w.repeats.Load()
}

func (w *Wallet) credit(response http.ResponseWriter, request *http.Request) {
	if !w.authorize(response, request) {
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

	if w.fail(response, "wallet unavailable", func() bool { return oneIn(&w.failOneIn, &w.failCount) }) {
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

	if w.hold(request, func() bool { return oneIn(&w.hangOneIn, &w.hangCount) }) {
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
