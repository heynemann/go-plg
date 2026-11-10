//go:build integration

package authorguide_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/examples/authorrepo"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var (
	marker    = regexp.MustCompile(`^<!-- authorguide:(\S+) -->$`)
	errFormat = errors.New("malformed author guide")
	// serving is the service's log line saying where it listens.
	serving = regexp.MustCompile(`msg=serving addr=(\S+)`)
)

// parseGuide returns the code blocks of the README that an authorguide marker comment precedes, by the marker's
// name; a name given twice is an error.
func parseGuide(readme string) (map[string]string, error) {
	blocks := map[string]string{}
	lines := strings.Split(readme, "\n")

	for i := 0; i < len(lines); i++ {
		match := marker.FindStringSubmatch(lines[i])
		if match == nil {
			continue
		}

		if i+1 == len(lines) || !strings.HasPrefix(lines[i+1], "```") {
			return nil, fmt.Errorf("%w: README.md:%d: no code block after the marker", errFormat, i+1)
		}

		start := i + 2

		end := slices.Index(lines[start:], "```")
		if end < 0 {
			return nil, fmt.Errorf("%w: README.md:%d: unterminated code block", errFormat, i+2)
		}

		if _, dup := blocks[match[1]]; dup {
			return nil, fmt.Errorf("%w: README.md:%d: a second %s block", errFormat, i+1, match[1])
		}

		blocks[match[1]] = strings.Join(lines[start:start+end], "\n") + "\n"
		i = start + end
	}

	return blocks, nil
}

// sh runs script with sh -eu in dir, reading stdin, and returns what it printed (stdout and stderr), failing the
// test unless it exits 0.
func sh(t *testing.T, dir string, environ []string, script, stdin string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "sh", "-euc", script) //nolint:gosec // G204: the README's commands
	cmd.Dir = dir
	cmd.Env = environ
	cmd.Stdin = strings.NewReader(stdin)

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s\n%s", script, out)

	return string(out)
}

