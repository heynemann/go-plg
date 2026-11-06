//go:build integration

package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/examples/plugins"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var servingLine = regexp.MustCompile(`msg=serving addr=(\S+)`)

// serviceLog is a running service's log, read by [watch].
type serviceLog struct {
	// rest receives the lines after the one saying the service serves; it is closed when the log ends.
	rest <-chan string
	// url is the service's base URL.
	url string
}

// watch reads the service's log until the line saying it serves, and keeps draining it afterwards (so the
// service never blocks on its log).
func watch(t *testing.T, log io.Reader) serviceLog {
	t.Helper()

	lines := make(chan string, 1024)
	scanner := bufio.NewScanner(log)

	for scanner.Scan() {
		if match := servingLine.FindStringSubmatch(scanner.Text()); match != nil {
			go func() {
				defer close(lines)

				for scanner.Scan() {
					select {
					case lines <- scanner.Text():
					default: // a full buffer only drops lines
					}
				}
			}()

			return serviceLog{url: "http://" + match[1], rest: lines}
		}

		t.Log(scanner.Text())
	}

	require.FailNow(t, "the service exited before serving")

	return serviceLog{}
}

// start runs the service in this process with args (always on a free loopback port) and returns its URL. When
// the test ends, the service is stopped and must exit cleanly.
func start(t *testing.T, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	logR, logW := io.Pipe()
	exited := make(chan int, 1)

	go func() {
		exited <- run(ctx, append([]string{"-addr", "127.0.0.1:0"}, args...), logW)

		assert.NoError(t, logW.Close())
	}()

	log := watch(t, logR)

	t.Cleanup(func() {
		cancel()

		for range log.rest { // drains the log until the service exits
		}

		assert.Equal(t, exitOK, <-exited, "a clean shutdown")
	})

	return log.url
}

// answer is the service's answer to a request.
type answer struct {
	body   string
	status int
}

// post sends body to the service's /quote.
func post(t *testing.T, url, body string) answer {
	t.Helper()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/quote", strings.NewReader(body))
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	return answer{status: resp.StatusCode, body: string(data)}
}

// quoteFor posts an order and decodes the quote.
func quoteFor(t *testing.T, url, order string) quote {
	t.Helper()

	got := post(t, url, order)
	require.Equal(t, http.StatusOK, got.status, got.body)

	var decoded quote

	require.NoError(t, sonic.UnmarshalString(got.body, &decoded))

	return decoded
}

func TestServicePricesWithEveryRule(t *testing.T) {
	t.Parallel()

	url := start(t)

	tests := map[string]struct {
		order string
		want  quote
	}{
		"every rule applies, the largest discount wins": {
			order: `{"customer":"ada","coupon":"WELCOME5","items":[{"sku":"tea","quantity":12,"unitCents":450}]}`,
			want: quote{
				Applied: "pricing/bulk", SubtotalCents: 5400, TotalCents: 4860,
				Discounts: []ruleResult{
					{Rule: "pricing/bulk", Cents: 540, Reason: "12 items: 10% off"},
					{Rule: "pricing/coupon", Cents: 500, Reason: "coupon WELCOME5"},
					{Rule: "pricing/loyalty", Cents: 270, Reason: "gold customer: 5% off"},
				},
			},
		},
		"the Lua rule asks the host for the customer's tier": {
			order: `{"customer":"bob","items":[{"sku":"pot","quantity":1,"unitCents":1000}]}`,
			want: quote{
				Applied: "pricing/loyalty", SubtotalCents: 1000, TotalCents: 980,
				Discounts: []ruleResult{
					{Rule: "pricing/bulk"},
					{Rule: "pricing/coupon"},
					{Rule: "pricing/loyalty", Cents: 20, Reason: "silver customer: 2% off"},
				},
			},
		},
		"a coupon never takes off more than the subtotal": {
			order: `{"customer":"eve","coupon":"SPRING10","items":[{"sku":"cup","quantity":2,"unitCents":300}]}`,
			want: quote{
				Applied: "pricing/coupon", SubtotalCents: 600, TotalCents: 0,
				Discounts: []ruleResult{
					{Rule: "pricing/bulk"},
					{Rule: "pricing/coupon", Cents: 600, Reason: "coupon SPRING10"},
					{Rule: "pricing/loyalty"},
				},
			},
		},
		"a failing rule is reported with the plugin's own error": {
			order: `{"customer":"eve","coupon":"toString","items":[{"sku":"cup","quantity":1,"unitCents":300}]}`,
			want: quote{
				SubtotalCents: 300, TotalCents: 300,
				Discounts: []ruleResult{
					{Rule: "pricing/bulk"},
					{Rule: "pricing/coupon", Error: "unknown coupon toString"},
					{Rule: "pricing/loyalty"},
				},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, quoteFor(t, url, tc.order))
		})
	}
}

