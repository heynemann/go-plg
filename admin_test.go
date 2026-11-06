package plg_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

// TestAdminHandlerDescribesPluginsNoVersionServes registers a plugin in a language its Register does not enable,
// owned by a full owner, then lists a key twice: GET /plugins answers both in key order and GET /plugins/{key}
// each, as the documented JSON shape of what Info says (the rejection's error as its message, times in UTC,
// durations in nanoseconds, every key present, null where nothing serves, empty lists and labels). The key's
// slash may come escaped. None needs a guest.
func TestAdminHandlerDescribesPluginsNoVersionServes(t *testing.T) {
	t.Parallel()

	first, second := plg.NewMemoryStorage("first"), plg.NewMemoryStorage("second")
	full := wantFullOwner()
	put := func(store *plg.MemoryStorage, key string, lang plg.Language, version string, by *plg.Owner) {
		t.Helper()

		require.NoError(t, store.Put(&plg.Descriptor{
			Key: key, Language: lang, Entrypoint: "plugin", Version: version, Owner: by,
		}, fstest.MapFS{"plugin": {}}))
	}

	put(first, "lua", plg.LanguageLua, "3", &full)

	ctr := newUnitContainer(t)
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(first),
		plg.WithStorage(second), plg.WithGolangPlugins()))

	put(first, "dup", plg.LanguageGo, "1", &full)
	put(second, "dup", plg.LanguageGo, "2", nil)
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "ping"), plg.ErrDuplicateKey)

	dup, lua := info(t, ctr, "ping/dup"), info(t, ctr, "ping/lua")
	require.Equal(t, plg.StatusRejected, dup.Status)
	require.NotNil(t, dup.Rejection)
	require.Equal(t, plg.StatusSkipped, lua.Status)

	handler := plg.AdminHandler(ctr)

	body := getAdmin(t, handler, "/plugins", http.StatusOK)
	assert.Equal(t, map[string]any{"plugins": []any{stateJSON(&dup), stateJSON(&lua)}}, body)

	for path, want := range map[string]*plg.PluginState{
		"/plugins/ping/dup": &dup, "/plugins/ping%2Fdup": &dup, "/plugins/ping/lua": &lua,
	} {
		assert.Equal(t, stateJSON(want), getAdmin(t, handler, path, http.StatusOK), path)
	}

	// The shape, spelled out where stateJSON derives it: the rejection of a key listed twice.
	body = getAdmin(t, handler, "/plugins/ping/dup", http.StatusOK)
	rejection, ok := body["rejection"].(map[string]any)
	require.True(t, ok, "rejection: %v", body["rejection"])
	assert.Equal(t, dup.Rejection.Err.Error(), rejection["error"])
	assert.Equal(t, dup.Rejection.At.UTC().Format(time.RFC3339Nano), rejection["at"])
	assert.Equal(t, []any{}, rejection["issues"])
	assert.Equal(t, map[string]any{
		"team": map[string]any{"name": "", "department": ""}, "org": "", "company": "",
		"contact":     map[string]any{"oncall": "", "channel": "", "email": ""},
		"links":       map[string]any{"repository": "", "docs": "", "runbook": "", "issues": ""},
		"maintainers": []any{}, "labels": map[string]any{},
	}, body["owner"], "the second storage's listing names no owner")
	assert.Nil(t, body["pool"])
	assert.Nil(t, body["loadedAt"])
}

