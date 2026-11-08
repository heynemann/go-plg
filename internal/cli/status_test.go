package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/cli"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// The tokens the environment's mounts of the admin handler require.
const (
	stagingToken = "staging-s3cret"
	prodToken    = "prod-s3cret"
)

// environment is a service holding plugins none of which serves (no guest runs): it mounts plg.AdminHandler on a
// real HTTP server at /staging/ and /prod/, each behind its own bearer token, and at /open/ without one; /forbidden/
// refuses everyone as the service's own authorization would, /broken/ is a failing proxy and /garbled/ answers
// 200 with something else than JSON. home is the user's home directory, where profiles go.
type environment struct {
	ctr  *plg.Container
	srv  *httptest.Server
	home string
}

// searchOwner owns the search plugin.
func searchOwner() plg.Owner {
	return plg.Owner{Team: plg.Team{Name: "search"}, Contact: plg.Contact{Oncall: "search-oncall"}}
}

// billingOwner owns the billing and dup plugins.
func billingOwner() plg.Owner {
	return plg.Owner{
		Team:        plg.Team{Name: "billing", Department: "finance"},
		Contact:     plg.Contact{Channel: "#billing"},
		Maintainers: []plg.Maintainer{{Name: "Ana Lima", Email: "ana@example.com", Handle: "@ana"}},
		Links:       plg.Links{Runbook: "https://runbook.example.com/billing"},
		Labels:      map[string]string{"tier": "1"},
	}
}

// newEnvironment registers, as "ping", plugins the registration skips (it enables Go only): search (Lua),
// billing (TypeScript) and x/events (Lua, without an owner); then a reload finds dup listed twice, rejecting it.
func newEnvironment(t *testing.T) *environment {
	t.Helper()

	first, second := plg.NewMemoryStorage("first"), plg.NewMemoryStorage("second")
	put := func(store *plg.MemoryStorage, key string, lang plg.Language, version string, owner *plg.Owner) {
		t.Helper()

		require.NoError(t, store.Put(&plg.Descriptor{
			Key: key, Implements: "github.com/heynemann/go-plg/internal/testcontract.Fooer@v1.0.0", Language: lang,
			Entrypoint: "plugin", Version: version, Owner: owner,
		}, fstest.MapFS{"plugin": {}}))
	}

	search, billing := searchOwner(), billingOwner()
	put(first, "search", plg.LanguageLua, "3", &search)
	put(first, "billing", plg.LanguageTypeScript, "1", &billing)
	put(first, "x/events", plg.LanguageLua, "", nil)

	ctr := plg.New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "ping", plg.WithStorage(first),
		plg.WithStorage(second), plg.WithGolangPlugins()))

	put(first, "dup", plg.LanguageTypeScript, "1", &billing)
	put(second, "dup", plg.LanguageTypeScript, "2", &billing)
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "ping"), plg.ErrDuplicateKey)

	handler := plg.AdminHandler(ctr)
	mux := http.NewServeMux()
	mux.Handle("/staging/", bearer(stagingToken, http.StripPrefix("/staging", handler)))
	mux.Handle("/prod/", bearer(prodToken, http.StripPrefix("/prod", handler)))
	mux.Handle("/open/", http.StripPrefix("/open", handler))
	mux.HandleFunc("/forbidden/", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusForbidden)

		_, err := writer.Write([]byte(`{"error": "not an admin"}`))
		assert.NoError(t, err)
	})
	mux.HandleFunc("/broken/", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "upstream\tdown\n\x1b[2Jfor now", http.StatusBadGateway)
	})
	mux.HandleFunc("/garbled/", func(writer http.ResponseWriter, _ *http.Request) {
		_, err := writer.Write([]byte("<html>not json</html>"))
		assert.NoError(t, err)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &environment{ctr: ctr, srv: srv, home: t.TempDir()}
}

// bearer lets through only the requests carrying token, as a service's authentication would.
func bearer(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+token {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(writer, req)
	})
}

// profiles is a profiles.yaml naming the environment's mounts: staging (the default, its token in
// PLG_STAGING_TOKEN), prod (its token printed by a command, padded with white space) and open (no token).
func (e *environment) profiles() string {
	return fmt.Sprintf(`default: staging
profiles:
  - name: staging
    url: %[1]s/staging
    token: {env: PLG_STAGING_TOKEN}
  - name: prod
    url: %[1]s/prod/
    token:
      command: [sh, -c, "printf '  %[2]s\n'"]
  - name: open
    url: %[1]s/open
`, e.srv.URL, prodToken)
}

