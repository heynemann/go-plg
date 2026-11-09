package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/heynemann/go-plg/internal/adminjson"
)

// none stands for an empty value in a table.
const none = "-"

// listPlugins writes plugins as a table, one row each.
func listPlugins(out io.Writer, plugins []adminjson.Plugin) error {
	table := newTable(out)
	table.row("KEY", "STATUS", "VERSION", "LANGUAGE", "TEAM", "PANICS", "PROBLEM")

	for i := range plugins {
		p := &plugins[i]
		table.row(p.Key, p.Status, orNone(p.Version), orNone(p.Language), orNone(p.Owner.Team.Name),
			strconv.FormatUint(p.Panics, 10), orNone(problem(p)))
	}

	return table.flush()
}

// problem is what keeps the plugin from serving as listed: its failed validation's error, else its rejection's.
func problem(p *adminjson.Plugin) string {
	for _, report := range []*adminjson.Report{p.Validation, p.Rejection} {
		if report != nil && report.Error != nil {
			return *report.Error
		}
	}

	return ""
}

// describe writes one plugin as a table of its fields, leaving out those it has not.
func describe(out io.Writer, plugin *adminjson.Plugin) error {
	table := newTable(out)
	table.row("key", plugin.Key)
	table.row("registration", plugin.Registration)
	table.row("status", plugin.Status)
	table.row("version", orNone(plugin.Version))
	table.row("language", orNone(plugin.Language))
	table.row("storage", orNone(plugin.Storage))

	if plugin.LoadedAt != nil {
		table.row("loaded", stamp(*plugin.LoadedAt))
	}

	if provenance := plugin.Provenance; provenance != nil {
		table.row("provenance", describeProvenance(provenance))
	}

	describeOwner(table, &plugin.Owner)

	if pool := plugin.Pool; pool != nil {
		table.row("pool", fmt.Sprintf("%d instances (min %d, max %d), %d in use, %d calls in flight, %d waiting",
			pool.Instances, pool.Min, pool.Max, pool.InUse, pool.CallsInFlight, pool.Waiting))
		table.row("memory", fmt.Sprintf("%d bytes (limit %d per instance)", pool.Memory, pool.MemoryLimit))
	}

	table.row("panics", strconv.FormatUint(plugin.Panics, 10))

	if last := plugin.LastPanic; last != nil {
		table.row("last panic", stamp(last.Time)+" "+last.Message)
	}

	describeReport(table, "load", plugin.Load)
	describeReport(table, "validation", plugin.Validation)
	describeReport(table, "rejection", plugin.Rejection)

	return table.flush()
}

// describeProvenance says where an artifact's plugin comes from: its commit (and whether the files differed from
// it), its contract module, the plg that packed it and its content digest.
func describeProvenance(provenance *adminjson.Provenance) string {
	commit := "no git commit"
	if provenance.Commit != "" {
		commit = "commit " + provenance.Commit
	}

	if provenance.Dirty {
		commit += " (dirty)"
	}

	return fmt.Sprintf("%s, contract %s %s, packed by plg %s, digest %s", commit, provenance.Contract.Path,
		provenance.Contract.Version, provenance.Plg, provenance.Digest)
}

// describeOwner adds the owner's fields that are set to table.
func describeOwner(table *table, owner *adminjson.Owner) {
	team := owner.Team.Name
	if owner.Team.Department != "" {
		team += " (" + owner.Team.Department + ")"
	}

	for _, field := range [][2]string{
		{"team", team},
		{"org", owner.Org},
		{"company", owner.Company},
		{"oncall", owner.Contact.Oncall},
		{"channel", owner.Contact.Channel},
		{"email", owner.Contact.Email},
	} {
		if field[1] != "" {
			table.row(field[0], field[1])
		}
	}

	for _, m := range owner.Maintainers {
		table.row("maintainer", strings.Join(strings.Fields(m.Name+" "+angled(m.Email)+" "+m.Handle), " "))
	}

	for _, link := range [][2]string{
		{"repository", owner.Links.Repository},
		{"docs", owner.Links.Docs},
		{"runbook", owner.Links.Runbook},
		{"issues", owner.Links.Issues},
	} {
		if link[1] != "" {
			table.row(link[0], link[1])
		}
	}

	for _, name := range slices.Sorted(maps.Keys(owner.Labels)) {
		table.row("label", name+"="+owner.Labels[name])
	}
}

// angled returns email in angle brackets; nothing for none.
func angled(email string) string {
	if email == "" {
		return ""
	}

	return "<" + email + ">"
}

// describeReport adds the report named name to table, with one row per issue; nothing for a nil report.
func describeReport(table *table, name string, report *adminjson.Report) {
	if report == nil {
		return
	}

	outcome := "ok"
	if report.Error != nil {
		outcome = *report.Error
	}

	table.row(name, fmt.Sprintf("%s version %s: %s", stamp(report.At), orNone(report.Version), outcome))

	for _, issue := range report.Issues {
		table.row("", issue.Severity+" "+issueText(&issue))
	}
}

// listEvents writes events as a table, one row each.
func listEvents(out io.Writer, events []adminjson.Event) error {
	table := newTable(out)
	table.row("AT", "KIND", "VERSION", "DETAIL")

	for i := range events {
		event := &events[i]
		table.row(stamp(event.At), event.Kind, orNone(event.Version), orNone(detail(event)))
	}

	return table.flush()
}

// detail is what an event says beyond its kind: its panic's message, its report's error, or its report's issues.
func detail(event *adminjson.Event) string {
	switch {
	case event.Panic != nil:
		return event.Panic.Message
	case event.Report == nil:
		return ""
	case event.Report.Error != nil:
		return *event.Report.Error
	}

	issues := make([]string, len(event.Report.Issues))
	for i, issue := range event.Report.Issues {
		issues[i] = fmt.Sprintf("%s %s: %s", issue.Severity, issue.Code, issue.Message)
	}

	return strings.Join(issues, "; ")
}

// table writes aligned columns.
type table struct {
	writer *tabwriter.Writer
	err    error // the first write's failure
}

func newTable(out io.Writer) *table {
	return &table{writer: tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)}
}

// row writes one row; each cell on one line, without control characters (plugins write their messages).
func (t *table) row(cells ...string) {
	for i, cell := range cells {
		cells[i] = oneLine(cell)
	}

	if t.err == nil {
		_, t.err = io.WriteString(t.writer, strings.Join(cells, "\t")+"\n")
	}
}

// flush writes what the rows hold; it returns the first failure.
func (t *table) flush() error {
	if t.err == nil {
		t.err = t.writer.Flush()
	}

	return t.err
}

// oneLine returns s on one line: runs of white space become one space, and other control characters (an escape
// sequence a plugin's message could hold would drive the terminal) U+FFFD.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return unicode.ReplacementChar
		}

		return r
	}, strings.Join(strings.Fields(s), " "))
}

// orNone returns s, or none for an empty s.
func orNone(s string) string {
	if s == "" {
		return none
	}

	return s
}

// stamp formats a time for a table: RFC 3339 in UTC, to the second.
func stamp(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}
