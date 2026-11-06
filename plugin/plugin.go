// Package plugin is what Go plugins import from plg itself: the [Issue] their optional Validate method returns.
// It is compiled into the Go guest's symbol tables (custom guests built by `plg build-guest` included), so a
// plugin imports it by its import path, though its source is not part of the plugin:
//
//	import "github.com/heynemann/go-plg/plugin"
//
//	func (r *rule) Validate(ctx context.Context, in contract.Limits) ([]plugin.Issue, error) {
//		if r.max > in.Max {
//			return []plugin.Issue{{Severity: plugin.Error, Code: "rule.max", Message: "max over the limit", Field: "max"}}, nil
//		}
//
//		return nil, nil
//	}
//
// A service registering its interface with plg.WithValidation requires every plugin's instance (what Create
// returns) to have that method, taking the validation input type the service chose; plg calls it in a separate
// instance before the plugin goes live, and an error-severity issue keeps it from loading.
package plugin

// Severity says how much an [Issue] matters.
type Severity string

// The severities of an [Issue]. plg counts any other value as [Error].
const (
	// Error blocks the plugin's load.
	Error Severity = "error"
	// Warning is a problem worth fixing that does not keep the plugin from serving.
	Warning Severity = "warning"
	// Info is worth knowing, not a problem.
	Info Severity = "info"
)

// Issue is a problem a plugin's Validate found with itself. plg records it in the load report as a plg.Issue with
// the same fields and logs it with the plugin's key, version and owner.
type Issue struct {
	// Severity says whether the issue blocks the load.
	Severity Severity
	// Code identifies the kind of issue, such as "rule.max"; plg's own codes start with "plg.".
	Code string
	// Message describes the issue for people.
	Message string
	// Field names the input or setting the issue is about; empty when none.
	Field string
}
