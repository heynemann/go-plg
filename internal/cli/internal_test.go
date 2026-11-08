package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/offline"
)

// TestResolveReturnsCleanAbsolutePaths: a path given to a command resolves against the working directory to a
// clean absolute path, whether that directory is given relative or absolute, so "." names the directory itself
// (its last element is the plugin's key) and git's pathspecs and the author repository's directory compare with
// it; an empty path stays empty (a flag not given). With the working directory gone, a relative path stays
// relative, cleaned.
//
//nolint:paralleltest // t.Chdir: the working directory is the process's
func TestResolveReturnsCleanAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	for _, c := range []struct{ dir, path, want string }{
		{".", ".", root},
		{".", "rules/vip/", filepath.Join(root, "rules", "vip")},
		{"rules", "../lib", filepath.Join(root, "lib")},
		{root, "rules/./vip", filepath.Join(root, "rules", "vip")},
		{"rules", "/elsewhere/x/..", "/elsewhere"},
		{".", "", ""},
	} {
		assert.Equal(t, c.want, resolve(c.dir, c.path), "%s %s", c.dir, c.path)
	}

	gone := filepath.Join(root, "gone")
	require.NoError(t, os.Mkdir(gone, 0o750))
	t.Chdir(gone)
	require.NoError(t, os.Remove(gone))

	assert.Equal(t, filepath.Join("rules", "vip"), resolve(".", "rules/./vip"))
}

// TestWatchTicksReportsChangedFiles drives plg dev's polling watcher with explicit ticks: each tick reports the
// files created, changed and removed under the watched directory and the watched file since the last report, and
// nothing outside them; a tick with nothing changed sends nothing.
func TestWatchTicksReportsChangedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	plugin := filepath.Join(dir, "plugin")
	repo := filepath.Join(dir, "plg.yaml")
	outside := filepath.Join(dir, "outside.txt")

	require.NoError(t, os.MkdirAll(filepath.Join(plugin, "tests"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(plugin, "plugin.lua"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(repo, []byte("plugins: [plugin]\n"), 0o600))

	ticks := make(chan time.Time)
	changes := watchTicks(t.Context(), []string{plugin, repo}, ticks)

	tick := func() { ticks <- time.Time{} }

	test := filepath.Join(plugin, "tests", "example.yaml")
	require.NoError(t, os.WriteFile(test, []byte("cases: []\n"), 0o600))
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o600))
	tick()
	assert.Equal(t, []string{test}, <-changes)

	require.NoError(t, os.WriteFile(filepath.Join(plugin, "plugin.lua"), []byte("ab"), 0o600))
	require.NoError(t, os.WriteFile(repo, []byte("plugins: [plugin, other]\n"), 0o600))
	tick()
	assert.Equal(t, []string{repo, filepath.Join(plugin, "plugin.lua")}, <-changes) // sorted

	require.NoError(t, os.Remove(test))
	tick()
	assert.Equal(t, []string{test}, <-changes)

	for range 2 { // the watcher takes the second tick only if the first one, with nothing changed, sent nothing
		select {
		case ticks <- time.Time{}:
		case changed := <-changes:
			t.Fatalf("nothing changed, yet the watcher reports %v", changed)
		}
	}
}

// TestPollWatchReportsAChange runs plg dev's default watcher, which polls on its own ticker: a file created under the
// watched directory is reported, and the channel closes once the context ends.
func TestPollWatchReportsAChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	changes := pollWatch(ctx, []string{dir})

	file := filepath.Join(dir, "plugin.lua")
	require.NoError(t, os.WriteFile(file, []byte("a"), 0o600))
	assert.Equal(t, []string{file}, <-changes)

	cancel()

	for range changes { // drains until the watcher closes the channel
	}
}

// TestAHangingTokenCommandTimesOut: a token command that never prints its token is killed at the timeout (the
// admin requests' own, in `plg status`) and fails saying so, a child holding its output open included.
func TestAHangingTokenCommandTimesOut(t *testing.T) {
	t.Parallel()

	prof := &profile{Name: "p", Token: &tokenSource{Command: []string{"sh", "-c", "sleep 60 & sleep 60"}}}

	token, err := prof.token(t.Context(), &Env{Dir: t.TempDir()}, 50*time.Millisecond)
	require.ErrorIs(t, err, errTokenCommand)
	assert.Empty(t, token)
	assert.EqualError(t, err,
		`profile p: token command failed: ["sh" "-c" "sleep 60 & sleep 60"] did not finish within 50ms`)
}

