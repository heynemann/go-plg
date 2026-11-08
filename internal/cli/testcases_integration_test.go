//go:build integration

package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/testproxy"
)

// withCases copies the testdata plugin dir into a temporary directory, with the test cases of
// testdata/validate/cases (tests/ and fixtures/) and the extra files given by name and content, and returns its
// path.
func withCases(t *testing.T, dir string, extra ...string) string {
	t.Helper()

	plugin := filepath.Join(t.TempDir(), dir)
	testdata := filepath.Join("..", "..", "testdata", "validate")

	require.NoError(t, os.CopyFS(plugin, os.DirFS(filepath.Join(testdata, "plugins", dir))))
	require.NoError(t, os.CopyFS(plugin, os.DirFS(filepath.Join(testdata, "cases"))))

	for i := 0; i+1 < len(extra); i += 2 {
		require.NoError(t, os.WriteFile(filepath.Join(plugin, extra[i]), []byte(extra[i+1]), 0o600))
	}

	return plugin
}

// rows returns the rows plg validate printed, each cell separated by one space.
func rows(stdout string) []string {
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}

	return lines
}

// step is a row plg validate printed and the rows of details below it, each cell separated by one space.
type step struct {
	row     string
	details []string
}

// following returns the row starting with prefix and the rows of details below it, up to the next row of a step (a
// row whose first cell is not empty: the rows of details start with spaces); a zero step when no row starts with
// prefix.
func following(stdout, prefix string) step {
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")

	for i, line := range lines {
		found := step{row: strings.Join(strings.Fields(line), " ")}
		if !strings.HasPrefix(found.row, prefix) {
			continue
		}

		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, " ") {
				break
			}

			found.details = append(found.details, strings.Join(strings.Fields(next), " "))
		}

		return found
	}

	return step{}
}

// TestValidateRunsTestCasesInEveryLanguage validates the pricing rule of each language with the same test case
// files, run in the language's real guest: every case of tests/pass.yaml passes (results, errors returned and
// thrown, Validate's issues, Host stubs matched by argument and counted, HTTP stubs, the HTTP policy refusing a
// host, fixtures with nested $ref), and every case of tests/fail.yaml fails for its own reason, shown with the
// expected and actual values; a file that does not parse fails on its own. plg validate exits 1.
func TestValidateRunsTestCasesInEveryLanguage(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	passed := []string{
		"gold customers get 10% off", "a gift is a reason", "the directory's error is the call's",
		"an empty order asks nobody", "split returns the parts and the rest", "coupons come from the coupon service",
		"an unknown coupon", "the policy refuses other hosts", "within the limits", "over the limits",
	}

	type failure struct {
		name, failure string
		details       []string
	}

	failed := func(key, panicked string) []failure {
		return []failure{
			{"another total", "Quote returned other results than expect.result", []string{
				"result.total: expected 1000, got 900", "result.discount: expected 0, got 100",
				`result.reasons: expected ["gold", "gift"], got ["gold"]`,
			}},
			{"an unexpected error", "Quote failed: the directory is down", nil},
			{"another error", "Coupon returned another error than expect.error", []string{
				`expected an error containing "expired"`, `got "unknown coupon NOPE"`,
			}},
			{"no error", "Split returned no error", []string{
				`expected an error containing "no parts"`, "got results [[5, 5], 0]",
			}},
			{"a stub called less than declared", "the stubs were not used as the case declares", []string{
				"host stub Tier() answered 1 call, expected 2",
			}},
			{
				"a host call no stub matches",
				`Quote failed: plg validate: no host stub of the test case matches Tier(customer: "alice")`,
				[]string{
					`Host call Tier(customer: "alice") matched no host stub`,
				},
			},
			{"a request no stub matches", "Coupon failed: ", []string{
				"HTTP request GET https://coupons.example.com/codes/SAVE10 matched no http stub",
			}},
			{
				"a typo in an input",
				`tests/fail.yaml:64:30: input.order.lines[0]: unknown field "qtty" (fields: sku, qty, cents)`,
				nil,
			},
			{
				"a typo in an expectation",
				`tests/fail.yaml:70:24: expect.result[1]: str "zero" is not an integer (int)`,
				nil,
			},
			{"an input of another type", `tests/fail.yaml:74:20: input.cents: str "ten" is not an integer (int)`, nil},
			{"a missing fixture", "tests/fail.yaml:79:21: input.order: $ref: open fixtures/missing.yaml: ", nil},
			{"a panic", "Spin failed: it panicked or trapped: plg: " + key + ": call Spin failed: " + panicked, nil},
			{"a timeout", "Spin failed: it did not return within 3s: plg: " + key + ": call Spin failed: ", nil},
			{
				"other issues",
				"Validate returned other issues than expect.issues",
				[]string{
					`issues: expected 0, got 1: [{severity: "error", code: "gold.percent", message: "the gold discount (10%) ` +
						`is over the limit (5%)", field: "maxPercent"}]`,
				},
			},
			{
				"an unknown method",
				`tests/fail.yaml:96:5: call "Price" is not a method of the contract (Coupon, Quote, Spin, Split, Validate)`,
				nil,
			},
		}
	}

	for _, tc := range []struct{ dir, panicked string }{
		{"gopricing", "spun out"}, {"luapricing", "not enough memory"}, {"tspricing", "out of memory"},
	} {
		plugin := withCases(t, tc.dir, "tests/broken.yaml", "cases: [")
		got := validate(environ, plugin)
		require.Equal(t, 1, got.code, "%s\n%s%s", tc.dir, got.stdout, got.stderr)

		printed := rows(got.stdout)

		for _, name := range passed {
			assert.Contains(t, printed, "test ok tests/pass.yaml: "+name, "%s\n%s", tc.dir, got.stdout)
		}

		for _, want := range failed("validate/"+tc.dir, tc.panicked) {
			found := following(got.stdout, "test FAIL tests/fail.yaml: "+want.name+": ")
			assert.True(t, strings.HasPrefix(found.row, "test FAIL tests/fail.yaml: "+want.name+": "+want.failure),
				"%s: %s\n%s", tc.dir, want.name, got.stdout)
			assert.Equal(t, want.details, found.details, "%s: %s", tc.dir, want.name)
		}

		assert.Equal(t, "test FAIL tests/broken.yaml: parsing: yaml: line 1: did not find expected node content",
			following(got.stdout, "test FAIL tests/broken.yaml: ").row, tc.dir)
		assert.Equal(t, "tests FAIL test cases failed: 16 failed, 10 passed", printed[len(printed)-1], tc.dir)
	}
}

// TestValidatePassesWhenEveryCasePasses validates a plugin whose test cases all pass: plg validate exits 0.
func TestValidatePassesWhenEveryCasePasses(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	plugin := withCases(t, "luapricing")
	require.NoError(t, os.Remove(filepath.Join(plugin, "tests", "fail.yaml")))

	got := validate(environ, plugin)
	require.Equal(t, 0, got.code, "%s%s", got.stdout, got.stderr)
	assert.Equal(t, "tests ok 10 passed", rows(got.stdout)[len(rows(got.stdout))-1])
}
