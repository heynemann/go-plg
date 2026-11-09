//go:build integration

package plg_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

// TestAdminHandlerServesLivePluginsAndRunsNoPluginCode serves the admin handler over a real HTTP server for a Lua
// plugin through real loads, on-demand validations and a reload: serving (its load report and pool), degraded by
// a panicking validation (its report's error and the PanicError as lastPanic, counted), then, once a validation
// passed, serving its last good version beside an update whose validation found issues (the rejection's issues
// and rejected version).
// Each answer is the documented JSON form of what Info and Plugins say. Serving GETs and refusing other methods
// runs no plugin code: no validation input is asked for, the pool starts no instance and the instance serves no
// call in between.
func TestAdminHandlerServesLivePluginsAndRunsNoPluginCode(t *testing.T) {
	t.Parallel()

	const key = "live/p"

	ctr := newContainer(t)
	checks := newLiveChecks(map[string]string{key: "valid"})
	store := registerInspected(t, ctr, "live", checks, []string{"p"})

	srv := httptest.NewServer(plg.AdminHandler(ctr))
	t.Cleanup(srv.Close)

	get := func(path string) map[string]any {
		t.Helper()

		answer := requestAdmin(t, srv, http.MethodGet, path)
		defer func() { require.NoError(t, answer.Body.Close()) }()

		require.Equal(t, http.StatusOK, answer.StatusCode, path)
		assertAdminHeaders(t, answer.Header)

		return decodeAdmin(t, answer.Body)
	}
	answersAsInfo := func(msg string) plg.PluginState {
		t.Helper()

		state := inspect(t, ctr, key)
		assert.Equal(t, stateJSON(&state), get("/plugins/"+key), msg)
		assert.Equal(t, map[string]any{"plugins": []any{stateJSON(&state)}}, get("/plugins"), msg)

		return state
	}

	serving := answersAsInfo("serving")
	require.Equal(t, plg.StatusServing, serving.Status)
	require.Equal(t, 1, serving.Pool.Instances)

	checks.set(map[string]string{key: "panic"})

	panicked, err := plg.Validate(t.Context(), ctr, key)
	require.NoError(t, err)
	require.Error(t, panicked.Err)

	degraded := answersAsInfo("degraded by a panicking validation")
	require.Equal(t, plg.StatusDegraded, degraded.Status)
	require.NotNil(t, degraded.LastPanic)
	assert.Equal(t, plg.PhaseCall, degraded.LastPanic.Phase)
	assert.Equal(t, "Validate", degraded.LastPanic.Method)

	checks.set(map[string]string{key: "valid"})

	passed, err := plg.Validate(t.Context(), ctr, key)
	require.NoError(t, err)
	require.NoError(t, passed.Err)

	checks.set(map[string]string{key: "error"})
	putValidator(t, store, luaLanguage(), "p", "2")
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "live"), plg.ErrBlockingIssue)

	lastGood := answersAsInfo("an update rejected")
	require.Equal(t, plg.StatusServingLastGood, lastGood.Status)
	require.NotNil(t, lastGood.Rejection)
	require.Len(t, lastGood.Rejection.Issues, 3, "the contract's local checkout, the validation's error and warning")
	assert.Equal(t, "2", lastGood.RejectedVersion)
	assert.Equal(t, uint64(1), lastGood.Panics, "the key's panic is kept")

	// Reading runs no plugin code: the instance's call count goes on from one call to the next, no validation
	// input is asked for and the pool, reports and panics stay as they were.
	fooer := getFooer(t, ctr, key)
	calls, err := fooer.Count(t.Context())
	require.NoError(t, err)

	before, asked := inspect(t, ctr, key), checks.asked.Load()

	answersAsInfo("read again")

	// The key's events, as Events keeps them: the load, the panicking validation (its PanicError and its
	// report), the passing one and the rejected update, with their reports.
	events := keptEvents(t, ctr, key)
	kinds := make([]plg.EventKind, len(events))

	for i := range events {
		kinds[i] = events[i].Kind
	}

	require.Equal(t, []plg.EventKind{
		plg.EventLoaded, plg.EventPanicked, plg.EventValidated, plg.EventValidated, plg.EventRejected,
	}, kinds)
	assert.Same(t, degraded.LastPanic, events[1].Panic)
	assert.Same(t, lastGood.Rejection, events[4].Report)
	assert.Equal(t, eventsJSON(events), get("/plugins/"+key+"/events"))

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		for _, path := range []string{"/plugins/" + key, "/plugins/" + key + "/events"} {
			answer := requestAdmin(t, srv, method, path)
			require.NoError(t, answer.Body.Close())
			assert.Equal(t, http.StatusMethodNotAllowed, answer.StatusCode, "%s %s", method, path)
		}
	}

	assert.Equal(t, before, inspect(t, ctr, key))
	assert.Equal(t, asked, checks.asked.Load(), "no validation ran")

	next, err := fooer.Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, calls+1, next, "the instance served no call in between")
}

// requestAdmin sends a request with no body to the admin server's path; the caller closes the answer's body.
func requestAdmin(t *testing.T, srv *httptest.Server, method, path string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, http.NoBody)
	require.NoError(t, err)

	answer, err := srv.Client().Do(req)
	require.NoError(t, err)

	return answer
}
