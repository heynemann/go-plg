package harness

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/examples/authorrepo"
	"github.com/heynemann/go-plg/internal/adminjson"
)

// AdminToken is the bearer token the admin endpoint of NewAdmin requires, which its profile reads from the
// environment.
const AdminToken = "adm-5Rk2pQ8vN1" //nolint:gosec // G101: the test admin endpoint's made-up token

// adminTokenEnv is the environment variable the profile reads the token from.
const adminTokenEnv = "PLG_REWARDS_ADMIN_TOKEN" //nolint:gosec // G101: a variable's name, not its value

// Admin is a platform's admin handler (platform.AdminHandler) served over HTTP behind a bearer token, as a platform
// mounts it behind its own authentication, and the plg command built from this checkout reading it through the
// only profile of its profiles.yaml, as a plugin author runs plg status and plg events against an environment.
type Admin struct {
	// URL is where the handler is served.
	URL     string
	bin     string
	environ []string
}

// NewAdmin serves handler behind AdminToken until tb ends, builds plg and writes the profile naming the endpoint.
func NewAdmin(tb testing.TB, handler http.Handler) *Admin {
	tb.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+AdminToken {
			http.Error(response, "a valid bearer token is required", http.StatusUnauthorized)

			return
		}

		handler.ServeHTTP(response, request)
	}))
	tb.Cleanup(server.Close)

	config := tb.TempDir()
	require.NoError(tb, os.MkdirAll(filepath.Join(config, "plg"), dirMode))
	require.NoError(tb, os.WriteFile(filepath.Join(config, "plg", "profiles.yaml"), fmt.Appendf(nil,
		"profiles:\n  - {name: rewards, url: %q, token: {env: %s}}\n", server.URL, adminTokenEnv), fileMode))

	return &Admin{
		URL:     server.URL,
		bin:     filepath.Join(authorrepo.Command(tb), "plg"),
		environ: append(os.Environ(), "XDG_CONFIG_HOME="+config, adminTokenEnv+"="+AdminToken),
	}
}

// Plg runs plg with args against the endpoint, requires it to succeed with nothing on stderr, and returns what
// it printed.
func (a *Admin) Plg(tb testing.TB, args ...string) string {
	tb.Helper()

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(tb.Context(), a.bin, args...) //nolint:gosec // G204: this checkout's plg
	cmd.Env = a.environ
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(tb, cmd.Run(), "plg %s: %s", strings.Join(args, " "), stderr.String())
	require.Empty(tb, stderr.String(), "plg %s", strings.Join(args, " "))

	return stdout.String()
}

// cellGap is what separates the cells of plg's tables.
var cellGap = regexp.MustCompile(" {2,}")

// Table runs plg with args (status or events, printing a table) and returns its rows, the header first, each cut
// into the header's cells: the last cell keeps whatever it holds.
func (a *Admin) Table(tb testing.TB, args ...string) [][]string {
	tb.Helper()

	lines := strings.Split(strings.TrimSuffix(a.Plg(tb, args...), "\n"), "\n")
	columns := len(cellGap.Split(lines[0], -1))
	rows := make([][]string, 0, len(lines))

	for _, line := range lines {
		rows = append(rows, cellGap.Split(strings.TrimRight(line, " "), columns))
	}

	return rows
}

// Plugins runs plg status -format json with args (none, or -team) and returns the plugins it printed.
func (a *Admin) Plugins(tb testing.TB, args ...string) []adminjson.Plugin {
	tb.Helper()

	var plugins adminjson.Plugins

	decode(tb, a.Plg(tb, append([]string{"status", "-format", "json"}, args...)...), &plugins)

	return plugins.Plugins
}

// Plugin runs plg status -format json for the plugin key and returns what it printed.
func (a *Admin) Plugin(tb testing.TB, key string) adminjson.Plugin {
	tb.Helper()

	var plugin adminjson.Plugin

	decode(tb, a.Plg(tb, "status", "-format", "json", key), &plugin)

	return plugin
}

// Events runs plg events -format json for the plugin key and returns the events it printed, oldest first.
func (a *Admin) Events(tb testing.TB, key string) []adminjson.Event {
	tb.Helper()

	var events adminjson.Events

	decode(tb, a.Plg(tb, "events", "-format", "json", key), &events)

	return events.Events
}

// decode decodes the JSON plg printed into v.
func decode(tb testing.TB, printed string, v any) {
	tb.Helper()

	require.NoError(tb, sonic.ConfigStd.UnmarshalFromString(printed, v), printed)
}
