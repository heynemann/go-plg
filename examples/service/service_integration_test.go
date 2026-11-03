//go:build integration

package main

import (
	"bufio"
	"context"
	"io"
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

	tests := map[string]struct {
		log  string
		args []string
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
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var log strings.Builder

			assert.Equal(t, tc.want, run(t.Context(), tc.args, &log))
			assert.Contains(t, log.String(), tc.log)
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