// sample is a report of a plugin with every kind of step: ok, a warning holding an issue, one with a subject, and
// a failure with lines, a hint and issues of each severity; its resources hold one call near its memory limit.
func sample() *report {
	plugin := newPluginReport("rules/gold", "/repo/rules/gold")
	plugin.row("manifest", resultWarning, "lua plugin gold implementing example.com/rewards/cohort.Provider@v1.2.0")
	plugin.issue(adminjson.Issue{
		Severity: "warning", Code: "plg.ownership.missing", Message: "no owner",
		Field: "owner",
	})
	plugin.about("test", resultOK, "tests/a.yaml: gold members", "")
	plugin.about("test", resultFailed, "tests/a.yaml: silver members", "IsMember returned other results")
	plugin.line("result: expected true, got false")
	plugin.row("load", resultFailed, "plugin Create failed: boom")
	plugin.hint("if Create takes a Host, pass -host <Ledger|Users>")
	plugin.issue(adminjson.Issue{Severity: "error", Code: "rule.bad", Message: "bad\trule", Field: ""})
	plugin.issue(adminjson.Issue{Severity: "info", Code: "rule.note", Message: "a note", Field: "x"})
	plugin.about("resources", resultWarning, "tests/a.yaml: gold members", "memory 15.0 MiB of 16.0 MiB (94%)")

	plugin.Resources = []*callResources{{
		Call: "tests/a.yaml: gold members", NearLimit: []string{"memory"}, MemoryBytes: 15 << 20,
		MemoryLimitBytes: 16 << 20, DurationMs: durationMs(1500 * time.Microsecond), TimeoutMs: 300,
	}}
	plugin.finish()

	return &report{Version: reportVersion, Repository: "/repo/plg.yaml", Plugins: []*pluginReport{plugin}}
}

func TestPluginReportRecordsStepsAndCountsWarnings(t *testing.T) {
	t.Parallel()

	plugin := sample().Plugins[0]
	assert.False(t, plugin.Valid)
	assert.Equal(t, "test", plugin.FailedStep, "the first step that failed")
	assert.Equal(t, 2, plugin.Warnings, "the manifest's warning issue (once, not with its step), the resources")
	assert.Equal(t, "failed at test", plugin.summary())

	clean := newPluginReport("p", "/p")
	clean.row("manifest", resultOK, "")
	clean.issue(adminjson.Issue{Severity: "info"})
	clean.finish()
	assert.True(t, clean.Valid)
	assert.Zero(t, clean.Warnings, "an info issue is no warning")
	assert.Empty(t, clean.summary())

	warned := newPluginReport("p", "/p")
	warned.row("go.mod", resultWarning, "replaced")
	warned.row("load", resultOK, "")
	warned.issue(adminjson.Issue{Severity: "warning"})
	warned.issue(adminjson.Issue{Severity: "warning"})
	warned.finish()
	warned.finish() // idempotent
	assert.True(t, warned.Valid)
	assert.Equal(t, 3, warned.Warnings)
	assert.Equal(t, "3 warnings", warned.summary())
}

func TestStepsAreATableOfStepsAndTheirDetails(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	require.NoError(t, sample().Plugins[0].steps(&out, false))
	assert.Equal(t, ""+
		"STEP       RESULT   DETAIL\n"+
		"manifest   warning  lua plugin gold implementing example.com/rewards/cohort.Provider@v1.2.0\n"+
		"           warning  plg.ownership.missing (owner): no owner\n"+
		"test       ok       tests/a.yaml: gold members\n"+
		"test       FAIL     tests/a.yaml: silver members: IsMember returned other results\n"+
		"                    result: expected true, got false\n"+
		"load       FAIL     plugin Create failed: boom\n"+
		"           hint     if Create takes a Host, pass -host <Ledger|Users>\n"+
		"           error    rule.bad: bad rule\n"+
		"           info     rule.note (x): a note\n"+
		"resources  warning  tests/a.yaml: gold members: memory 15.0 MiB of 16.0 MiB (94%)\n", out.String())
}

// TestGridAlignsAsTabwriterDoes writes the same rows, wide characters and control characters among them, with grid
// and with the tabwriter table plg status uses: the output is the same.
func TestGridAlignsAsTabwriterDoes(t *testing.T) {
	t.Parallel()

	rows := [][]string{
		{"STEP", "RESULT", "DETAIL"}, {"manifest", "ok", "détail\x1b[2J"}, {"", "warning", ""}, {"ünïcode", "", "x"},
	}

	var want, got bytes.Buffer

	tabs := newTable(&want)
	plain := grid{column: 1}

	for _, row := range rows {
		tabs.row(append([]string(nil), row...)...)
		plain.row(append([]string(nil), row...)...)
	}

	require.NoError(t, tabs.flush())
	require.NoError(t, plain.write(&got))
	assert.Equal(t, want.String(), got.String())
}

