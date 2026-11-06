//go:build integration

package plg_test

import (
	"bufio"
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// Owner blocks for the ownership checks: lacking the team, a contact or both, and the least that passes.
const (
	teamOnlyOwner    = "owner: {team: {name: payments}}\n"
	contactOnlyOwner = "owner: {contact: {oncall: payments-oncall}}\n"
	minimalOwner     = "owner: {team: {name: payments}, contact: {email: payments@example.com}}\n"
)

// writeLuaPlugin writes the Lua hostile-corpus plugin into the plugin directory root/key, with a plugin.yaml of
// version 1 holding manifest's keys too.
func writeLuaPlugin(t *testing.T, root, key, manifest string) {
	t.Helper()

	dir := filepath.Join(root, key)
	require.NoError(t, os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "hostile", "lua", "hostile"))))
	writeManifest(t, root, key, "1", manifest)
}

// writeManifest replaces the plugin.yaml of the Lua plugin directory root/key with one of the given version
// holding manifest's keys too.
func writeManifest(t *testing.T, root, key, version, manifest string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(root, key, "plugin.yaml"),
		[]byte("language: lua\nentrypoint: plugin.lua\nversion: \""+version+"\"\n"+manifest), 0o600))
}

// issueRecord is a "plg: plugin issue" log record.
type issueRecord struct {
	Owner    map[string]any `json:"owner"`
	Level    string         `json:"level"`
	Key      string         `json:"key"`
	Version  string         `json:"version"`
	Severity string         `json:"severity"`
	Code     string         `json:"code"`
	Field    string         `json:"field"`
	Issue    string         `json:"issue"`
}

// issueRecords returns the "plg: plugin issue" records of logs (JSON lines) about key.
func issueRecords(t *testing.T, logs []byte, key string) []issueRecord {
	t.Helper()

	var records []issueRecord

	lines := bufio.NewScanner(bytes.NewReader(logs))
	for lines.Scan() {
		var rec struct {
			issueRecord

			Msg string `json:"msg"`
		}

		require.NoError(t, sonic.Unmarshal(lines.Bytes(), &rec))

		if rec.Msg == "plg: plugin issue" && rec.Key == key {
			records = append(records, rec.issueRecord)
		}
	}

	require.NoError(t, lines.Err())

	return records
}

// assertIssuesLogged checks that the issue records about report's plugin are exactly its issues, each at its
// severity's level with the plugin's version and owner group (nil when the owner sets nothing).
func assertIssuesLogged(t *testing.T, logs []byte, report *plg.Report, owner map[string]any) {
	t.Helper()

	levels := map[plg.Severity]string{plg.SeverityError: "ERROR", plg.SeverityWarning: "WARN", plg.SeverityInfo: "INFO"}
	want := make([]issueRecord, 0, len(report.Issues))

	for _, issue := range report.Issues {
		want = append(want, issueRecord{
			Owner: owner, Level: levels[issue.Severity], Key: report.Key, Version: report.Version,
			Severity: string(issue.Severity), Code: issue.Code, Field: issue.Field, Issue: issue.Message,
		})
	}

	assert.Equal(t, want, append([]issueRecord{}, issueRecords(t, logs, report.Key)...))
}

