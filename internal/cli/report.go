package cli

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bytedance/sonic"

	"github.com/heynemann/go-plg/internal/adminjson"
)

// reportVersion is the version of plg validate's JSON report: it changes only when a field is removed or changes
// meaning; fields may be added within a version.
const reportVersion = 1

// Step results, in the JSON report.
const (
	resultOK      = "ok"
	resultFailed  = "failed"
	resultWarning = "warning"
)

// report is what plg validate found, and its JSON report (-format json), documented in the README.
type report struct {
	// Repository is the plg.yaml of the author repository validated; empty for a plugin validated on its own.
	Repository string `json:"repository,omitempty"`
	// Plugins are the plugins validated, in the repository's order.
	Plugins []*pluginReport `json:"plugins"`
	// Version is [reportVersion].
	Version int `json:"version"`
	// Valid reports whether every plugin is valid.
	Valid bool `json:"valid"`
}

// pluginReport is the validation of one plugin: its steps in the order they ran, which also records each step as
// it runs.
type pluginReport struct {
	// Plugin is the plugin as named: plg validate's operand, or its entry in the repository's plg.yaml.
	Plugin string `json:"plugin"`
	// Dir is the plugin's directory.
	Dir string `json:"dir"`
	// FailedStep is the first step that failed; empty when the plugin is valid.
	FailedStep string `json:"failedStep,omitempty"`
	// Steps are the steps that ran, a test case or an example input being one each; the steps after a failed
	// one do not run, except that the test cases, Validate's examples and the resource report run together
	// once the plugin loaded.
	Steps []*stepReport `json:"steps"`
	// Resources are what each test case's call took against the limits.
	Resources []*callResources `json:"resources,omitempty"`
	// Warnings counts the warnings: the issues of warning severity, and the steps with a warning result that hold
	// none.
	Warnings int `json:"warnings"`
	// Valid reports whether no step failed.
	Valid bool `json:"valid"`
}

// stepReport is one step of a validation.
type stepReport struct {
	// Step names the step: plg.yaml, manifest, shared, contract, interface, limits, go.mod, sandbox, types,
	// guest, typings, syntax, load, example, validate, test, tests, resources.
	Step string `json:"step"`
	// Result is ok, failed or warning.
	Result string `json:"result"`
	// Subject is what a step that repeats is about: a test case's file and name, an example's file.
	Subject string `json:"subject,omitempty"`
	// Detail says what the step found, or why it failed.
	Detail string `json:"detail"`
	// Hint suggests how to fix a failure.
	Hint string `json:"hint,omitempty"`
	// Lines show a failure: a test case's expected and actual values, the stubs' problems.
	Lines []string `json:"lines,omitempty"`
	// Issues are the issues the step reported: the load's, Validate's for an example.
	Issues []adminjson.Issue `json:"issues,omitempty"`
}

// callResources is what a test case's call took against the plugin's limits.
type callResources struct {
	// Call is the test case: its file and name.
	Call string `json:"call"`
	// NearLimit lists the limits the call came near (at or above -near-limit of it): memory, time.
	NearLimit []string `json:"nearLimit,omitempty"`
	// MemoryBytes is the instance's peak memory, Create's included.
	MemoryBytes uint64 `json:"memoryBytes"`
	// MemoryLimitBytes is the instance's memory limit.
	MemoryLimitBytes uint64 `json:"memoryLimitBytes"`
	// DurationMs is how long the call took, in milliseconds (Create not included).
	DurationMs float64 `json:"durationMs"`
	// TimeoutMs is the call's timeout, in milliseconds.
	TimeoutMs float64 `json:"timeoutMs"`
}

// newPluginReport returns the report of the plugin dir, named plugin.
func newPluginReport(plugin, dir string) *pluginReport {
	return &pluginReport{Plugin: plugin, Dir: dir, Steps: []*stepReport{}}
}

// row records a step: its result and detail.
func (p *pluginReport) row(step, result, detail string) {
	p.about(step, result, "", detail)
}

// about records a step about subject.
func (p *pluginReport) about(step, result, subject, detail string) {
	if result == resultFailed && p.FailedStep == "" {
		p.FailedStep = step
	}

	p.Steps = append(p.Steps, &stepReport{Step: step, Result: result, Subject: subject, Detail: detail})
}

// last returns the step recorded last.
func (p *pluginReport) last() *stepReport {
	return p.Steps[len(p.Steps)-1]
}

// line adds a line showing the failure of the step recorded last.
func (p *pluginReport) line(text string) {
	step := p.last()
	step.Lines = append(step.Lines, text)
}

// hint adds a hint to the step recorded last.
func (p *pluginReport) hint(text string) {
	p.last().Hint = text
}