// writeProfiles writes content as the profiles file under dir, the configuration directory.
func writeProfiles(t *testing.T, dir, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "plg"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plg", "profiles.yaml"), []byte(content), 0o600))
}

// ran is how a run of plg ended.
type ran struct {
	stdout, stderr string
	code           int
}

// run runs plg with args in the environment's home, with HOME and PLG_STAGING_TOKEN set as well as vars, and no
// other variable.
func (e *environment) run(args []string, vars map[string]string) ran {
	all := map[string]string{"HOME": e.home, "PLG_STAGING_TOKEN": stagingToken}
	maps.Copy(all, vars)

	environ := make([]string, 0, len(all))
	for name, value := range all {
		environ = append(environ, name+"="+value)
	}

	var stdout, stderr bytes.Buffer

	code := cli.Main(args, &cli.Env{
		Stdout: &stdout, Stderr: &stderr, Environ: func() []string { return environ }, Dir: e.home,
	})

	return ran{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// ok runs plg as run does without more variables, requires it to succeed silently on stderr and returns its
// stdout.
func (e *environment) ok(t *testing.T, args ...string) string {
	t.Helper()

	return e.okWith(t, nil, args...)
}

// okWith is ok with vars.
func (e *environment) okWith(t *testing.T, vars map[string]string, args ...string) string {
	t.Helper()

	res := e.run(args, vars)
	require.Equal(t, 0, res.code, res.stderr)
	require.Empty(t, res.stderr)

	return res.stdout
}

// fails runs plg as run does, requires it to fail with exit code 1 and nothing on stdout, and returns its stderr.
func (e *environment) fails(t *testing.T, vars map[string]string, args ...string) string {
	t.Helper()

	res := e.run(args, vars)
	require.Equal(t, 1, res.code)
	require.Empty(t, res.stdout)

	return res.stderr
}

// answer GETs path from the handler's open mount and decodes its answer into out, as the handler wrote it.
func (e *environment) answer(t *testing.T, path string, out any) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.srv.URL+"/open"+path, http.NoBody)
	require.NoError(t, err)

	res, err := e.srv.Client().Do(req)
	require.NoError(t, err)

	defer func() { assert.NoError(t, res.Body.Close()) }()

	require.Equal(t, http.StatusOK, res.StatusCode)
	require.NoError(t, sonic.ConfigDefault.NewDecoder(res.Body).Decode(out))
}

// decode decodes the JSON plg wrote into out.
func decode(t *testing.T, stdout string, out any) {
	t.Helper()

	require.NoError(t, sonic.ConfigDefault.UnmarshalFromString(stdout, out), stdout)
}

// columnGap separates the columns of a table: cells never hold two spaces in a row.
var columnGap = regexp.MustCompile(` {2,}`)

// cells splits a table into its rows' cells.
func cells(table string) [][]string {
	lines := strings.Split(strings.TrimSuffix(table, "\n"), "\n")
	rows := make([][]string, len(lines))

	for i, line := range lines {
		rows[i] = columnGap.Split(strings.TrimRight(line, " "), -1)
	}

	return rows
}

// pluginRow is the row `plg status` shows for state.
func pluginRow(state *plg.PluginState) []string {
	row := []string{
		state.Key, string(state.Status), orNone(state.Version), orNone(string(state.Language)),
		orNone(state.Owner.Team.Name), strconv.FormatUint(state.Panics, 10), "-",
	}

	if state.Rejection != nil && state.Rejection.Err != nil {
		row[6] = state.Rejection.Err.Error()
	}

	return row
}