func TestServiceRejectsInvalidOrders(t *testing.T) {
	t.Parallel()

	url := start(t)

	tests := map[string]string{
		"malformed JSON": `{"customer":`,
		"unknown field":  `{"customer":"ada","discount":100,"items":[{"sku":"a","quantity":1,"unitCents":1}]}`,
		"no items":       `{"customer":"ada","items":[]}`,
		"null items":     `{"customer":"ada","items":null}`,
		"zero quantity":  `{"items":[{"sku":"a","quantity":0,"unitCents":1}]}`,
		"too many":       `{"items":[{"sku":"a","quantity":1000001,"unitCents":1}]}`,
		"negative price": `{"items":[{"sku":"a","quantity":1,"unitCents":-1}]}`,
		"price too high": `{"items":[{"sku":"a","quantity":1,"unitCents":1000000001}]}`,
		"subtotal too big": `{"items":[` + strings.Repeat(
			`{"sku":"a","quantity":1000000,"unitCents":1000000000},`,
			2,
		) + `{"sku":"b","quantity":1,"unitCents":1}]}`,
		"too many lines": `{"items":[` + strings.Repeat(
			`{"sku":"a","quantity":1,"unitCents":1},`,
			1000,
		) + `{"sku":"b","quantity":1,"unitCents":1}]}`,
		"body too large": `{"customer":"` + strings.Repeat("a", 1<<20) + `"}`,
	}

	for name, order := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := post(t, url, order)
			assert.Equal(t, http.StatusBadRequest, got.status, got.body)
		})
	}

	t.Run("only POST", func(t *testing.T) {
		t.Parallel()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/quote", http.NoBody)
		require.NoError(t, err)

		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})
}

// copyPlugins writes the embedded plugins, but those in skip, into a fresh directory.
func copyPlugins(t *testing.T, skip ...string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, plugins.FS))

	for _, name := range skip {
		require.NoError(t, os.RemoveAll(filepath.Join(dir, name)))
	}

	return dir
}

func TestServiceServesAPluginDirectory(t *testing.T) {
	t.Parallel()

	url := start(t, "-plugins", copyPlugins(t, "coupon"))

	got := quoteFor(t, url, `{"customer":"ada","coupon":"WELCOME5","items":[{"sku":"t","quantity":1,"unitCents":100}]}`)
	assert.Equal(t, quote{
		Applied: "pricing/loyalty", SubtotalCents: 100, TotalCents: 95,
		Discounts: []ruleResult{
			{Rule: "pricing/bulk"},
			{Rule: "pricing/loyalty", Cents: 5, Reason: "gold customer: 5% off"},
		},
	}, got, "only the directory's rules serve")
}