// issue adds an issue to the step recorded last.
func (p *pluginReport) issue(issue adminjson.Issue) {
	step := p.last()
	step.Issues = append(step.Issues, issue)
}

// finish sets what the steps add up to: whether the plugin is valid and its warnings.
func (p *pluginReport) finish() {
	p.Valid = p.FailedStep == ""
	p.Warnings = 0

	for _, step := range p.Steps {
		warned := false

		for _, issue := range step.Issues {
			if issue.Severity == resultWarning {
				p.Warnings++
				warned = true
			}
		}

		if step.Result == resultWarning && !warned {
			p.Warnings++
		}
	}
}

// summary describes the validation in a word: the step that failed, or the warnings.
func (p *pluginReport) summary() string {
	switch {
	case p.FailedStep != "":
		return "failed at " + p.FailedStep
	case p.Warnings > 0:
		return strconv.Itoa(p.Warnings) + " " + plural(p.Warnings, "warning", "warnings")
	default:
		return ""
	}
}

// output is how plg validate shows its report: on stdout as a coloured or plain table or as JSON, and in a JUnit
// file.
type output struct {
	junit  string // the JUnit file to write; empty writes none
	format string // formatTable or formatJSON
	colour bool
}

// colours of the results in a terminal.
const (
	colourReset  = "\x1b[0m"
	colourRed    = "\x1b[31m"
	colourGreen  = "\x1b[32m"
	colourYellow = "\x1b[33m"
	colourCyan   = "\x1b[36m"
	colourDim    = "\x1b[2m"
)

// Results in the terminal.
const (
	shownFailed = "FAIL"
	shownHint   = "hint"
)

// colourOf returns the colour a result is shown in.
func colourOf(result string) string {
	switch result {
	case resultOK:
		return colourGreen
	case shownFailed, "error":
		return colourRed
	case resultWarning:
		return colourYellow
	case shownHint:
		return colourCyan
	default:
		return colourDim
	}
}

// grid writes rows of cells aligned in columns two spaces apart, as text/tabwriter does; when colour is set, the
// cells of column below the first row (the header) are shown in the colour of their text. Each cell is shown on
// one line, without control characters (plugins write their messages).
type grid struct {
	rows   [][]string
	column int
	colour bool
}

func (g *grid) row(cells ...string) {
	for i, cell := range cells {
		cells[i] = oneLine(cell)
	}

	g.rows = append(g.rows, cells)
}

func (g *grid) write(out io.Writer) error {
	widths := g.widths()

	var text strings.Builder

	for r, row := range g.rows {
		for i, cell := range row {
			shown := cell
			if g.colour && r > 0 && i == g.column && cell != "" { // the first row is the header
				shown = colourOf(cell) + cell + colourReset
			}

			text.WriteString(shown)

			if i < len(row)-1 {
				text.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)+2))
			}
		}

		text.WriteByte('\n')
	}

	_, err := io.WriteString(out, text.String())

	return err //nolint:wrapcheck // the command prefixes its name
}