// eventRow is the row `plg events` shows for an event of a plugin no version of which serves.
func eventRow(e *plg.Event) []string {
	row := []string{e.At.UTC().Format(time.RFC3339), string(e.Kind), orNone(e.Version), "-"}
	if e.Report != nil && e.Report.Err != nil {
		row[3] = e.Report.Err.Error()
	}

	return row
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// TestStatusListsAnEnvironmentsPlugins lists the plugins of the default profile's environment as a table, one row
// per plugin in key order (its status, version, language, owner team, panics and what keeps it from serving), and
// as JSON, the handler's own answer; -team keeps one team's plugins in both formats. -profile picks another
// environment, flags may follow the arguments and take two dashes.
func TestStatusListsAnEnvironmentsPlugins(t *testing.T) {
	t.Parallel()

	env := newEnvironment(t)
	writeProfiles(t, filepath.Join(env.home, ".config"), env.profiles())

	states := plg.Plugins(env.ctr)
	require.Len(t, states, 4)

	header := []string{"KEY", "STATUS", "VERSION", "LANGUAGE", "TEAM", "PANICS", "PROBLEM"}
	all := make([][]string, 0, 1+len(states))
	all = append(all, header)

	for i := range states {
		all = append(all, pluginRow(&states[i]))
	}

	assert.Equal(t, []string{"ping/dup", "rejected", "-", "typescript", "billing", "0"}, all[2][:6])
	assert.Equal(t, all, cells(env.ok(t, "status")))
	assert.Equal(t, all, cells(env.ok(t, "status", "-profile", "prod")), "the same environment, another mount")

	var want, got adminjson.Plugins

	env.answer(t, "/plugins", &want)
	decode(t, env.ok(t, "status", "--format", "json", "--profile=open"), &got)
	assert.Equal(t, want, got)

	billing := [][]string{header, pluginRow(&states[0]), pluginRow(&states[1])}
	assert.Equal(t, billing, cells(env.ok(t, "status", "-team", "billing")))

	decode(t, env.ok(t, "status", "-format", "json", "-team", "billing"), &got)
	assert.Equal(t, want.Plugins[:2], got.Plugins)

	assert.Equal(t, [][]string{header}, cells(env.ok(t, "status", "-team", "nobody")))
	assert.JSONEq(t, `{"plugins": []}`, env.ok(t, "status", "-team", "nobody", "-format", "json"))
}

// TestStatusDescribesAPlugin shows one plugin as a table of the fields it has, its owner's included, with the
// report that rejected it; as JSON, the handler's own answer. A key whose last part is "events" is that plugin,
// not the events of the key before it. An unknown key fails with the handler's message.
func TestStatusDescribesAPlugin(t *testing.T) {
	t.Parallel()

	env := newEnvironment(t)
	writeProfiles(t, filepath.Join(env.home, ".config"), env.profiles())

	dup, err := plg.Info(env.ctr, "ping/dup")
	require.NoError(t, err)
	require.NotNil(t, dup.Rejection)

	assert.Equal(t, [][]string{
		{"key", "ping/dup"},
		{"registration", "ping"},
		{"status", "rejected"},
		{"version", "-"},
		{"language", "typescript"},
		{"storage", "second"},
		{"team", "billing (finance)"},
		{"channel", "#billing"},
		{"maintainer", "Ana Lima <ana@example.com> @ana"},
		{"runbook", "https://runbook.example.com/billing"},
		{"label", "tier=1"},
		{"panics", "0"},
		{"rejection", dup.Rejection.At.UTC().Format(time.RFC3339) + " version 2: " + dup.Rejection.Err.Error()},
	}, cells(env.ok(t, "status", "ping/dup")))

	var want, got adminjson.Plugin

	env.answer(t, "/plugins/ping/dup", &want)
	decode(t, env.ok(t, "status", "ping/dup", "-format", "json"), &got)
	assert.Equal(t, want, got)

	decode(t, env.ok(t, "status", "-format", "json", "ping/x/events"), &got)
	assert.Equal(t, "ping/x/events", got.Key)
	assert.Equal(t, string(plg.StatusSkipped), got.Status)

	assert.Equal(t, "plg status: profile staging: GET "+env.srv.URL+"/staging/plugins/ping/none: the admin "+
		"handler answered 404 Not Found: plg: plugin not found: ping/none\n", env.fails(t, nil, "status", "ping/none"))
}

// TestEventsShowsAPluginsEvents shows a plugin's events, oldest first, as a table (when, kind, version and what
// the report says) and as JSON, the handler's own answer; a key whose last part is "events" reads its own events.
// An unknown key fails with the handler's message.
func TestEventsShowsAPluginsEvents(t *testing.T) {
	t.Parallel()

	env := newEnvironment(t)
	writeProfiles(t, filepath.Join(env.home, ".config"), env.profiles())

	header := []string{"AT", "KIND", "VERSION", "DETAIL"}

	for _, key := range []string{"ping/search", "ping/dup", "ping/x/events"} {
		events, err := plg.Events(env.ctr, key)
		require.NoError(t, err)

		want := [][]string{header}
		for i := range events {
			want = append(want, eventRow(&events[i]))
		}

		assert.Equal(t, want, cells(env.ok(t, "events", key)), key)
	}

	search, err := plg.Events(env.ctr, "ping/search")
	require.NoError(t, err)
	require.Len(t, search, 2, "skipped at Register and at the reload")
	assert.Equal(t, plg.EventSkipped, search[1].Kind)

	dup := cells(env.ok(t, "events", "ping/dup"))
	require.Len(t, dup, 2)
	assert.Equal(t, []string{"rejected", "2"}, dup[1][1:3])

	var want, got adminjson.Events

	env.answer(t, "/plugins/ping/search/events", &want)
	decode(t, env.ok(t, "events", "ping/search", "-format", "json", "-profile", "prod"), &got)
	assert.Equal(t, want, got)

	assert.Equal(t, "plg events: profile staging: GET "+env.srv.URL+"/staging/plugins/ping/x/events: the admin "+
		"handler answered 404 Not Found: plg: plugin not found: ping/x\n", env.fails(t, nil, "events", "ping/x"))
}

// TestProfilesNameTheEnvironment reads the profiles file from $XDG_CONFIG_HOME/plg when that is absolute, else from
// ~/.config/plg: the profile used is the one -profile names, else the default, else the only one. Each reaches its
// own mount with its own token, from a variable or a command (its output trimmed), or none.
func TestProfilesNameTheEnvironment(t *testing.T) {
	t.Parallel()

	env := newEnvironment(t)
	xdg := t.TempDir()
	only := fmt.Sprintf("profiles:\n  - {name: prod, url: %q, token: {env: PROD}}\n", env.srv.URL+"/prod")

	writeProfiles(t, xdg, env.profiles())
	writeProfiles(t, filepath.Join(env.home, ".config"), only)

	for name, tc := range map[string]struct {
		vars map[string]string
		args []string
	}{
		"the default":          {vars: map[string]string{"XDG_CONFIG_HOME": xdg}},
		"a token's command":    {vars: map[string]string{"XDG_CONFIG_HOME": xdg}, args: []string{"-profile", "prod"}},
		"no token":             {vars: map[string]string{"XDG_CONFIG_HOME": xdg}, args: []string{"-profile", "open"}},
		"the only one, HOME's": {vars: map[string]string{"PROD": prodToken}},
		"a relative XDG_CONFIG_HOME is ignored": {
			vars: map[string]string{"XDG_CONFIG_HOME": "config", "PROD": prodToken},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var got adminjson.Plugins

			decode(t, env.okWith(t, tc.vars, append([]string{"status", "-format", "json"}, tc.args...)...), &got)
			assert.Len(t, got.Plugins, 4)
		})
	}
}

