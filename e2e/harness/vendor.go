package harness

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/bytedance/sonic"
)

// ChurnRisk is the vendor's segment of the users likely to churn, which the CRM team's churn-risk cohort asks for.
const ChurnRisk = "churn-risk"

// vendorPageSize is how many members a page of the vendor's segment holds.
const vendorPageSize = 100

// Vendor is a fake of the CRM vendor's API the CRM team's churn-risk cohort provider calls, with the bearer token:
// GET /segments/{segment}/members?cursor=<id> answers a page of the segment's members after cursor
// ({"users": [...], "next": "<id>"}, next empty on the last page), and GET /segments/{segment}/members/{user}
// 200 for a member and 404 for anyone else. Its churn-risk segment is a directory's users with ChurnRisk. Safe for
// concurrent use.
type Vendor struct {
	server   *httptest.Server
	segments map[string][]string // by name: the members' IDs, sorted
	token    string
	requests atomic.Int64
	refused  atomic.Int64
}

// NewVendor starts a vendor requiring token, whose churn-risk segment is dir's, closed when tb ends.
func NewVendor(tb testing.TB, token string, dir *Directory) *Vendor {
	tb.Helper()

	var churn []string

	for _, user := range dir.Users() {
		if user.ChurnRisk {
			churn = append(churn, user.ID)
		}
	}

	vendor := &Vendor{token: token, segments: map[string][]string{ChurnRisk: churn}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /segments/{segment}/members", vendor.page)
	mux.HandleFunc("GET /segments/{segment}/members/{user}", vendor.member)

	vendor.server = httptest.NewServer(vendor.authorized(mux))
	tb.Cleanup(vendor.server.Close)

	return vendor
}

// URL is the vendor's base URL, the secret vendor_api_url.
func (v *Vendor) URL() string {
	return v.server.URL
}

// Host is the vendor's host:port, for an HTTP policy's allowed hosts.
func (v *Vendor) Host() string {
	u, _ := url.Parse(v.server.URL) //nolint:errcheck // httptest's URL parses

	return u.Host
}

// Requests is how many requests the vendor received.
func (v *Vendor) Requests() int64 {
	return v.requests.Load()
}

// Refused is how many requests the vendor refused for a missing or wrong token.
func (v *Vendor) Refused() int64 {
	return v.refused.Load()
}

// authorized counts each request and refuses one without the bearer token.
func (v *Vendor) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		v.requests.Add(1)

		if request.Header.Get("Authorization") != "Bearer "+v.token {
			v.refused.Add(1)
			http.Error(response, "a valid bearer token is required", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(response, request)
	})
}

func (v *Vendor) page(response http.ResponseWriter, request *http.Request) {
	members, ok := v.segments[request.PathValue("segment")]
	if !ok {
		http.NotFound(response, request)

		return
	}

	cursor := request.URL.Query().Get("cursor")
	start := sort.SearchStrings(members, cursor)

	if start < len(members) && members[start] == cursor {
		start++
	}

	end := min(start+vendorPageSize, len(members))
	page := struct {
		Next  string   `json:"next"`
		Users []string `json:"users"`
	}{Users: append([]string{}, members[start:end]...)}

	if end < len(members) {
		page.Next = members[end-1]
	}

	answer, err := sonic.Marshal(page)
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)

		return
	}

	response.Header().Set("Content-Type", "application/json")

	_, _ = response.Write(answer) //nolint:errcheck // the provider reports a cut answer
}

func (v *Vendor) member(response http.ResponseWriter, request *http.Request) {
	members := v.segments[request.PathValue("segment")]
	user := request.PathValue("user")

	i := sort.SearchStrings(members, user)
	if i == len(members) || members[i] != user {
		http.NotFound(response, request)

		return
	}

	response.Header().Set("Content-Type", "application/json")

	_, _ = response.Write([]byte(`{"member":true}`)) //nolint:errcheck // the provider reports a cut answer
}