// TestAdminHandlerAnswersOnlyGETOnItsPaths: every other method on its two paths answers 405 with Allow: GET, and
// any other path, an unknown key or an empty one answers 404, whatever the method; errors are JSON objects
// holding the message, and no answer may be cached.
func TestAdminHandlerAnswersOnlyGETOnItsPaths(t *testing.T) {
	t.Parallel()

	store := plg.NewMemoryStorage("store")
	require.NoError(t, store.Put(&plg.Descriptor{Key: "lua", Language: plg.LanguageLua, Entrypoint: "plugin"},
		fstest.MapFS{"plugin": {}}))

	ctr := newUnitContainer(t)
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(store),
		plg.WithGolangPlugins()))

	_, notFound := plg.Info(ctr, "ping/none")
	require.ErrorIs(t, notFound, plg.ErrNotFound)

	handler := plg.AdminHandler(ctr)
	tests := map[string]struct {
		method, path string
		message      string // the error's message, when known
		status       int
	}{
		"unknown key": {
			method:  http.MethodGet,
			path:    "/plugins/ping/none",
			status:  http.StatusNotFound,
			message: notFound.Error(),
		},
		"registration name only": {method: http.MethodGet, path: "/plugins/ping", status: http.StatusNotFound},
		"empty key":              {method: http.MethodGet, path: "/plugins/", status: http.StatusNotFound},
		"root":                   {method: http.MethodGet, path: "/", status: http.StatusNotFound},
		"longer name":            {method: http.MethodGet, path: "/pluginsx", status: http.StatusNotFound},
		"under another path":     {method: http.MethodGet, path: "/admin/plugins", status: http.StatusNotFound},
		"POST to another path":   {method: http.MethodPost, path: "/reload", status: http.StatusNotFound},
		"POST /plugins":          {method: http.MethodPost, path: "/plugins", status: http.StatusMethodNotAllowed},
		"PUT /plugins/{key}": {
			method: http.MethodPut,
			path:   "/plugins/ping/lua",
			status: http.StatusMethodNotAllowed,
		},
		"PATCH /plugins/{key}": {
			method: http.MethodPatch,
			path:   "/plugins/ping/lua",
			status: http.StatusMethodNotAllowed,
		},
		"DELETE /plugins/{key}": {
			method: http.MethodDelete,
			path:   "/plugins/ping/lua",
			status: http.StatusMethodNotAllowed,
		},
		"DELETE an unknown key": {
			method: http.MethodDelete,
			path:   "/plugins/ping/none",
			status: http.StatusMethodNotAllowed,
		},
		"HEAD /plugins": {method: http.MethodHead, path: "/plugins", status: http.StatusMethodNotAllowed},
		"OPTIONS /plugins": {
			method: http.MethodOptions,
			path:   "/plugins",
			status: http.StatusMethodNotAllowed,
		},
		"CONNECT /plugins/{key}": {
			method: http.MethodConnect,
			path:   "/plugins/ping/lua",
			status: http.StatusMethodNotAllowed,
		},
		"a method HTTP does not know": {method: "PURGE", path: "/plugins", status: http.StatusMethodNotAllowed},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			answer := serveAdmin(t, handler, tc.method, tc.path)
			assert.Equal(t, tc.status, answer.Code)
			assertAdminHeaders(t, answer.Header())

			if tc.status == http.StatusMethodNotAllowed {
				assert.Equal(t, http.MethodGet, answer.Header().Get("Allow"))
			} else {
				assert.Empty(t, answer.Header().Values("Allow"))
			}

			var body map[string]string

			require.NoError(t, sonic.Unmarshal(answer.Body.Bytes(), &body), answer.Body.String())
			require.Len(t, body, 1, "only the error: %v", body)
			require.Contains(t, body, "error")

			if tc.message != "" {
				assert.Equal(t, tc.message, body["error"])
			}
		})
	}

	assert.Equal(t, []plg.PluginState{info(t, ctr, "ping/lua")}, plg.Plugins(ctr), "nothing changed")
}

// TestAdminHandlerOfAClosedContainer answers 503 with ErrClosed's message on both paths once the container is
// closed, rather than an empty list.
func TestAdminHandlerOfAClosedContainer(t *testing.T) {
	t.Parallel()

	store := plg.NewMemoryStorage("store")
	require.NoError(t, store.Put(&plg.Descriptor{Key: "lua", Language: plg.LanguageLua, Entrypoint: "plugin"},
		fstest.MapFS{"plugin": {}}))

	ctr := plg.New()
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(store),
		plg.WithGolangPlugins()))

	handler := plg.AdminHandler(ctr)
	getAdmin(t, handler, "/plugins/ping/lua", http.StatusOK)
	require.NoError(t, ctr.Close(t.Context()))

	for _, path := range []string{"/plugins", "/plugins/ping/lua"} {
		assert.Equal(t, map[string]any{"error": plg.ErrClosed.Error()},
			getAdmin(t, handler, path, http.StatusServiceUnavailable), path)
	}
}

// TestAdminHandlerLogsAnAnswerItCouldNotWrite: a client gone before its answer is written is logged at Debug with
// the path and the write's error.
func TestAdminHandlerLogsAnAnswerItCouldNotWrite(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := plg.New(plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))

	t.Cleanup(func() { assert.NoError(t, ctr.Close(t.Context())) })

	writer := &goneClient{header: http.Header{}}
	plg.AdminHandler(ctr).ServeHTTP(writer, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/plugins", http.NoBody))

	assert.Equal(t, http.StatusOK, writer.status)

	var record map[string]any

	require.NoError(t, sonic.Unmarshal(logs.Bytes(), &record), logs.String())
	assert.Equal(t, "DEBUG", record["level"])
	assert.Equal(t, "plg: admin answer not written", record["msg"])
	assert.Equal(t, "/plugins", record["path"])
	assert.Equal(t, errClientGone.Error(), record["error"])
}

var errClientGone = errors.New("client gone")

// goneClient is a ResponseWriter whose client went away: every write fails.
type goneClient struct {
	header http.Header
	status int
}

func (c *goneClient) Header() http.Header { return c.header }

func (c *goneClient) WriteHeader(status int) { c.status = status }

func (*goneClient) Write([]byte) (int, error) { return 0, errClientGone }