// TestStatusFailures: a profiles file that is missing, malformed or names no usable environment, a token that
// cannot be had, and an admin handler that cannot be reached or refuses (the service's authentication included)
// each fail with exit code 1 and say why on stderr, before anything is shown.
func TestStatusFailures(t *testing.T) {
	t.Parallel()

	env := newEnvironment(t)
	gone := goneServer(t)

	profile := func(url, token string) string {
		return fmt.Sprintf("profiles:\n  - name: p\n    url: %s\n%s", url, token)
	}
	url := func(mount string) string { return env.srv.URL + mount }
	// withSecret is the URL of mount with credentials in it, which no error may show.
	withSecret := func(mount string) string { return strings.Replace(url(mount), "://", "://ana:s3cret@", 1) }
	redacted := func(mount string) string { return strings.Replace(url(mount), "://", "://ana:xxxxx@", 1) }

	tests := map[string]struct {
		vars     map[string]string
		profiles string // the profiles file; none when empty
		contains string // in stderr, $FILE standing for the profiles file's path
		args     []string
	}{
		"no profiles file": {contains: "$FILE does not exist"},
		"an empty file":    {profiles: "\n", contains: "profiles.yaml holds no document"},
		"no profiles":      {profiles: "profiles: []\n", contains: "profiles.yaml holds no profiles"},
		"an unknown key": {
			profiles: profile(url("/open"), "    tokn: {env: X}\n"),
			contains: "field tokn not found",
		},
		"a duplicate key": {
			profiles: profile(url("/open"), "    url: "+url("/open")+"\n"),
			contains: `duplicate key "url"`,
		},
		"an anchor": {
			profiles: "profiles:\n  - &p {name: p, url: " + url("/open") + "}\n",
			contains: "anchors are not allowed",
		},
		"a profile without a name": {
			profiles: "profiles:\n  - {url: " + url("/open") + "}\n",
			contains: "a profile needs a name",
		},
		"a name twice": {
			profiles: "profiles:\n  - {name: p, url: " + url("/open") + "}\n  - {name: p, url: " + url("/open") + "}\n",
			contains: `"p" is named by two profiles`,
		},
		"a URL without a scheme": {
			profiles: profile("staging.example.com/admin", ""),
			contains: `profile p: url must be an absolute http or https URL, not "staging.example.com/admin"`,
		},
		"an FTP URL":           {profiles: profile("ftp://example.com", ""), contains: "url must be an absolute http"},
		"a URL without a host": {profiles: profile("http://", ""), contains: "url must be an absolute http"},
		"an FTP URL with credentials": { //nolint:gosec // G101: a test's fake password
			profiles: profile("ftp://ana:s3cret@example.com", ""),
			contains: `url must be an absolute http or https URL, not "ftp://ana:xxxxx@example.com"`,
		},
		"an unparsable URL with credentials": {
			profiles: profile("http://ana:s3cret@ex%zz.com", ""),
			contains: "profile p: url must be an absolute http or https URL, not a URL\n",
		},
		"a refusal from a URL with credentials": {
			profiles: profile(withSecret("/staging"), ""),
			contains: "GET " + redacted("/staging/plugins") + ": the admin handler answered 401 Unauthorized",
		},
		"an answer that is not JSON from a URL with credentials": {
			profiles: profile(withSecret("/garbled"), ""),
			contains: "GET " + redacted("/garbled/plugins") + ": reading the answer: ",
		},
		"a server that is gone at a URL with credentials": {
			profiles: profile(strings.Replace(gone.URL, "://", "://ana:s3cret@", 1), ""),
			contains: `profile p: Get "` + strings.Replace(gone.URL, "://", "://ana:***@", 1) + `/plugins": dial tcp`,
		},
		"a token from two sources": {
			profiles: profile(url("/open"), "    token: {env: X, command: [echo, x]}\n"),
			contains: "profile p: token needs exactly one of env and command",
		},
		"a token from nowhere": {
			profiles: profile(url("/open"), "    token: {}\n"),
			contains: "profile p: token needs exactly one of env and command",
		},
		"a default naming no profile": {
			profiles: "default: qa\n" + profile(url("/open"), ""),
			contains: `default names no profile "qa"`,
		},
		"several profiles and no default": {
			profiles: strings.Replace(env.profiles(), "default: staging\n", "", 1),
			contains: "name a profile with -profile ($FILE has staging, prod, open and no default)",
		},
		"an unknown profile": {
			profiles: env.profiles(), args: []string{"-profile", "qa"},
			contains: `no such profile "qa" in $FILE (it has staging, prod, open)`,
		},
		"an oversized file": {
			profiles: profile(url("/open"), "") + "# " + strings.Repeat("x", 1<<20) + "\n",
			contains: "profiles.yaml is over 1048576 bytes",
		},
		"a token variable unset": {
			profiles: profile(url("/staging"), "    token: {env: PLG_NOPE}\n"),
			contains: "profile p: the token's variable PLG_NOPE is not set",
		},
		"a failing token command": {
			profiles: profile(url("/staging"), "    token: {command: [sh, -c, 'echo denied >&2; exit 3']}\n"),
			contains: `profile p: token command failed: ["sh" "-c" "echo denied >&2; exit 3"]: exit status 3: denied`,
		},
		"a token command printing nothing": {
			profiles: profile(url("/staging"), "    token: {command: [sh, -c, 'printf \"\\n\"']}\n"),
			contains: "profile p: token command printed no token",
		},
		"a missing token command": {
			profiles: profile(url("/staging"), "    token: {command: [plg-no-such-command]}\n"),
			contains: `token command failed: ["plg-no-such-command"]: exec: "plg-no-such-command"`,
		},
		"no token": {
			profiles: profile(url("/staging"), ""),
			contains: "GET " + url("/staging/plugins") + ": the admin handler answered 401 Unauthorized: " +
				"unauthorized (check the profile's token)\n",
		},
		"a wrong token": {
			profiles: profile(url("/staging"), "    token: {env: PLG_STAGING_TOKEN}\n"),
			vars:     map[string]string{"PLG_STAGING_TOKEN": prodToken},
			contains: "answered 401 Unauthorized: unauthorized (check the profile's token)\n",
		},
		"forbidden": {
			profiles: profile(url("/forbidden"), ""),
			contains: "answered 403 Forbidden: not an admin (check the profile's token)\n",
		},
		"a failing proxy": {
			profiles: profile(url("/broken"), ""),
			contains: "answered 502 Bad Gateway: upstream down \ufffd[2Jfor now\n",
		},
		"an answer that is not JSON": {
			profiles: profile(url("/garbled"), ""),
			contains: "GET " + url("/garbled/plugins") + ": reading the answer: ",
		},
		"a server that is gone": {
			profiles: profile(gone.URL, ""),
			contains: `profile p: Get "` + gone.URL + `/plugins": dial tcp`,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			if tc.profiles != "" {
				writeProfiles(t, filepath.Join(home, ".config"), tc.profiles)
			}

			vars := map[string]string{"HOME": home}
			maps.Copy(vars, tc.vars)

			stderr := env.fails(t, vars, append([]string{"status"}, tc.args...)...)
			assert.True(t, strings.HasPrefix(stderr, "plg status: "), stderr)
			assert.Contains(t, stderr,
				strings.ReplaceAll(tc.contains, "$FILE", filepath.Join(home, ".config", "plg", "profiles.yaml")))
			assert.NotContains(t, stderr, "s3cret", "no error shows a URL's password")
		})
	}
}