// TestLoadReportsFlagMissingOwnership registers plugins whose plugin.yaml lacks the owning team, a contact or
// both: each loads, and its load report and the log carry one plg.ownership.missing warning naming what is
// missing. A plugin with a team and a contact loads with no issue.
func TestLoadReportsFlagMissingOwnership(t *testing.T) {
	t.Parallel()

	missingBoth := &plg.Issue{Severity: plg.SeverityWarning, Code: plg.CodeOwnershipMissing, Field: "owner"}
	missingContact := &plg.Issue{Severity: plg.SeverityWarning, Code: plg.CodeOwnershipMissing, Field: "owner.contact"}
	missingTeam := &plg.Issue{Severity: plg.SeverityWarning, Code: plg.CodeOwnershipMissing, Field: "owner.team.name"}
	tests := []struct {
		want     *plg.Issue // nil: no issue
		owner    plg.Owner
		logOwner map[string]any
		name     string
		manifest string
		missing  []string // in the issue's message
	}{
		{
			name: "none", want: missingBoth,
			missing: []string{"team.name", "contact.oncall", "contact.channel", "contact.email"},
		},
		{
			name: "empty", manifest: "owner: {}\n", want: missingBoth,
			missing: []string{"team.name", "contact.oncall", "contact.channel", "contact.email"},
		},
		{
			name: "teamonly", manifest: teamOnlyOwner, want: missingContact,
			owner: plg.Owner{Team: plg.Team{Name: "payments"}}, logOwner: map[string]any{"team": "payments"},
			missing: []string{"contact.oncall", "contact.channel", "contact.email"},
		},
		{
			name: "contactonly", manifest: contactOnlyOwner, want: missingTeam,
			owner:    plg.Owner{Contact: plg.Contact{Oncall: "payments-oncall"}},
			logOwner: map[string]any{"oncall": "payments-oncall"}, missing: []string{"team.name"},
		},
		{
			name: "minimal", manifest: minimalOwner,
			owner: plg.Owner{Team: plg.Team{Name: "payments"}, Contact: plg.Contact{Email: "payments@example.com"}},
		},
		{name: "full", manifest: fullOwner, owner: wantFullOwner()},
	}

	var logs bytes.Buffer

	// slog's JSON handler serializes its writes; the test reads once the container is closed.
	ctr := newContainer(t, plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	reports := make([]plg.Report, len(tests))

	for i, tc := range tests {
		root := t.TempDir()
		writeLuaPlugin(t, root, "p", tc.manifest)

		before := time.Now()

		require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, tc.name,
			plg.WithStorage(plg.NewFSStorage(root)), plg.WithPool(1, 1)), tc.name)

		after := time.Now()

		report, ok := plg.ServingReport(ctr, tc.name+"/p")
		require.True(t, ok, tc.name)

		assert.Equal(t, tc.name+"/p", report.Key, tc.name)
		assert.Equal(t, "1", report.Version, tc.name)
		assert.Equal(t, tc.owner, report.Owner, tc.name)
		require.NoError(t, report.Err, tc.name)
		assert.False(t, report.At.Before(before) || report.At.After(after), "%s: At within the Register", tc.name)
		assert.LessOrEqual(t, report.Duration, after.Sub(before), tc.name)
		assert.Positive(t, report.Duration, tc.name)

		if tc.want == nil {
			assert.Empty(t, report.Issues, tc.name)
		} else {
			require.Len(t, report.Issues, 1, tc.name)

			issue := report.Issues[0]
			assert.Equal(
				t,
				*tc.want,
				plg.Issue{Severity: issue.Severity, Code: issue.Code, Field: issue.Field},
				tc.name,
			)

			for _, field := range tc.missing {
				assert.Contains(t, issue.Message, field, tc.name)
			}
		}

		reports[i] = report
	}

	require.NoError(t, ctr.Close(t.Context())) // every record is written

	for i, tc := range tests {
		assertIssuesLogged(t, logs.Bytes(), &reports[i], tc.logOwner)
	}
}

// TestRequiredOwnershipBlocksRegister registers a plugin without a contact under WithRequiredOwnership: Register
// fails before any instance starts, naming the plugin and the missing fields, and logs the issue as an error. A
// plugin with a team and a contact registers cleanly under the option.
func TestRequiredOwnershipBlocksRegister(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	// slog's JSON handler serializes its writes; the test reads once the container is closed.
	ctr := newContainer(t, plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))

	blocked := t.TempDir()
	writeLuaPlugin(t, blocked, "p", teamOnlyOwner)

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "blocked", plg.WithStorage(plg.NewFSStorage(blocked)),
		plg.WithPool(1, 1), plg.WithRequiredOwnership())
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Contains(t, err.Error(), "blocked/p")
	assert.Contains(t, err.Error(), plg.CodeOwnershipMissing+": plugin.yaml's owner lacks one of contact.oncall, "+
		"contact.channel, contact.email")
	assert.Zero(t, plg.Instances(ctr), "no Create ran")
	assert.Zero(t, plg.MemoryReserved(ctr), "the reservation was given back")

	_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, "blocked/p")
	require.ErrorIs(t, err, plg.ErrNotFound)
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "blocked"), plg.ErrNotFound, "nothing registered")

	owned := t.TempDir()
	writeLuaPlugin(t, owned, "p", minimalOwner)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "owned",
		plg.WithStorage(plg.NewFSStorage(owned)), plg.WithPool(1, 1), plg.WithRequiredOwnership()))

	report, ok := plg.ServingReport(ctr, "owned/p")
	require.True(t, ok)
	assert.Empty(t, report.Issues)

	require.NoError(t, ctr.Close(t.Context())) // every record is written
	assert.Equal(t, []issueRecord{{
		Owner: map[string]any{"team": "payments"}, Level: "ERROR", Key: "blocked/p", Version: "1",
		Severity: "error", Code: plg.CodeOwnershipMissing, Field: "owner.contact",
		Issue: "plugin.yaml's owner lacks one of contact.oncall, contact.channel, contact.email",
	}}, issueRecords(t, logs.Bytes(), "blocked/p"))
	assert.Empty(t, issueRecords(t, logs.Bytes(), "owned/p"))
}