func TestServiceFailures(t *testing.T) {
	t.Parallel()

	broken := copyPlugins(t)
	require.NoError(t, os.WriteFile(filepath.Join(broken, "bulk", "bulk.go"), []byte("package bulk\n"), 0o600))

	overLimit := copyPlugins(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(overLimit, "loyalty", "tiers.lua"), []byte("return { gold = 50, silver = 2 }\n"), 0o600))

	unowned := copyPlugins(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(unowned, "coupon", "plugin.yaml"), []byte("language: typescript\nentrypoint: rule.ts\n"), 0o600))

	tests := map[string]struct {
		log  string
		args []string
		more []string // further lines the log holds
		want int
	}{
		"unknown flag": {args: []string{"-nope"}, want: exitUsage, log: "flag provided but not defined"},
		"help":         {args: []string{"-h"}, want: exitOK, log: "-plugins"},
		"bad address":  {args: []string{"-addr", "127.0.0.1:x"}, want: exitFailed, log: "service failed"},
		"missing directory": {
			args: []string{"-addr", "127.0.0.1:0", "-plugins", filepath.Join(broken, "none")},
			want: exitFailed,
			log:  "none",
		},
		"a rule that fails to load": {
			args: []string{"-addr", "127.0.0.1:0", "-plugins", broken},
			want: exitFailed,
			log:  "pricing/bulk",
		},
		// The Lua rule's own Validate finds its gold tier over the service's limits: the error issue blocks the
		// load, and the event sink logs the rejection with the team to tell (Close delivers queued events).
		"a rule over the limits": {
			args: []string{"-addr", "127.0.0.1:0", "-plugins", overLimit},
			want: exitFailed,
			log:  `level=WARN msg="plugin event" kind=rejected key=pricing/loyalty`,
			more: []string{
				"team=loyalty oncall=loyalty-oncall channel=#loyalty",
				"loyalty.percent: gold grants 50% off, over the 10% limit",
			},
		},
		"a rule without an owner": {
			args: []string{"-addr", "127.0.0.1:0", "-plugins", unowned},
			want: exitFailed,
			log:  "plg.ownership.missing",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var log strings.Builder

			assert.Equal(t, tc.want, run(t.Context(), tc.args, &log))
			assert.Contains(t, log.String(), tc.log)

			for _, want := range tc.more {
				assert.Contains(t, log.String(), want)
			}
		})
	}
}

func TestServeStopsWhenItsListenerFails(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig

	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, listener.Close(), "Serve fails at once on a closed listener")

	err = serve(
		t.Context(),
		listener,
		config{storage: plg.NewMemoryStorage("none"), logger: slog.New(slog.DiscardHandler)},
	)
	require.ErrorIs(t, err, net.ErrClosed)
}

// The service built and run as a program: it serves until it receives SIGINT, then exits cleanly.
func TestServiceProgram(t *testing.T) {
	t.Parallel()

	bin := filepath.Join(t.TempDir(), "service")

	//nolint:gosec // G204: builds this example
	build, err := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".").CombinedOutput()
	require.NoError(t, err, string(build))

	cmd := exec.CommandContext(t.Context(), bin, "-addr", "127.0.0.1:0") //nolint:gosec // G204: the program just built
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	log := watch(t, stderr)

	got := quoteFor(
		t,
		log.url,
		`{"customer":"bob","coupon":"WELCOME5","items":[{"sku":"t","quantity":10,"unitCents":100}]}`,
	)
	assert.Equal(t, "pricing/coupon", got.Applied)
	assert.Equal(t, int64(500), got.TotalCents)

	require.NoError(t, cmd.Process.Signal(syscall.SIGINT))

	for range log.rest { // drains the log until the program exits
	}

	require.NoError(t, cmd.Wait(), "a clean shutdown exits 0")
}

// adminAnswer is the service's answer to an admin request.
type adminAnswer struct {
	body   []byte
	status int
}

// decode decodes the answer's JSON body into v.
func (a adminAnswer) decode(t *testing.T, v any) {
	t.Helper()

	require.NoError(t, sonic.Unmarshal(a.body, v), string(a.body))
}

// adminDo sends the request to the service with the bearer token (none when empty).
func adminDo(t *testing.T, method, target, token string, body io.Reader) adminAnswer {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, target, body)
	require.NoError(t, err)

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	require.NoError(t, err)

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	return adminAnswer{status: resp.StatusCode, body: data}
}

const adminToken = "s3cret"

// adminPlugin is what the test reads of plg.AdminHandler's <plugin>.
type adminPlugin struct {
	Load     *adminReport `json:"load"`
	Key      string       `json:"key"`
	Status   string       `json:"status"`
	Language string       `json:"language"`
	Owner    adminOwner   `json:"owner"`
}

type adminReport struct {
	Error  *string `json:"error"`
	Issues []issue `json:"issues"`
}

type adminOwner struct {
	Team    adminTeam    `json:"team"`
	Contact adminContact `json:"contact"`
}

type adminTeam struct {
	Name string `json:"name"`
}

type adminContact struct {
	Oncall string `json:"oncall"`
}