// widths returns the width of each column but the last: its widest cell's.
func (g *grid) widths() []int {
	var widths []int

	for _, row := range g.rows {
		for i, cell := range row[:len(row)-1] {
			if i == len(widths) {
				widths = append(widths, 0)
			}

			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}

	return widths
}

// steps writes the plugin's steps as a table: a row per step (its subject before its detail), then a row per
// line, its hint and a row per issue.
func (p *pluginReport) steps(out io.Writer, colour bool) error {
	table := grid{column: 1, colour: colour}
	table.row("STEP", "RESULT", "DETAIL")

	for _, step := range p.Steps {
		result := step.Result
		if result == resultFailed {
			result = shownFailed
		}

		detail := step.Detail
		if step.Subject != "" {
			detail = strings.TrimSuffix(step.Subject+": "+detail, ": ")
		}

		table.row(step.Step, result, detail)

		for _, line := range step.Lines {
			table.row("", "", line)
		}

		if step.Hint != "" {
			table.row("", shownHint, step.Hint)
		}

		for _, issue := range step.Issues {
			table.row("", issue.Severity, issueText(&issue))
		}
	}

	return table.write(out)
}

// issueText writes an issue after its severity: its code, field (if any) and message.
func issueText(issue *adminjson.Issue) string {
	at := ""
	if issue.Field != "" {
		at = " (" + issue.Field + ")"
	}

	return issue.Code + at + ": " + issue.Message
}

// writeJSON writes the report as indented JSON.
func (r *report) writeJSON(out io.Writer) error {
	encoder := sonic.ConfigStd.NewEncoder(out)
	encoder.SetIndent("", "  ")

	return encoder.Encode(r) //nolint:wrapcheck // the command prefixes its name
}

// JUnit's XML, as CI systems read it: a test suite per plugin, a test case per step.
type (
	junitSuites struct {
		XMLName  xml.Name     `xml:"testsuites"`
		Name     string       `xml:"name,attr"`
		Suites   []junitSuite `xml:"testsuite"`
		Tests    int          `xml:"tests,attr"`
		Failures int          `xml:"failures,attr"`
	}
	junitSuite struct {
		Name     string      `xml:"name,attr"`
		Cases    []junitCase `xml:"testcase"`
		Tests    int         `xml:"tests,attr"`
		Failures int         `xml:"failures,attr"`
	}
	junitCase struct {
		Failure   *junitFailure `xml:"failure,omitempty"`
		Name      string        `xml:"name,attr"`
		ClassName string        `xml:"classname,attr"`
		SystemOut string        `xml:"system-out,omitempty"`
	}
	junitFailure struct {
		Message string `xml:"message,attr"`
		Type    string `xml:"type,attr"`
		Text    string `xml:",chardata"`
	}
)

// writeJUnit writes the report as JUnit XML to the file name: a test suite per plugin, named as the report names
// it, and a test case per step, named after the step (and its subject), failed when the step failed; what the
// step found is the case's output, and its message the failure's.
func (r *report) writeJUnit(name string) error {
	suites := junitSuites{Name: "plg validate"}

	for _, plugin := range r.Plugins {
		suite := junitSuite{Name: plugin.Plugin}

		for _, step := range plugin.Steps {
			suite.Cases = append(suite.Cases, step.junit(plugin.Plugin))
			if step.Result == resultFailed {
				suite.Failures++
			}
		}

		suite.Tests = len(suite.Cases)
		suites.Tests += suite.Tests
		suites.Failures += suite.Failures
		suites.Suites = append(suites.Suites, suite)
	}

	data, err := xml.MarshalIndent(&suites, "", "  ")
	if err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}

	err = os.WriteFile(name, append([]byte(xml.Header), append(data, '\n')...), reportFileMode)
	if err != nil {
		return fmt.Errorf("writing the JUnit report: %w", err)
	}

	return nil
}

// reportFileMode is the JUnit report's mode: read and written by its owner only.
const reportFileMode = 0o600

// junit returns the step as a JUnit test case of the plugin's suite.
func (s *stepReport) junit(plugin string) junitCase {
	name := s.Step
	if s.Subject != "" {
		name += ": " + s.Subject
	}

	text := []string{s.Result + ": " + s.Detail}
	text = append(text, s.Lines...)

	if s.Hint != "" {
		text = append(text, "hint: "+s.Hint)
	}

	for _, issue := range s.Issues {
		text = append(text, issue.Severity+" "+issueText(&issue))
	}

	tc := junitCase{Name: name, ClassName: plugin}
	if s.Result == resultFailed {
		tc.Failure = &junitFailure{Message: s.Detail, Type: s.Step, Text: strings.Join(text[1:], "\n")}
	} else {
		tc.SystemOut = strings.Join(text, "\n")
	}

	return tc
}

// show shows the report: on out, as JSON with -format json, and in the JUnit file. A repository's table was
// written plugin by plugin as each was validated (see [output.plugin]); it ends with the summary.
func (o *output) show(out io.Writer, rep *report) error {
	var err error

	switch {
	case o.format == formatJSON:
		err = rep.writeJSON(out)
	case rep.Repository != "":
		err = o.summary(out, rep)
	default:
	}

	if err == nil && o.junit != "" {
		err = rep.writeJUnit(o.junit)
	}

	return err
}

// plugin writes the table of a plugin just validated (nothing with -format json): its steps, after its name and
// before a blank line in a repository.
func (o *output) plugin(out io.Writer, rep *report, plugin *pluginReport) error {
	if o.format == formatJSON {
		return nil
	}

	if rep.Repository == "" {
		return plugin.steps(out, o.colour)
	}

	_, err := fmt.Fprintf(out, "%s\n", plugin.Plugin)
	if err == nil {
		err = plugin.steps(out, o.colour)
	}

	if err == nil {
		_, err = fmt.Fprintln(out)
	}

	return err //nolint:wrapcheck // the command prefixes its name
}

// summary writes a repository's summary table: a row per plugin.
func (o *output) summary(out io.Writer, rep *report) error {
	table := grid{column: 1, colour: o.colour}
	table.row("PLUGIN", "RESULT", "DETAIL")

	for _, plugin := range rep.Plugins {
		result := resultOK
		if !plugin.Valid {
			result = shownFailed
		}

		table.row(plugin.Plugin, result, plugin.summary())
	}

	return table.write(out)
}

// durationMs is d in milliseconds.
func durationMs(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
