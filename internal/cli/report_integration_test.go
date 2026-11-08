//go:build integration

package cli_test

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/testproxy"
)

// jsonReport is plg validate's JSON report, every field a consumer reads (README, "plg validate").
type jsonReport struct {
	Repository string       `json:"repository"`
	Plugins    []jsonPlugin `json:"plugins"`
	Version    int          `json:"version"`
	Valid      bool         `json:"valid"`
}

type jsonPlugin struct {
	Plugin     string          `json:"plugin"`
	Dir        string          `json:"dir"`
	FailedStep string          `json:"failedStep"`
	Steps      []jsonStep      `json:"steps"`
	Resources  []jsonResources `json:"resources"`
	Warnings   int             `json:"warnings"`
	Valid      bool            `json:"valid"`
}

type jsonStep struct {
	Step    string      `json:"step"`
	Result  string      `json:"result"`
	Subject string      `json:"subject"`
	Detail  string      `json:"detail"`
	Hint    string      `json:"hint"`
	Lines   []string    `json:"lines"`
	Issues  []jsonIssue `json:"issues"`
}

type jsonIssue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Field    string `json:"field"`
}

type jsonResources struct {
	Call             string   `json:"call"`
	NearLimit        []string `json:"nearLimit"`
	MemoryBytes      uint64   `json:"memoryBytes"`
	MemoryLimitBytes uint64   `json:"memoryLimitBytes"`
	DurationMs       float64  `json:"durationMs"`
	TimeoutMs        float64  `json:"timeoutMs"`
}

// decodeReport decodes plg validate's JSON report strictly: a field jsonReport lacks fails, so the tests pin every
// field the report has.
func decodeReport(t *testing.T, stdout string) jsonReport {
	t.Helper()

	decoder := sonic.ConfigStd.NewDecoder(strings.NewReader(stdout))
	decoder.DisallowUnknownFields()

	var got jsonReport

	require.NoError(t, decoder.Decode(&got), stdout)
	require.False(t, decoder.More(), "one JSON document")

	return got
}

// step returns the plugin's first step named name with the result, and whether there is one.
func (p *jsonPlugin) step(name, result string) (jsonStep, bool) {
	i := slices.IndexFunc(p.Steps, func(s jsonStep) bool { return s.Step == name && s.Result == result })
	if i < 0 {
		return jsonStep{}, false
	}

	return p.Steps[i], true
}

// junitReport is the JUnit XML plg validate writes, as CI systems read it.
type junitReport struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Suites   []junitSuite `xml:"testsuite"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Cases    []junitCase `xml:"testcase"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
}