func TestGridColoursItsColumnBelowTheHeader(t *testing.T) {
	t.Parallel()

	table := grid{column: 1, colour: true}
	table.row("STEP", "RESULT", "DETAIL")
	table.row("manifest", "ok", "fine")
	table.row("", "warning", "careful")
	table.row("load", "FAIL", "broken")
	table.row("", "error", "an error issue")
	table.row("", "hint", "try this")
	table.row("", "info", "a note")
	table.row("", "", "a line")

	var out bytes.Buffer

	require.NoError(t, table.write(&out))
	assert.Equal(t, ""+
		"STEP      RESULT   DETAIL\n"+
		"manifest  \x1b[32mok\x1b[0m       fine\n"+
		"          \x1b[33mwarning\x1b[0m  careful\n"+
		"load      \x1b[31mFAIL\x1b[0m     broken\n"+
		"          \x1b[31merror\x1b[0m    an error issue\n"+
		"          \x1b[36mhint\x1b[0m     try this\n"+
		"          \x1b[2minfo\x1b[0m     a note\n"+
		"                   a line\n", out.String())
}

// TestJSONReportSchema pins the JSON report a consumer reads, field by field (README, "plg validate").
func TestJSONReportSchema(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	require.NoError(t, sample().writeJSON(&out))
	assert.JSONEq(t, `{
		"version": 1,
		"valid": false,
		"repository": "/repo/plg.yaml",
		"plugins": [{
			"plugin": "rules/gold",
			"dir": "/repo/rules/gold",
			"valid": false,
			"failedStep": "test",
			"warnings": 2,
			"steps": [
				{"step": "manifest", "result": "warning",
					"detail": "lua plugin gold implementing example.com/rewards/cohort.Provider@v1.2.0",
					"issues": [{"severity": "warning", "code": "plg.ownership.missing", "message": "no owner",
						"field": "owner"}]},
				{"step": "test", "result": "ok", "subject": "tests/a.yaml: gold members", "detail": ""},
				{"step": "test", "result": "failed", "subject": "tests/a.yaml: silver members",
					"detail": "IsMember returned other results", "lines": ["result: expected true, got false"]},
				{"step": "load", "result": "failed", "detail": "plugin Create failed: boom",
					"hint": "if Create takes a Host, pass -host <Ledger|Users>",
					"issues": [
						{"severity": "error", "code": "rule.bad", "message": "bad\trule", "field": ""},
						{"severity": "info", "code": "rule.note", "message": "a note", "field": "x"}
					]},
				{"step": "resources", "result": "warning", "subject": "tests/a.yaml: gold members",
					"detail": "memory 15.0 MiB of 16.0 MiB (94%)"}
			],
			"resources": [{"call": "tests/a.yaml: gold members", "nearLimit": ["memory"], "memoryBytes": 15728640,
				"memoryLimitBytes": 16777216, "durationMs": 1.5, "timeoutMs": 300}]
		}]
	}`, out.String())

	single := &report{Version: reportVersion, Valid: true, Plugins: []*pluginReport{newPluginReport("p", "/p")}}
	single.Plugins[0].finish()

	out.Reset()
	require.NoError(t, single.writeJSON(&out))
	assert.JSONEq(t, `{"version": 1, "valid": true, "plugins": [{"plugin": "p", "dir": "/p", "valid": true,
		"warnings": 0, "steps": []}]}`, out.String(), "no repository, no failed step, no resources")
}