// TestStatusFollowsNoRedirect: an admin URL answering with a redirect, here to another port of the same host, where
// Go's client would carry the bearer token, fails plg status and plg events with the redirect's status and target,
// and the token never reaches the target; a redirect without a target fails likewise.
func TestStatusFollowsNoRedirect(t *testing.T) {
	t.Parallel()

	var reached atomic.Int32 // requests the redirect's target got

	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		reached.Add(1)
		assert.Empty(t, req.Header.Get("Authorization"), "the token never reaches the redirect's target")
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	redirecting := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/nowhere/") {
			writer.WriteHeader(http.StatusMultipleChoices)

			return
		}

		writer.Header().Set("Location", target.URL+req.URL.Path)
		writer.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)

	env := &environment{srv: redirecting, home: t.TempDir()}
	profiles := func(mount string) string {
		return "profiles:\n  - name: p\n    url: " + redirecting.URL + mount + "\n    token: {env: PLG_STAGING_TOKEN}\n"
	}

	const advice = " (plg follows no redirect, which would carry the token: set the profile's url to where the " +
		"handler is)\n"

	writeProfiles(t, filepath.Join(env.home, ".config"), profiles("/admin"))
	assert.Equal(t, "plg status: profile p: GET "+redirecting.URL+"/admin/plugins: the admin handler answered 302 "+
		"Found to "+target.URL+"/admin/plugins"+advice, env.fails(t, nil, "status"))
	assert.Equal(t, "plg events: profile p: GET "+redirecting.URL+"/admin/plugins/ping/search/events: the admin "+
		"handler answered 302 Found to "+target.URL+"/admin/plugins/ping/search/events"+advice,
		env.fails(t, nil, "events", "ping/search"))

	writeProfiles(t, filepath.Join(env.home, ".config"), profiles("/nowhere"))
	assert.Equal(t, "plg status: profile p: GET "+redirecting.URL+"/nowhere/plugins: the admin handler answered "+
		"300 Multiple Choices"+advice, env.fails(t, nil, "status"))

	assert.Zero(t, reached.Load(), "plg follows no redirect")
}

// TestStatusOfAClosedContainer reports the handler's 503 once the service closed its container.
func TestStatusOfAClosedContainer(t *testing.T) {
	t.Parallel()

	env := newEnvironment(t)
	writeProfiles(t, filepath.Join(env.home, ".config"), env.profiles())
	require.NoError(t, env.ctr.Close(t.Context()))

	assert.Equal(t, "plg events: profile staging: GET "+env.srv.URL+"/staging/plugins/ping/search/events: the "+
		"admin handler answered 503 Service Unavailable: plg: container closed\n",
		env.fails(t, nil, "events", "ping/search"))
}

// gone is a server that is gone: its URL's port is held by a socket the test owns, bound but not listening, so
// connecting to it is refused for as long as the test runs (a closed server's freed port could be taken by
// another process meanwhile, answering instead of refusing).
type gone struct{ URL string }

func goneServer(t *testing.T) gone {
	t.Helper()

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, syscall.Close(fd)) })

	require.NoError(t, syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}))

	addr, err := syscall.Getsockname(fd)
	require.NoError(t, err)

	inet, ok := addr.(*syscall.SockaddrInet4)
	require.True(t, ok)

	return gone{URL: "http://127.0.0.1:" + strconv.Itoa(inet.Port)}
}