// serveAdmin answers a request to handler.
func serveAdmin(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()

	answer := httptest.NewRecorder()
	handler.ServeHTTP(answer, httptest.NewRequestWithContext(t.Context(), method, target, http.NoBody))

	return answer
}

// getAdmin GETs path from handler, checks the answer's status and headers and returns its decoded body.
func getAdmin(t *testing.T, handler http.Handler, path string, status int) map[string]any {
	t.Helper()

	answer := serveAdmin(t, handler, http.MethodGet, path)
	require.Equal(t, status, answer.Code, "GET %s: %s", path, answer.Body)
	assertAdminHeaders(t, answer.Header())

	return decodeAdmin(t, answer.Body)
}

// decodeAdmin decodes an admin answer's JSON object, numbers as float64.
func decodeAdmin(t *testing.T, body io.Reader) map[string]any {
	t.Helper()

	var decoded map[string]any

	require.NoError(t, sonic.ConfigDefault.NewDecoder(body).Decode(&decoded))

	return decoded
}

// assertAdminHeaders checks the headers of every admin answer.
func assertAdminHeaders(t *testing.T, header http.Header) {
	t.Helper()

	assert.Equal(t, "application/json", header.Get("Content-Type"))
	assert.Equal(t, "no-store", header.Get("Cache-Control"))
	assert.Equal(t, "nosniff", header.Get("X-Content-Type-Options"))
}

// stateJSON is the documented JSON form of state.
func stateJSON(state *plg.PluginState) map[string]any {
	want := map[string]any{
		"key": state.Key, "registration": state.Registration, "storage": state.Storage,
		"language": string(state.Language), "status": string(state.Status), "version": state.Version,
		"rejectedVersion": state.RejectedVersion, "owner": ownerJSON(&state.Owner), "panics": float64(state.Panics),
		"loadedAt": nil, "pool": nil, "lastPanic": nil, "load": reportJSON(state.Load),
		"validation": reportJSON(state.Validation), "rejection": reportJSON(state.Rejection),
	}

	if state.Load != nil {
		pool := &state.Pool
		want["loadedAt"] = timeJSON(state.LoadedAt)
		want["pool"] = map[string]any{
			"min": float64(pool.Min), "max": float64(pool.Max), "instances": float64(pool.Instances),
			"inUse": float64(pool.InUse), "callsInFlight": float64(pool.CallsInFlight),
			"idleSlots": float64(pool.IdleSlots), "waiting": float64(pool.Waiting),
			"refills": float64(pool.Refills), "grows": float64(pool.Grows),
			"memoryLimitBytes": float64(pool.MemoryLimit), "memoryBytes": float64(pool.Memory),
		}
	}

	failure := state.LastPanic
	if failure != nil {
		want["lastPanic"] = map[string]any{
			"time": timeJSON(failure.Time), "phase": string(failure.Phase), "method": failure.Method,
			"message": failure.Error(), "value": fmt.Sprint(failure.Value), "guest": failure.Guest,
			"stack": string(failure.Stack),
		}
	}

	return want
}

// reportJSON is the documented JSON form of report: null when it is nil.
func reportJSON(report *plg.Report) any {
	if report == nil {
		return nil
	}

	issues := make([]any, 0, len(report.Issues))
	for _, issue := range report.Issues {
		issues = append(issues, map[string]any{
			"severity": string(issue.Severity), "code": issue.Code, "message": issue.Message, "field": issue.Field,
		})
	}

	var msg any

	if report.Err != nil {
		msg = report.Err.Error()
	}

	return map[string]any{
		"key": report.Key, "version": report.Version, "at": timeJSON(report.At),
		"durationNs": float64(report.Duration.Nanoseconds()), "error": msg, "owner": ownerJSON(&report.Owner),
		"issues": issues,
	}
}

// ownerJSON is the documented JSON form of owner.
func ownerJSON(owner *plg.Owner) map[string]any {
	maintainers := make([]any, 0, len(owner.Maintainers))
	for _, m := range owner.Maintainers {
		maintainers = append(maintainers, map[string]any{"name": m.Name, "email": m.Email, "handle": m.Handle})
	}

	labels := map[string]any{}
	for key, value := range owner.Labels {
		labels[key] = value
	}

	contact := &owner.Contact

	return map[string]any{
		"team": map[string]any{"name": owner.Team.Name, "department": owner.Team.Department},
		"org":  owner.Org, "company": owner.Company,
		"contact": map[string]any{"oncall": contact.Oncall, "channel": contact.Channel, "email": contact.Email},
		"links": map[string]any{
			"repository": owner.Links.Repository, "docs": owner.Links.Docs, "runbook": owner.Links.Runbook,
			"issues": owner.Links.Issues,
		},
		"maintainers": maintainers, "labels": labels,
	}
}

// timeJSON is the documented JSON form of a time: RFC 3339 with nanoseconds, in UTC.
func timeJSON(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}
