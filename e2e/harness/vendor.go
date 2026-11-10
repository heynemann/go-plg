package harness

import (
	"net/http"
	"sort"
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
// 200 for a member and 404 for anyone else. Its churn-risk segment is a directory's users with ChurnRisk. Like the
// wallet, it can be told to fail the next requests with 503 (FailNext) or to hold the next ones' answer until the
// caller gives up (HangNext). Safe for concurrent use.
type Vendor struct {
	*fakeAPI

	segments map[string][]string // by name: the members' IDs, sorted
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

	vendor := &Vendor{fakeAPI: &fakeAPI{}, segments: map[string][]string{ChurnRisk: churn}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /segments/{segment}/members", vendor.page)
	mux.HandleFunc("GET /segments/{segment}/members/{user}", vendor.member)

	vendor.start(tb, token, vendor.authorized(mux))

	return vendor
}

// authorized counts each request, refuses one without the bearer token, and fails or holds those FailNext and
// HangNext ask for.
func (v *Vendor) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if v.authorize(response, request) && !v.fail(response, "vendor unavailable", nil) && !v.hold(request, nil) {
			next.ServeHTTP(response, request)
		}
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