// TestJUnitReportHasASuitePerPluginAndACasePerStep writes a repository's report as JUnit: a suite per plugin, a
// test case per step named after it and its subject, failed when the step failed (its detail the message, its
// lines, hint and issues the text), with what an ok or warning step found as its output.
func TestJUnitReportHasASuitePerPluginAndACasePerStep(t *testing.T) {
	t.Parallel()

	rep := sample()
	ok := newPluginReport("rules/silver", "/repo/rules/silver")
	ok.row("manifest", resultOK, "a <plugin> & more")
	ok.finish()

	rep.Plugins = append(rep.Plugins, ok)

	name := filepath.Join(t.TempDir(), "junit.xml")
	require.NoError(t, rep.writeJUnit(name))

	data, err := os.ReadFile(name) //nolint:gosec // G304: the test's file
	require.NoError(t, err)
	assert.Equal(t, `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="plg validate" tests="6" failures="2">
  <testsuite name="rules/gold" tests="5" failures="2">
    <testcase name="manifest" classname="rules/gold">
      <system-out>warning: lua plugin gold implementing example.com/rewards/cohort.Provider@v1.2.0&#xA;warning plg.ownership.missing (owner): no owner</system-out>
    </testcase>
    <testcase name="test: tests/a.yaml: gold members" classname="rules/gold">
      <system-out>ok: </system-out>
    </testcase>
    <testcase name="test: tests/a.yaml: silver members" classname="rules/gold">
      <failure message="IsMember returned other results" type="test">result: expected true, got false</failure>
    </testcase>
    <testcase name="load" classname="rules/gold">
      <failure message="plugin Create failed: boom" type="load">hint: if Create takes a Host, pass -host &lt;Ledger|Users&gt;&#xA;error rule.bad: bad&#x9;rule&#xA;info rule.note (x): a note</failure>
    </testcase>
    <testcase name="resources: tests/a.yaml: gold members" classname="rules/gold">
      <system-out>warning: memory 15.0 MiB of 16.0 MiB (94%)</system-out>
    </testcase>
  </testsuite>
  <testsuite name="rules/silver" tests="1" failures="0">
    <testcase name="manifest" classname="rules/silver">
      <system-out>ok: a &lt;plugin&gt; &amp; more</system-out>
    </testcase>
  </testsuite>
</testsuites>
`, string(data))

	assert.ErrorContains(t, rep.writeJUnit(filepath.Join(t.TempDir(), "missing", "junit.xml")),
		"writing the JUnit report: ")
}

func TestWantsColour(t *testing.T) {
	t.Parallel()

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)

	t.Cleanup(func() { assert.NoError(t, devNull.Close()) })

	env := func(out any, environ ...string) *Env {
		e := &Env{Environ: func() []string { return environ }}
		if file, ok := out.(*os.File); ok {
			e.Stdout = file
		} else {
			e.Stdout = &bytes.Buffer{}
		}

		return e
	}

	assert.True(t, wantsColour(env(nil), colourAlways))
	assert.False(t, wantsColour(env(devNull), colourNever))
	assert.False(t, wantsColour(env(nil), colourAuto), "a buffer is no terminal")
	assert.True(t, wantsColour(env(devNull), colourAuto), "a character device")
	assert.False(t, wantsColour(env(devNull, "NO_COLOR=1"), colourAuto))
	assert.False(t, wantsColour(env(devNull, "TERM=dumb"), colourAuto))
	assert.True(t, wantsColour(env(devNull, "NO_COLOR=1", "TERM=dumb", "NO_COLOR=", "TERM=xterm"), colourAuto),
		"a variable's last value is the one")

	closed, err := os.CreateTemp(t.TempDir(), "closed")
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	assert.False(t, wantsColour(env(closed), colourAuto), "a file that cannot be read")
}

func TestShareOfALimit(t *testing.T) {
	t.Parallel()

	assert.InDelta(t, 0.25, share(uint64(4), uint64(16)), 1e-9)
	assert.InDelta(t, 1.5, share(3*time.Second, 2*time.Second), 1e-9)
	assert.Zero(t, share(uint64(4), uint64(0)), "no limit")
	assert.Equal(t, "1.5 MiB", mebibytes(3<<19))
}