// The admin handler shows every rule live, validated, with the team to talk to; only with the token.
func TestServiceAdminShowsWhoOwnsEachRule(t *testing.T) {
	t.Parallel()

	url := start(t, "-admin-token", adminToken)
	list := url + "/admin/plg/plugins"

	assert.Equal(t, http.StatusUnauthorized, adminDo(t, http.MethodGet, list, "", http.NoBody).status, "no token")
	assert.Equal(t, http.StatusUnauthorized, adminDo(t, http.MethodGet, list, "wrong", http.NoBody).status,
		"a wrong token")

	got := adminDo(t, http.MethodGet, list, adminToken, http.NoBody)
	require.Equal(t, http.StatusOK, got.status)

	var answer struct {
		Plugins []adminPlugin `json:"plugins"`
	}

	got.decode(t, &answer)

	owners := map[string]string{}

	for _, state := range answer.Plugins {
		assert.Equal(t, "serving", state.Status, state.Key)
		require.NotNil(t, state.Load, state.Key)
		assert.Nil(t, state.Load.Error, "validated and loaded")
		assert.Empty(t, state.Load.Issues, "no ownership warning: every rule names its team and contact")

		owners[state.Key] = state.Owner.Team.Name + " " + state.Owner.Contact.Oncall
	}

	assert.Equal(t, map[string]string{
		"pricing/bulk":    "wholesale wholesale-oncall",
		"pricing/coupon":  "promotions promotions-oncall",
		"pricing/loyalty": "loyalty loyalty-oncall",
	}, owners)

	got = adminDo(t, http.MethodGet, list+"/pricing/loyalty", adminToken, http.NoBody)
	require.Equal(t, http.StatusOK, got.status)

	var one adminPlugin

	got.decode(t, &one)
	assert.Equal(t, "lua", one.Language)
}

func TestServiceAdminEndpointsNeedAToken(t *testing.T) {
	t.Parallel()

	url := start(t)

	got := adminDo(t, http.MethodGet, url+"/admin/plg/plugins", adminToken, http.NoBody)
	assert.Equal(t, http.StatusNotFound, got.status, "without -admin-token, no admin endpoints")
}

// zipOfCoupon archives the embedded coupon rule.
func zipOfCoupon(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	archive := zip.NewWriter(&buf)
	sub, err := fs.Sub(plugins.FS, "coupon")
	require.NoError(t, err)
	require.NoError(t, archive.AddFS(sub))
	require.NoError(t, archive.Close())

	return &buf
}

// POST /admin/check dry-runs an uploaded rule with plg.Check: the same validation and ownership rules as a load,
// and nothing published.
func TestServiceChecksACandidateRule(t *testing.T) {
	t.Parallel()

	url := start(t, "-admin-token", adminToken)

	check := func(t *testing.T, key string, body io.Reader, status int) checkAnswer {
		t.Helper()

		got := adminDo(t, http.MethodPost, url+"/admin/check/"+key, adminToken, body)
		require.Equal(t, status, got.status, string(got.body))

		var answer checkAnswer

		got.decode(t, &answer)

		return answer
	}

	t.Run("a rule that would load", func(t *testing.T) {
		t.Parallel()

		got := check(t, "coupon2", zipOfCoupon(t), http.StatusOK)
		assert.True(t, got.Loads)
		assert.Equal(t, "pricing/coupon2", got.Key)
		assert.Empty(t, got.Error)
		assert.Empty(t, got.Issues)
		assert.NotEmpty(t, got.Version, "the content hash")
	})

	t.Run("a rule its own Validate rejects", func(t *testing.T) {
		t.Parallel()

		got := check(t, "loyalty2", zipOfLoyalty(t, "return { gold = 5, platinum = 25 }\n"), http.StatusOK)
		assert.False(t, got.Loads)
		assert.Contains(t, got.Error, "platinum grants 25% off, over the 10% limit")
		assert.Equal(t, []issue{{
			Severity: "error", Code: "loyalty.percent", Field: "tiers.platinum",
			Message: "platinum grants 25% off, over the 10% limit",
		}}, got.Issues)
	})

	t.Run("a rule without an owner", func(t *testing.T) {
		t.Parallel()

		got := check(t, "unowned", zipOfFiles(t, map[string]string{
			"plugin.yaml": "language: lua\nentrypoint: rule.lua\n",
			"rule.lua": "function Create() return { Discount = function() return {} end, " +
				"Validate = function() end } end\n",
		}), http.StatusOK)
		assert.False(t, got.Loads)
		require.Len(t, got.Issues, 1)
		assert.Equal(t, plg.CodeOwnershipMissing, got.Issues[0].Code)
	})

	t.Run("not a zip archive", func(t *testing.T) {
		t.Parallel()

		got := check(t, "junk", strings.NewReader("junk"), http.StatusBadRequest)
		assert.Contains(t, got.Error, "zip")
	})

	t.Run("an invalid plugin.yaml", func(t *testing.T) {
		t.Parallel()

		got := check(t, "bad", zipOfFiles(t, map[string]string{"plugin.yaml": "language: cobol\n"}),
			http.StatusBadRequest)
		assert.Contains(t, got.Error, "plugin.yaml")
	})

	t.Run("no plugin.yaml", func(t *testing.T) {
		t.Parallel()

		got := check(t, "none", zipOfFiles(t, map[string]string{"rule.lua": ""}), http.StatusBadRequest)
		assert.Contains(t, got.Error, "plugin.yaml")
	})

	t.Run("without the token", func(t *testing.T) {
		t.Parallel()

		got := adminDo(t, http.MethodPost, url+"/admin/check/x", "", zipOfCoupon(t))
		assert.Equal(t, http.StatusUnauthorized, got.status)
	})
}