type junitCase struct {
	Failure   *junitFailure `xml:"failure"`
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	SystemOut string        `xml:"system-out"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

func readJUnit(t *testing.T, name string) junitReport {
	t.Helper()

	data, err := os.ReadFile(name) //nolint:gosec // G304: the test's file
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(data), xml.Header), "an XML declaration first")

	var got junitReport

	require.NoError(t, xml.Unmarshal(data, &got), string(data))

	return got
}

// TestValidateReportsEachStepsFailureInEveryFormat validates a plugin failing at each step of the pipeline, twice:
// as a table with -junit, and with -format json. The step fails in all three: its table row says FAIL, the JUnit
// file's suite for the plugin has a failed test case named after it, its first, whose message is the step's
// detail, and the JSON report names it as the plugin's failed step, with that detail (and the issue it holds, if
// any). plg validate exits 1 each time. A repository whose plg.yaml is invalid is reported the same way, its one
// suite and plugin the repository failing at plg.yaml. (The guest step fails in TestValidateReportFromTheCommand,
// which can take away the user cache directory.)
func TestValidateReportsEachStepsFailureInEveryFormat(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	badRepo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.CopyFS(filepath.Join(badRepo, "luagood"), os.DirFS(filepath.Join("..", "..", "testdata",
		"validate", "plugins", "luagood"))))
	require.NoError(t, os.WriteFile(filepath.Join(badRepo, "plg.yaml"), []byte("plugins: [luagood"), 0o600))

	clashing := copyRepo(t, "plugins: [rules/silver]\nshared: [lib]\n",
		map[string]string{"rules/silver/lib/own.lua": "return {}\n"})

	strict := withCases(t, "luapricing", "examples/validation/strict.yaml", "maxPercent: 5\n")
	require.NoError(t, os.Remove(filepath.Join(strict, "tests", "fail.yaml")))

	owned := copyRepo(t, "plugins: [unowned]\nrequireOwnership: true\n", map[string]string{})

	tests := []struct {
		step, detail, plugin, issue string
	}{
		{"plg.yaml", "invalid plg.yaml: " + filepath.Join(badRepo, "plg.yaml") + ": ", badRepo, ""},
		{"plg.yaml", "invalid plg.yaml: ", filepath.Join(badRepo, "luagood"), ""},
		{"manifest", "plg: invalid plugin.yaml: implements is required", "badmanifest", ""},
		{
			"manifest", "lua plugin unowned implementing example.com/rewards/cohort.Provider@v1.2.0",
			filepath.Join(owned, "unowned"), "plg.ownership.missing",
		},
		{"shared", "cannot copy a shared directory into the plugin: ", filepath.Join(clashing, "rules", "silver"), ""},
		{
			"contract",
			"no module provides the contract package example.com/nosuch/cohort at v1.0.0",
			"unknowncontract",
			"",
		},
		{
			"interface",
			"not an interface plg can adapt: package cohort declares no type Segmenter",
			"missinginterface",
			"",
		},
		{"go.mod", "the plugin's go.mod does not require the pinned contract version: ", "gomodmismatch", ""},
		{"sandbox", "plg: go statements are not allowed in plugins: plugin.go:15:2, plugin.go:22:3", "gogoroutine", ""},
		{"types", "the plugin does not type-check against the contract: plugin.go:14:9: ", "wrongsignature", ""},
		{"types", "plugin.ts:4:51: Property 'Rank' does not exist on type 'Users'. (TS2339)", "tsunknownhost", ""},
		{"typings", "plg: invalid interface cohort.Broken: ", "brokencontract", ""},
		{"syntax", "1 .lua file does not compile", "luasyntax", ""},
		{"load", "plugin Create failed: no cohort configured", "createfails", ""},
		{
			"example", "1 error issue: a service validating with this input would reject the plugin", strict,
			"gold.percent",
		},
		{"test", "Quote returned other results than expect.result", withCases(t, "luapricing"), ""},
	}

	for _, tc := range tests {
		junit := filepath.Join(t.TempDir(), "report.xml")

		table := validate(environ, "-junit", junit, tc.plugin)
		assert.Equal(t, 1, table.code, "%s\n%s%s", tc.step, table.stdout, table.stderr)
		assert.Regexp(t, `(?m)^`+regexp.QuoteMeta(tc.step)+` +FAIL +.*`+regexp.QuoteMeta(tc.detail), table.stdout,
			tc.step)

		suites := readJUnit(t, junit)
		require.Len(t, suites.Suites, 1, tc.step)
		assert.Equal(t, tc.plugin, suites.Suites[0].Name, tc.step)
		assert.GreaterOrEqual(t, suites.Failures, 1, tc.step)
		assert.Equal(t, suites.Suites[0].Failures, suites.Failures, tc.step)

		failed := slices.IndexFunc(suites.Suites[0].Cases, func(c junitCase) bool { return c.Failure != nil })
		require.GreaterOrEqual(t, failed, 0, "%s: a failed JUnit test case", tc.step)

		first := suites.Suites[0].Cases[failed]
		assert.True(t, first.Name == tc.step || strings.HasPrefix(first.Name, tc.step+": "), "%s: %s", tc.step,
			first.Name)
		assert.Equal(t, tc.plugin, first.ClassName, tc.step)
		assert.Contains(t, first.Failure.Message, tc.detail, tc.step)

		asJSON := validate(environ, "-format", "json", tc.plugin)
		assert.Equal(t, 1, asJSON.code, "%s\n%s%s", tc.step, asJSON.stdout, asJSON.stderr)

		got := decodeReport(t, asJSON.stdout)
		require.Len(t, got.Plugins, 1, tc.step)
		assert.False(t, got.Valid, tc.step)
		assert.False(t, got.Plugins[0].Valid, tc.step)
		assert.Equal(t, tc.step, got.Plugins[0].FailedStep, tc.step)

		step, found := got.Plugins[0].step(tc.step, "failed")
		require.True(t, found, "%s\n%s", tc.step, asJSON.stdout)
		assert.Contains(t, step.Detail, tc.detail, tc.step)

		if tc.issue != "" {
			require.Len(t, step.Issues, 1, tc.step)
			assert.Equal(t, "error", step.Issues[0].Severity, tc.step)
			assert.Equal(t, tc.issue, step.Issues[0].Code, tc.step)
		}

		if tc.plugin == badRepo {
			assert.Equal(t, filepath.Join(badRepo, "plg.yaml"), got.Repository)
			assert.Equal(t, badRepo, got.Plugins[0].Dir)
			require.Len(t, got.Plugins[0].Steps, 1, "no plugin validated")
		}
	}
}

// TestValidateReportsAValidPluginAsJSON validates a plugin whose every step passes, with example validation inputs
// (one taking a test case's fixture by $ref) and test cases, with -format json: the report pins what a consumer
// reads, every field included (decodeReport refuses unknown ones). The steps are those of a Lua plugin, in order,
// each ok; Validate ran with each example input; each test case's call has its resources against the limits.
func TestValidateReportsAValidPluginAsJSON(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	plugin := withCases(t, "luapricing", "examples/validation/lenient.yaml", "maxPercent: 20\n",
		"examples/validation/shared.yaml", "{$ref: fixtures/limits.yaml}\n", "fixtures/limits.yaml", "maxPercent: 10\n")
	require.NoError(t, os.Remove(filepath.Join(plugin, "tests", "fail.yaml")))

	got := validate(environ, "-format", "json", plugin)
	require.Equal(t, 0, got.code, "%s%s", got.stdout, got.stderr)

	report := decodeReport(t, got.stdout)
	assert.Equal(t, 1, report.Version)
	assert.True(t, report.Valid)
	assert.Empty(t, report.Repository, "a plugin validated on its own")
	require.Len(t, report.Plugins, 1)

	validated := report.Plugins[0]
	assert.Equal(t, plugin, validated.Plugin)
	assert.Equal(t, plugin, validated.Dir)
	assert.True(t, validated.Valid)
	assert.Empty(t, validated.FailedStep)
	assert.Zero(t, validated.Warnings)

	var steps []string

	for _, step := range validated.Steps {
		assert.Equal(t, "ok", step.Result, "%s: %s", step.Step, step.Detail)

		if len(steps) == 0 || steps[len(steps)-1] != step.Step {
			steps = append(steps, step.Step)
		}
	}

	assert.Equal(t, []string{
		"manifest", "contract", "interface", "limits", "typings", "syntax", "load", "example", "validate", "test",
		"tests", "resources",
	}, steps)

	examples, found := validated.step("example", "ok")
	require.True(t, found)
	assert.Equal(t, "examples/validation/lenient.yaml", examples.Subject)
	assert.Equal(t, "no issues", examples.Detail)

	summary, found := validated.step("validate", "ok")
	require.True(t, found)
	assert.Equal(t, "2 example inputs pass", summary.Detail)

	tests, found := validated.step("tests", "ok")
	require.True(t, found)
	assert.Equal(t, "10 passed", tests.Detail)

	require.Len(t, validated.Resources, 10, "a call per test case")

	for _, call := range validated.Resources {
		assert.True(t, strings.HasPrefix(call.Call, "tests/pass.yaml: "), call.Call)
		assert.Equal(t, uint64(64<<20), call.MemoryLimitBytes, "Lua's default")
		assert.Positive(t, call.MemoryBytes, call.Call)
		assert.Less(t, call.MemoryBytes, call.MemoryLimitBytes, call.Call)
		assert.InDelta(t, 20000.0, call.TimeoutMs, 0, "pricing.Rule's //plg:limits timeout=20s")
		assert.Positive(t, call.DurationMs, call.Call)
		assert.Empty(t, call.NearLimit, call.Call)
	}
}

// TestValidateReportsEachExampleInput validates pricing rules whose Validate warns about a maxPercent from 10 to
// 14, fails (raises) at 99 and accepts 15 and more, with example validation inputs of each kind, and a cohort
// provider whose contract declares no validation input:
//   - an example Validate warns about is a warning row holding the issue, counted in the warnings; plg validate
//     exits 0 and the summary counts it among those that pass;
//   - an example that does not decode into the validation input and one Validate fails for each fail their row, and
//     the summary row fails counting them; plg validate exits 1;
//   - examples of a contract without //plg:validation are warnings, not used: the summary says none passed and counts
//     them apart;
//   - a rule without examples gets a row saying so.
func TestValidateReportsEachExampleInput(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	source, err := os.ReadFile(filepath.Join("..", "..", "testdata", "validate", "plugins", "luapricing",
		"plugin.lua"))
	require.NoError(t, err)

	pickier := strings.Replace(string(source), "function Rule:Validate(limits)\n", "function Rule:Validate(limits)\n"+
		"  if limits.maxPercent == 99 then\n"+
		"    error(\"the limits service is down\")\n"+
		"  end\n"+
		"  if limits.maxPercent >= 10 and limits.maxPercent < 15 then\n"+
		"    return { { severity = \"warning\", code = \"gold.close\", field = \"maxPercent\",\n"+
		"      message = \"the limit is close to the gold discount\" } }\n"+
		"  end\n", 1)
	require.NotEqual(t, string(source), pickier)

	rule := func(examples ...string) string {
		plugin := withCases(t, "luapricing", examples...)
		lua := filepath.Join(plugin, "plugin.lua")
		require.NoError(t, os.WriteFile(lua, []byte(pickier), 0o600)) //nolint:gosec // G703: the test's directory
		require.NoError(t, os.Remove(filepath.Join(plugin, "tests", "fail.yaml")))

		return plugin
	}

	warned := rule("examples/validation/close.yaml", "maxPercent: 12\n",
		"examples/validation/lenient.yaml", "maxPercent: 20\n")

	got := validate(environ, warned)
	require.Equal(t, 0, got.code, "warnings do not fail validate\n%s%s", got.stdout, got.stderr)
	assert.Contains(t, rows(got.stdout), "example warning examples/validation/close.yaml: 1 warning issue")
	assert.Contains(t, rows(got.stdout), "warning gold.close (maxPercent): the limit is close to the gold discount")
	assert.Contains(t, rows(got.stdout), "example ok examples/validation/lenient.yaml: no issues")
	assert.Contains(t, rows(got.stdout), "validate ok 2 example inputs pass")

	asJSON := validate(environ, "-format", "json", warned)
	require.Equal(t, 0, asJSON.code, "%s%s", asJSON.stdout, asJSON.stderr)

	report := decodeReport(t, asJSON.stdout)
	require.Len(t, report.Plugins, 1)
	assert.True(t, report.Valid)
	assert.Equal(t, 1, report.Plugins[0].Warnings, "the warning issue, its step not counted again")

	nearly, found := report.Plugins[0].step("example", "warning")
	require.True(t, found, asJSON.stdout)
	assert.Equal(t, "examples/validation/close.yaml", nearly.Subject)
	assert.Equal(t, []jsonIssue{
		{
			Severity: "warning",
			Code:     "gold.close",
			Message:  "the limit is close to the gold discount",
			Field:    "maxPercent",
		},
	}, nearly.Issues)

	broken := rule("examples/validation/broken.yaml", "maxPercent: lots\n",
		"examples/validation/down.yaml", "maxPercent: 99\n", "examples/validation/lenient.yaml", "maxPercent: 20\n")

	got = validate(environ, broken)
	assert.Equal(t, 1, got.code, "%s%s", got.stdout, got.stderr)
	assert.Regexp(t, `(?m)^example +FAIL +examples/validation/broken\.yaml: examples/validation/broken\.yaml:1:13: `+
		`example\.maxPercent: str "lots" is not an `, got.stdout)
	assert.Regexp(t, `(?m)^example +FAIL +examples/validation/down\.yaml: Validate failed: .*the limits service is `+
		`down`, got.stdout)
	assert.Contains(t, rows(got.stdout), "example ok examples/validation/lenient.yaml: no issues")
	assert.Contains(t, rows(got.stdout), "validate FAIL example validation inputs failed: 2 of 3")

	unused := filepath.Join(t.TempDir(), "luagood")
	require.NoError(t, os.CopyFS(unused, os.DirFS(filepath.Join("..", "..", "testdata", "validate", "plugins",
		"luagood"))))
	require.NoError(t, os.MkdirAll(filepath.Join(unused, "examples", "validation"), 0o750))

	for _, name := range []string{"a.yaml", "b.yaml"} {
		require.NoError(t, os.WriteFile(filepath.Join(unused, "examples", "validation", name), []byte("limit: 1\n"),
			0o600))
	}

	got = validate(environ, unused)
	require.Equal(t, 0, got.code, "%s%s", got.stdout, got.stderr)

	for _, name := range []string{"a.yaml", "b.yaml"} {
		assert.Contains(t, rows(got.stdout), "example warning examples/validation/"+name+": not used: the contract "+
			"declares no validation input (//plg:validation)")
	}

	assert.Contains(t, rows(got.stdout), "validate ok 0 example inputs pass, 2 not used")
	assert.NotContains(t, got.stdout, "2 example inputs pass")

	asJSON = validate(environ, "-format", "json", unused)
	require.Equal(t, 0, asJSON.code, "%s%s", asJSON.stdout, asJSON.stderr)

	report = decodeReport(t, asJSON.stdout)
	require.Len(t, report.Plugins, 1)
	assert.Equal(t, 2, report.Plugins[0].Warnings, "an example not used each")

	none := validate(environ, rule())
	require.Equal(t, 0, none.code, "%s%s", none.stdout, none.stderr)
	assert.Contains(t, rows(none.stdout), "validate ok no example inputs (examples/validation/*.yaml)")
	assert.NotContains(t, none.stdout, "\nexample ")
}

// TestValidateReportsCallsNearTheirLimits validates a rule whose test cases build a string of 0 and of 6 MiB
// (built in a buffer of its own size first) under TightRule's 16 MiB: the resource report shows each call's peak
// memory against the limit, and the bigger one, near 14 MiB, is flagged at or above the default 80% of it, a
// warning (plg validate still exits 0), in the table and the JSON report. With -near-limit 0.95 nothing is. The
// rule is validated in an author repository whose plg.yaml lifts TightRule's 300ms timeout, which growing the
// memory can take under the race detector.
func TestValidateReportsCallsNearTheirLimits(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	files := map[string]string{}

	for _, name := range []string{"plugin.lua", "plugin.yaml", "plg.d.lua", "tests/memory.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "validate", "plugins", "luahog",
			filepath.FromSlash(name)))
		require.NoError(t, err)

		files["luahog/"+name] = string(data)
	}

	luahog := filepath.Join(copyRepo(t, "plugins: [luahog]\nlimits: {timeout: 1h}\n", files), "luahog")

	got := validate(environ, luahog)
	require.Equal(t, 0, got.code, "warnings do not fail validate\n%s%s", got.stdout, got.stderr)
	assert.Regexp(t, `(?m)^resources +ok +tests/memory.yaml: a small string: memory [0-9.]+ MiB of 16.0 MiB `+
		`\([0-9]+%\), time [^ ]+ of 1h0m0s \([0-9]+%\)$`, got.stdout)
	assert.Regexp(t, `(?m)^resources +warning +tests/memory.yaml: a big string: memory 1[34]\.[0-9] MiB of 16.0 MiB `+
		`\(([89][0-9])%\), time [^ ]+ of 1h0m0s \([0-9]+%\); at or above 80% of its memory( and time)? limits?$`,
		got.stdout)

	asJSON := validate(environ, "-format", "json", luahog)
	require.Equal(t, 0, asJSON.code, "%s%s", asJSON.stdout, asJSON.stderr)

	report := decodeReport(t, asJSON.stdout)
	require.Len(t, report.Plugins, 1)
	assert.True(t, report.Valid)
	assert.Equal(t, 1, report.Plugins[0].Warnings, "the big string's resources")

	resources := report.Plugins[0].Resources
	require.Len(t, resources, 2)
	assert.Equal(t, "tests/memory.yaml: a small string", resources[0].Call)
	assert.Empty(t, resources[0].NearLimit)
	assert.Less(t, resources[0].MemoryBytes, uint64(4<<20), "the idle Lua guest")
	assert.Equal(t, "tests/memory.yaml: a big string", resources[1].Call)
	assert.Contains(t, resources[1].NearLimit, "memory")
	assert.Equal(t, uint64(16<<20), resources[1].MemoryLimitBytes)
	assert.GreaterOrEqual(t, resources[1].MemoryBytes, resources[0].MemoryBytes+12<<20,
		"the 6 MiB string and the buffer it is built in")
	assert.Less(t, resources[1].MemoryBytes, resources[1].MemoryLimitBytes)
	assert.InDelta(t, 3600000.0, resources[1].TimeoutMs, 0, "plg.yaml's timeout")

	lenient := validate(environ, "-near-limit", "0.95", luahog)
	require.Equal(t, 0, lenient.code, "%s%s", lenient.stdout, lenient.stderr)
	assert.NotContains(t, lenient.stdout, "warning", "below 95% of the limit")
}

// TestValidateColoursItsTable validates with -color: always colours each result (ok green, FAIL red, a warning
// issue yellow) and nothing else, never (and auto, the default, on a writer that is no terminal) colours nothing.
func TestValidateColoursItsTable(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	coloured := validate(environ, "-color", "always", "noowner")
	require.Equal(t, 0, coloured.code, "%s%s", coloured.stdout, coloured.stderr)
	assert.Contains(t, coloured.stdout, "STEP       RESULT   DETAIL\n", "the header is not coloured")
	assert.Contains(t, coloured.stdout, "manifest   \x1b[33mwarning\x1b[0m  lua plugin noowner implementing ")
	assert.Contains(t, coloured.stdout, "\ncontract   \x1b[32mok\x1b[0m       module example.com/rewards v1.2.0 (")
	assert.Contains(t, coloured.stdout, "\n           \x1b[33mwarning\x1b[0m  plg.ownership.missing (owner): ")

	failed := validate(environ, "-color", "always", "createfails")
	assert.Equal(t, 1, failed.code)
	assert.Contains(
		t,
		failed.stdout,
		"\nload       \x1b[31mFAIL\x1b[0m    plugin Create failed: no cohort configured\n",
	)

	for _, args := range [][]string{{"-color", "never", "noowner"}, {"noowner"}} {
		plain := validate(environ, args...)
		require.Equal(t, 0, plain.code, "%s%s", plain.stdout, plain.stderr)
		assert.NotContains(t, plain.stdout, "\x1b[", args)
		assert.Contains(t, plain.stdout, "manifest   warning  lua plugin noowner implementing ", args)
	}
}
