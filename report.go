package plg

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/heynemann/go-plg/internal/contractref"
)

// Severity says how much an [Issue] matters: [SeverityError] blocks a load, [SeverityWarning] and [SeverityInfo]
// are recorded in its [Report] and logged. Any other value counts as an error.
type Severity string

// The severities of an [Issue].
const (
	// SeverityError blocks the load: Register fails, and a reload keeps the plugin's last good version.
	SeverityError Severity = "error"
	// SeverityWarning is a problem worth fixing that does not stop the plugin from serving.
	SeverityWarning Severity = "warning"
	// SeverityInfo is worth knowing, not a problem.
	SeverityInfo Severity = "info"
)

// blocks reports whether an issue of severity s blocks a load: an error, or a severity plg does not know.
func (s Severity) blocks() bool {
	return s != SeverityWarning && s != SeverityInfo
}

// level returns the slog level an issue of severity s is logged at: an unknown severity's is an error's.
func (s Severity) level() slog.Level {
	if s.blocks() {
		return slog.LevelError
	}

	if s == SeverityWarning {
		return slog.LevelWarn
	}

	return slog.LevelInfo
}

// CodeOwnershipMissing is the [Issue.Code] of a plugin whose plugin.yaml does not name its owning team
// (owner.team.name) or any way to reach it (owner.contact.oncall, owner.contact.channel or owner.contact.email).
// It is a warning, or an error with [WithRequiredOwnership].
const CodeOwnershipMissing = "plg.ownership.missing"

// CodeContractVersion is the [Issue.Code] of a plugin written against a version of its contract module that the
// program's cannot serve (see [Register]): another major version, or a newer minor version. It is an error; its
// message names both versions.
const CodeContractVersion = "plg.contract.version"

// CodeContractDevel is the [Issue.Code] of a plugin loaded by a program whose contract module is built from a
// local checkout, version "(devel)" (tests, go run, a replace directive pointing at a directory), or is missing from
// its build info: the plugin's pinned version cannot be checked. It is a warning.
const CodeContractDevel = "plg.contract.devel"

// Issue is a problem a load found with a plugin version.
type Issue struct {
	// Severity says whether the issue blocks the load.
	Severity Severity
	// Code identifies the kind of issue, such as [CodeOwnershipMissing].
	Code string
	// Message describes the issue for people.
	Message string
	// Field names the input the issue is about, such as owner.team.name; empty when none.
	Field string
}

// Report is the outcome of one attempt to load a plugin version, at Register or on a reload, whether the
// version went live or was rejected, or of an on-demand validation of a version it serves ([Validate]). Its Owner
// shares its slice and map with the plugin's descriptor: do not modify them.
type Report struct {
	// At is when the attempt started.
	At time.Time
	// Err is why the version was rejected, or why its on-demand validation failed; nil when it loaded or passed.
	Err error
	// Owner is the plugin's owner, as its descriptor names it.
	Owner Owner
	// Key is the plugin's key, "<name>/<descriptor key>".
	Key string
	// Version is the version the attempt loaded: the descriptor's, or the content hash of the plugin's files
	// when it has none (empty if the attempt failed before the files were read); the version validated, on demand.
	Version string
	// Issues are the problems the attempt found, in the order found; on demand, those of the validation only.
	Issues []Issue
	// Duration is how long the attempt took.
	Duration time.Duration
}

// newReport starts the report of an attempt to load src under key, started at start.
func newReport(key string, src *source, start time.Time) *Report {
	return &Report{Key: key, Owner: *src.desc.owner(), At: start}
}

// blocking returns the error of r's issues that block the load, wrapping [ErrBlockingIssue]; nil when none do.
func (r *Report) blocking() error {
	var reasons []string

	for _, issue := range r.Issues {
		if issue.Severity.blocks() {
			reasons = append(reasons, issue.Code+": "+issue.Message)
		}
	}

	if reasons == nil {
		return nil
	}

	return fmt.Errorf("%w: %s", ErrBlockingIssue, strings.Join(reasons, "; "))
}

// finishReport ends report, the attempt to load src, with its outcome err, and logs each of its issues at its
// severity's level with the plugin's key, version and owner.
func (c *Container) finishReport(ctx context.Context, report *Report, src *source, err error) {
	report.Version, report.Err, report.Duration = src.desc.Version, err, time.Since(report.At)

	c.logIssues(ctx, report)
}

// logIssues logs each issue of report at its severity's level with the plugin's key, version and owner.
func (c *Container) logIssues(ctx context.Context, report *Report) {
	if len(report.Issues) == 0 {
		return
	}

	owner := report.Owner.logAttr()

	for _, issue := range report.Issues {
		c.logger.LogAttrs(ctx, issue.Severity.level(), "plg: plugin issue", slog.String("key", report.Key), owner,
			slog.String("version", report.Version), slog.String("severity", string(issue.Severity)),
			slog.String("code", issue.Code), slog.String("field", issue.Field), slog.String("issue", issue.Message))
	}
}

// checkOwnership adds to report the [CodeOwnershipMissing] issue, of the given severity, when owner lacks
// team.name or all of contact.oncall, contact.channel and contact.email.
func checkOwnership(report *Report, owner *Owner, severity Severity) {
	noTeam := owner.Team.Name == ""
	noContact := owner.Contact == Contact{}

	var field, missing string

	switch {
	case noTeam && noContact:
		field, missing = "owner", "team.name and one of contact.oncall, contact.channel, contact.email"
	case noTeam:
		field, missing = "owner.team.name", "team.name"
	case noContact:
		field, missing = "owner.contact", "one of contact.oncall, contact.channel, contact.email"
	default:
		return
	}

	report.Issues = append(report.Issues, Issue{
		Severity: severity,
		Code:     CodeOwnershipMissing,
		Message:  "plugin.yaml's owner lacks " + missing,
		Field:    field,
	})
}

// checkContract adds to report the issue of a plugin pinned to ref against the program's contract module built:
// [CodeContractVersion] when built cannot serve the pinned version, [CodeContractDevel] when built is a local
// checkout.
func checkContract(report *Report, built contractref.Module, ref contractref.Ref) {
	if built.Devel() {
		where := "built from a local checkout"
		if built.Dir != "" {
			where = "replaced by the local directory " + built.Dir
		}

		report.Issues = append(report.Issues, Issue{
			Severity: SeverityWarning,
			Code:     CodeContractDevel,
			Message: fmt.Sprintf("the service's contract module %s is %s, version %s: the plugin's %s is not checked",
				built.Path, where, built.Version, ref.Version),
			Field: "implements",
		})

		return
	}

	err := contractref.Compatible(built.Version, ref.Version)
	if err != nil {
		report.Issues = append(report.Issues, Issue{
			Severity: SeverityError,
			Code:     CodeContractVersion,
			Message:  built.Path + ": " + err.Error(),
			Field:    "implements",
		})
	}
}