// zipOfLoyalty archives the loyalty rule with tiers as its tiers.lua.
func zipOfLoyalty(t *testing.T, tiers string) *bytes.Buffer {
	t.Helper()

	files := map[string]string{"tiers.lua": tiers}

	for _, name := range []string{"plugin.yaml", "rule.lua"} {
		data, err := fs.ReadFile(plugins.FS, "loyalty/"+name)
		require.NoError(t, err)

		files[name] = string(data)
	}

	return zipOfFiles(t, files)
}

// zipOfFiles archives files, by name.
func zipOfFiles(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	archive := zip.NewWriter(&buf)

	for name, content := range files {
		w, err := archive.Create(name)
		require.NoError(t, err)

		_, err = io.WriteString(w, content)
		require.NoError(t, err)
	}

	require.NoError(t, archive.Close())

	return &buf
}

// A Check publishes nothing: after checking a candidate, the admin handler still lists the three rules only.
func TestServiceCheckPublishesNothing(t *testing.T) {
	t.Parallel()

	url := start(t, "-admin-token", adminToken)

	got := adminDo(t, http.MethodPost, url+"/admin/check/coupon2", adminToken, zipOfCoupon(t))
	require.Equal(t, http.StatusOK, got.status)

	got = adminDo(t, http.MethodGet, url+"/admin/plg/plugins", adminToken, http.NoBody)
	require.Equal(t, http.StatusOK, got.status)

	var answer struct {
		Plugins []adminPlugin `json:"plugins"`
	}

	got.decode(t, &answer)
	assert.Len(t, answer.Plugins, 3)

	got = adminDo(t, http.MethodGet, url+"/admin/plg/plugins/pricing/coupon2", adminToken, http.NoBody)
	assert.Equal(t, http.StatusNotFound, got.status)
}

func TestCheckOnAClosedContainerCannotCheck(t *testing.T) {
	t.Parallel()

	container := plg.New()
	require.NoError(t, container.Close(t.Context()))

	got := check(t.Context(), container, "coupon", zipOfCoupon(t))
	assert.Equal(t, http.StatusServiceUnavailable, got.status)
	assert.Contains(t, got.Error, plg.ErrClosed.Error())
	assert.False(t, got.Loads)
}

// The event sink logs a panic at Warn with the team to tell.
func TestLogEventOfAPanic(t *testing.T) {
	t.Parallel()

	var log strings.Builder

	logEvent(slog.New(slog.NewTextHandler(&log, nil)))(t.Context(), plg.Event{
		Kind:  plg.EventPanicked,
		Key:   "pricing/bulk",
		Panic: &plg.PanicError{Key: "pricing/bulk", Value: "boom"},
		Owner: plg.Owner{Team: plg.Team{Name: "wholesale"}, Contact: plg.Contact{Channel: "#wholesale"}},
	})

	assert.Contains(t, log.String(), `level=WARN msg="plugin event" kind=panicked key=pricing/bulk version="" `+
		`team=wholesale oncall="" channel=#wholesale error=`)
}