// TestRequiredOwnershipKeepsTheLastGoodVersion reloads, under WithRequiredOwnership, an update that drops the
// plugin's contact: the reload fails and the last good version keeps serving, its instance untouched, while the
// rejection's report is kept beside it until a later reload loads the plugin or finds it unchanged. A new plugin
// rejected for ownership is never served, and its rejection goes once the storages stop listing it; a key a
// listing problem rejects gets a report too.
func TestRequiredOwnershipKeepsTheLastGoodVersion(t *testing.T) {
	t.Parallel()

	root, extra := t.TempDir(), t.TempDir()
	writeLuaPlugin(t, root, "p", fullOwner)

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "owned",
		plg.WithStorage(plg.NewFSStorage(root)), plg.WithStorage(plg.NewFSStorage(extra)), plg.WithPool(1, 1),
		plg.WithRequiredOwnership()))

	// count counts the calls of the plugin's one instance: a version loaded again starts from 1.
	count := func() int {
		t.Helper()

		n, err := getFooer(t, ctr, "owned/p").Count(t.Context())
		require.NoError(t, err)

		return n
	}
	reload := func() error {
		t.Helper()

		return plg.ReloadInterface(t.Context(), ctr, "owned")
	}
	rejection := func(key string) (plg.Report, bool) {
		t.Helper()

		return plg.RejectedReport(ctr, key)
	}

	assert.Equal(t, 1, count())

	served, ok := plg.ServingReport(ctr, "owned/p")
	require.True(t, ok)
	assert.Empty(t, served.Issues)

	_, ok = rejection("owned/p")
	assert.False(t, ok, "no rejection after a good Register")

	// An update without a contact is rejected; version 1 serves on.
	writeManifest(t, root, "p", "2", teamOnlyOwner)

	err := reload()
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Contains(t, err.Error(), "owned/p")
	assert.Equal(t, "1", plg.Version(ctr, "owned/p"))
	assert.Equal(t, 2, count(), "the last good version's instance serves on")

	rejected, ok := rejection("owned/p")
	require.True(t, ok)
	assert.Equal(t, "2", rejected.Version)
	assert.Equal(t, plg.Owner{Team: plg.Team{Name: "payments"}}, rejected.Owner)
	require.ErrorIs(t, rejected.Err, plg.ErrBlockingIssue)
	require.Len(t, rejected.Issues, 1)
	assert.Equal(t, plg.SeverityError, rejected.Issues[0].Severity)
	assert.Equal(t, plg.CodeOwnershipMissing, rejected.Issues[0].Code)
	assert.Equal(t, "owner.contact", rejected.Issues[0].Field)

	stillServed, ok := plg.ServingReport(ctr, "owned/p")
	require.True(t, ok)
	assert.Equal(t, served, stillServed, "the last good version keeps its report")

	// A good version 3 replaces version 1 and clears the rejection.
	writeManifest(t, root, "p", "3", minimalOwner)
	require.NoError(t, reload())
	assert.Equal(t, 1, count(), "version 3 loaded")

	served, ok = plg.ServingReport(ctr, "owned/p")
	require.True(t, ok)
	assert.Equal(t, "3", served.Version)
	assert.Empty(t, served.Issues)

	_, ok = rejection("owned/p")
	assert.False(t, ok, "a good load clears the rejection")

	// Rejected again, then the storage lists version 3 again: unchanged, and the rejection goes.
	writeManifest(t, root, "p", "4", contactOnlyOwner)
	require.ErrorIs(t, reload(), plg.ErrBlockingIssue)

	_, ok = rejection("owned/p")
	require.True(t, ok)

	writeManifest(t, root, "p", "3", minimalOwner)
	require.NoError(t, reload())
	assert.Equal(t, 2, count(), "version 3 was not loaded again")

	_, ok = rejection("owned/p")
	assert.False(t, ok, "listing the served version again clears the rejection")

	// A new plugin without an owner is rejected and never served; it goes when no longer listed.
	writeLuaPlugin(t, extra, "q", "")
	require.ErrorIs(t, reload(), plg.ErrBlockingIssue)

	_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, "owned/q")
	require.ErrorIs(t, err, plg.ErrNotFound)

	_, ok = plg.ServingReport(ctr, "owned/q")
	assert.False(t, ok)

	newRejected, ok := rejection("owned/q")
	require.True(t, ok)
	assert.Equal(t, "1", newRejected.Version)
	require.Len(t, newRejected.Issues, 1)
	assert.Equal(t, "owner", newRejected.Issues[0].Field)

	// The same key in both storages is a listing problem: rejected, and version 3 serves on.
	require.NoError(t, os.RemoveAll(filepath.Join(extra, "q")))
	writeLuaPlugin(t, extra, "p", minimalOwner)
	require.ErrorIs(t, reload(), plg.ErrDuplicateKey)
	assert.Equal(t, 3, count(), "version 3 serves on")

	_, ok = rejection("owned/q")
	assert.False(t, ok, "a key no longer listed has no rejection")

	duplicate, ok := rejection("owned/p")
	require.True(t, ok)
	require.ErrorIs(t, duplicate.Err, plg.ErrDuplicateKey)
	assert.Empty(t, duplicate.Issues)

	require.NoError(t, os.RemoveAll(filepath.Join(extra, "p")))
	require.NoError(t, reload())

	_, ok = rejection("owned/p")
	assert.False(t, ok)
}