// TestResourcesFlagsACallNearItsTimeout reports three calls: one far from both limits, one at 90% of its timeout
// (flagged for time alone), one near both. Each is a row and an entry of the JSON report's resources; a flagged one
// is a warning naming the limits it is near.
func TestResourcesFlagsACallNearItsTimeout(t *testing.T) {
	t.Parallel()

	run := validation{report: newPluginReport("rules/gold", "/repo/rules/gold"), nearLimit: 0.8}
	run.resources([]measured{
		{call: "tests/a.yaml: quick", used: offline.Resources{
			Memory: 1 << 20, MemoryLimit: 16 << 20, Duration: 10 * time.Millisecond, Timeout: time.Second,
		}},
		{call: "tests/a.yaml: slow", used: offline.Resources{
			Memory: 1 << 20, MemoryLimit: 16 << 20, Duration: 270 * time.Millisecond, Timeout: 300 * time.Millisecond,
		}},
		{call: "tests/a.yaml: heavy and slow", used: offline.Resources{
			Memory: 15 << 20, MemoryLimit: 16 << 20, Duration: 300 * time.Millisecond, Timeout: 300 * time.Millisecond,
		}},
	})
	run.report.finish()

	assert.Equal(t, []*stepReport{
		{
			Step: "resources", Result: resultOK, Subject: "tests/a.yaml: quick",
			Detail: "memory 1.0 MiB of 16.0 MiB (6%), time 10ms of 1s (1%)",
		},
		{
			Step: "resources", Result: resultWarning, Subject: "tests/a.yaml: slow",
			Detail: "memory 1.0 MiB of 16.0 MiB (6%), time 270ms of 300ms (90%); at or above 80% of its time limit",
		},
		{
			Step: "resources", Result: resultWarning, Subject: "tests/a.yaml: heavy and slow",
			Detail: "memory 15.0 MiB of 16.0 MiB (94%), time 300ms of 300ms (100%); at or above 80% of its memory " +
				"and time limits",
		},
	}, run.report.Steps)
	assert.Equal(t, []*callResources{
		{
			Call:             "tests/a.yaml: quick",
			MemoryBytes:      1 << 20,
			MemoryLimitBytes: 16 << 20,
			DurationMs:       10,
			TimeoutMs:        1000,
		},
		{
			Call: "tests/a.yaml: slow", NearLimit: []string{"time"}, MemoryBytes: 1 << 20, MemoryLimitBytes: 16 << 20,
			DurationMs: 270, TimeoutMs: 300,
		},
		{
			Call: "tests/a.yaml: heavy and slow", NearLimit: []string{"memory", "time"}, MemoryBytes: 15 << 20,
			MemoryLimitBytes: 16 << 20, DurationMs: 300, TimeoutMs: 300,
		},
	}, run.report.Resources)
	assert.Equal(t, 2, run.report.Warnings)
	assert.True(t, run.report.Valid, "calls near their limits are warnings")
}

// TestDescribeShowsTheProvenance shows the provenance of a plugin loaded from an artifact on a row of plg status's
// table: its commit and whether the files differed from it (or that there was no commit), its contract module,
// the plg that packed it and its digest. A plugin without provenance has no such row.
func TestDescribeShowsTheProvenance(t *testing.T) {
	t.Parallel()

	provenance := &adminjson.Provenance{
		Contract: adminjson.Module{Path: "example.com/rewards", Version: "v1.2.0"},
		Digest:   "sha256:00ff", Commit: "0123abcd", Plg: "v0.9.0", Dirty: true,
	}
	plugin := adminjson.Plugin{Key: "rules/silver", Registration: "rules", Status: "serving", Provenance: provenance}

	for _, tc := range []struct {
		commit string
		want   string
		dirty  bool
	}{
		{"0123abcd", "commit 0123abcd (dirty)", true},
		{"0123abcd", "commit 0123abcd", false},
		{"", "no git commit", false},
	} {
		provenance.Commit, provenance.Dirty = tc.commit, tc.dirty

		var out bytes.Buffer

		require.NoError(t, describe(&out, &plugin))
		assert.Contains(t, out.String(), "\nprovenance    "+tc.want+", contract example.com/rewards v1.2.0, packed "+
			"by plg v0.9.0, digest sha256:00ff\n")
	}

	plugin.Provenance = nil

	var out bytes.Buffer

	require.NoError(t, describe(&out, &plugin))
	assert.NotContains(t, out.String(), "provenance")
}

// TestDescribeShowsAReportsIssues shows a report on a row of plg status's table, then each of its issues on a row
// of its own, as plg validate writes them: severity, code, field (if any) and message.
func TestDescribeShowsAReportsIssues(t *testing.T) {
	t.Parallel()

	failed := "plugin Validate failed"
	plugin := adminjson.Plugin{
		Key: "rules/silver", Registration: "rules", Status: "rejected",
		Rejection: &adminjson.Report{
			At: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), Version: "3", Error: &failed,
			Issues: []adminjson.Issue{
				{Severity: "error", Code: "rules.tier", Message: "unknown tier", Field: "tier"},
				{Severity: "warning", Code: "rules.slow", Message: "takes a while"},
			},
		},
	}

	var out bytes.Buffer

	require.NoError(t, describe(&out, &plugin))
	assert.Contains(t, out.String(), "\nrejection     2026-11-08T12:00:00Z version 3: plugin Validate failed\n"+
		"              error rules.tier (tier): unknown tier\n"+
		"              warning rules.slow: takes a while\n")
}