// The README's author guide, run as written: a plugin started with plg init validates, and plg dev calls its
// Discount with the guide's order.yaml; in a copy of the example author repository the typings are up to date, a
// plugin validates and the CI script validates (JSON and JUnit
// reports) and packs every plugin; the pricing service serves the artifacts; plg status and plg events, through
// the profile the guide writes, show the plugins it serves; served again with the coupon API's address and token,
// the seasonal rule prices a traced quote with the API's answer. What needs the network differs: plg is built from
// this checkout instead of installed, the contract module comes from a local GOPROXY, the service is built once
// instead of `go run` and listens on a free port, which replaces localhost:8080 in the profile and the commands,
// and a local coupon API stands for the guide's.
func TestReadmeAuthorGuide(t *testing.T) {
	t.Parallel()

	examples := authorrepo.Examples(t)

	readme, err := os.ReadFile(filepath.Join(examples, "..", "README.md")) //nolint:gosec // G304: this checkout's
	require.NoError(t, err)

	guide, err := parseGuide(string(readme))
	require.NoError(t, err)

	install := "go install github.com/heynemann/go-plg/cmd/plg@latest\n"
	require.True(t, strings.HasPrefix(guide["start"], install), "the guide installs plg first")
	require.Equal(t, "cd examples/rules\n", guide["repo"][:len("cd examples/rules\n")], "from plg's checkout")
	require.Equal(t, "plg dev promo\n", guide["dev"], "the command this test feeds commands to")
	require.Contains(t, guide, "order")
	require.Contains(t, guide, "profiles")
	require.Contains(t, guide, "serving")
	require.Contains(t, guide, "coupon")

	environ := authorrepo.Environ(t)
	config := t.TempDir()
	environ = append(environ, "XDG_CONFIG_HOME="+config)

	start := t.TempDir()

	out := sh(t, start, environ, strings.TrimPrefix(guide["start"], install), "")
	assert.Contains(t, out, "files      ok       wrote plugin.yaml, plg.d.lua, plugin.lua", "plg init")
	assert.Regexp(t, `\nload +ok +Create ran in the lua guest`, out, "plg validate")

	require.NoError(t, os.WriteFile(filepath.Join(start, "order.yaml"), []byte(guide["order"]), 0o600))

	out = sh(t, start, environ, guide["dev"], "call Discount order.yaml\nquit\n")
	assert.Contains(t, out, "Discount: result {reason: \"\", cents: 0}\n", "the skeleton's zero discount")

	checkout := t.TempDir() // stands for plg's checkout, holding examples/rules
	authorrepo.Copy(t, filepath.Join(checkout, "examples", "rules"))

	out = sh(t, checkout, environ, guide["repo"], "")
	assert.Contains(t, out, "typings    ok      plg.d.ts, tsconfig.json already up to date")
	assert.Contains(t, out, "\ntests      ok      8 passed\n")

	dist := filepath.Join(checkout, "examples", "rules", "dist")

	for _, key := range []string{"bundle", "vip", "seasonal"} {
		assert.Contains(t, out, "plg pack: wrote dist/"+key+".plg (sha256:")
		assert.FileExists(t, filepath.Join(dist, key+".plg"))
	}

	for _, report := range []string{"validate.json", "validate.xml"} {
		assert.FileExists(t, filepath.Join(checkout, "examples", "rules", "reports", report))
	}

	bin := buildService(t, examples)
	addr := startService(t, bin, filepath.Join(checkout, "examples", "rules"), environ, guide["service"])
	local := func(s string) string { return strings.ReplaceAll(s, "localhost:8080", addr) }

	profiles := filepath.Join(config, "plg", "profiles.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(profiles), 0o750))
	require.NoError(t, os.WriteFile(profiles, []byte(local(guide["profiles"])), 0o600))

	out = sh(t, t.TempDir(), environ, local(guide["serving"]), "")
	assert.Contains(t, out, `"applied":"pricing/seasonal"`, "the quote")
	assert.Regexp(t, `\npricing/vip +serving +sha256:`, out, "plg status")
	assert.Regexp(t, `\nprovenance +commit [0-9a-f]{12}`, out, "packed from the repository's commit")
	assert.Contains(t, out, "contract github.com/heynemann/go-plg/examples v1.0.0", "plg status shows the provenance")
	assert.Regexp(t, `\n\S+ +loaded +sha256:`, out, "plg events")

	checkSecretsAndTraces(t, guide, bin, filepath.Join(checkout, "examples"), environ)
}

// The coupon API of the guide's step 8: its address and the token the guide gives it.
const (
	guideCouponURL   = "http://coupons.internal:8080"
	guideCouponHost  = "coupons.internal:8080"
	guideCouponToken = "c0up0n-t0k3n"
)

// checkSecretsAndTraces runs the guide's step 8 in dir (the checkout's examples directory): the service serves the
// artifacts again with the coupon API's address and token, here a local API's, which grants AUTUMN 5% off with the
// guide's token, and the guide's traced quote gets the API's answer, the API the token and the quote's trace.
func checkSecretsAndTraces(t *testing.T, guide map[string]string, bin, dir string, environ []string) {
	t.Helper()

	require.Contains(t, guide["secrets"], guideCouponURL)
	require.Contains(t, guide["secrets"], "-coupon-api "+guideCouponHost+" ")
	require.Contains(t, guide["secrets"], guideCouponToken)

	type request struct{ authorization, traceparent string }

	requests := make(chan request, 1)
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		requests <- request{req.Header.Get("Authorization"), req.Header.Get("Traceparent")}

		if req.URL.Path != "/coupons/AUTUMN" || req.Header.Get("Authorization") != "Bearer "+guideCouponToken {
			http.NotFound(writer, req)

			return
		}

		_, err := writer.Write([]byte(`{"percent":5}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(api.Close)

	script := strings.NewReplacer(guideCouponURL, api.URL, guideCouponHost, strings.TrimPrefix(api.URL, "http://")).
		Replace(guide["secrets"])
	addr := startService(t, bin, dir, environ, script)

	out := sh(t, t.TempDir(), environ, strings.ReplaceAll(guide["coupon"], "localhost:8080", addr), "")
	assert.Contains(t, out, `{"rule":"pricing/seasonal","reason":"coupon AUTUMN: 5% off","cents":225}`,
		"the coupon API's 5%")

	select {
	case got := <-requests: // sent before the API answered, so before the quote was
		assert.Equal(t, "Bearer "+guideCouponToken, got.authorization)
		assert.Regexp(t, `^00-4bf92f3577b34da6a3ce929d0e0e4736-[0-9a-f]{16}-01$`, got.traceparent, "the quote's trace")
	default:
		require.FailNow(t, "the coupon API received no request")
	}
}

// buildService builds the pricing service of the examples module and returns the program.
func buildService(t *testing.T, examples string) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "service")

	//nolint:gosec // G204: builds this example
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "./service")
	build.Dir = examples
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	return bin
}

// startService runs the guide's script in dir with sh, its `go run ./service` the program bin listening on a free
// port, until the test ends; it returns the address the service listens on.
func startService(t *testing.T, bin, dir string, environ []string, script string) string {
	t.Helper()

	const goRun = "go run ./service "

	require.Equal(t, 1, strings.Count(script, goRun), "the script runs the service once: %s", script)

	script = strings.Replace(script, goRun, "exec "+bin+" -addr 127.0.0.1:0 ", 1)

	//nolint:gosec // G204: the README's commands, running the program just built
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "sh", "-euc", script)
	cmd.Dir = dir
	cmd.Env = environ

	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	log := bufio.NewScanner(stderr)

	var addr string

	for addr == "" && log.Scan() {
		if match := serving.FindStringSubmatch(log.Text()); match != nil {
			addr = match[1]
		}
	}

	t.Cleanup(func() {
		assert.NoError(t, cmd.Process.Signal(syscall.SIGINT))

		for log.Scan() { // drains the log until the program exits
		}

		assert.NoError(t, cmd.Wait(), "a clean shutdown exits 0")
	})

	require.NotEmpty(t, addr, "the service serves")

	return addr
}

func TestParseGuideRejectsMalformedMarkers(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"no block":       "<!-- authorguide:start -->\ntext\n",
		"end of file":    "<!-- authorguide:start -->\n",
		"unterminated":   "<!-- authorguide:start -->\n```sh\nplg validate .\n",
		"block too late": "<!-- authorguide:start -->\n\n```sh\nplg validate .\n```\n",
		"twice":          "<!-- authorguide:a -->\n```sh\nx\n```\n<!-- authorguide:a -->\n```sh\ny\n```\n",
	}

	for name, readme := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := parseGuide(readme)
			require.ErrorIs(t, err, errFormat)
		})
	}

	blocks, err := parseGuide("<!-- authorguide:a -->\n```sh\nplg\n\n```\n<!-- other -->\n")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "plg\n\n"}, blocks)
}
