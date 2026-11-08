// Package directive reads the plg directives a contract writes on its interface's doc comment:
//
//	//plg:limits memory=64MiB timeout=2s pool=1..8
//	//plg:guest-packages encoding/xml example.com/lib
//	//plg:validation Input
//
// limits are the resource limits the interface's plugins should be tested under (plg validate runs them under
// those), guest-packages the packages beyond the Go guest's allowlist its Go plugins may import (plg validate
// builds that custom guest, as plg build-guest does), and validation names the data type the registration's
// validation passes to the plugins' Validate. plg gen checks them: a malformed directive fails generation.
//
// plg's library links this package for [Limits], so it imports no guest tooling: [Parse] takes the check of the
// guest packages from its caller.
package directive

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

// ErrDirective reports a malformed plg directive.
var ErrDirective = errors.New("invalid plg directive")

// prefix starts every plg directive. Like any Go directive it has no space after the slashes, so godoc hides it.
const prefix = "//plg:"

// Directives are the plg directives of an interface's doc comment.
type Directives struct {
	// Validation is the type `//plg:validation` names; empty without one.
	Validation string
	// GuestPackages are the extra Go guest packages `//plg:guest-packages` lists, in its order.
	GuestPackages []string
	// Limits are those `//plg:limits` sets; unset ones are zero.
	Limits Limits
}

// Parse reads the plg directives of doc, an interface's doc comment (nil for none): the lines starting with
// //plg:. Each directive may appear once. checkPackages checks the import paths of //plg:guest-packages as plg
// build-guest checks its packages (buildguest.CheckPackages). Errors wrap [ErrDirective] (and checkPackages's
// error) and quote the directive.
func Parse(doc *ast.CommentGroup, checkPackages func(pkgs []string) error) (Directives, error) {
	var dirs Directives

	if doc == nil {
		return dirs, nil
	}

	seen := map[string]bool{}

	for _, comment := range doc.List {
		text, ok := strings.CutPrefix(comment.Text, prefix)
		if !ok {
			continue
		}

		// The name follows the prefix at once: `//plg: limits` names no directive.
		fields := strings.Fields(text)

		var name string

		if len(fields) > 0 && strings.HasPrefix(text, fields[0]) {
			name, fields = fields[0], fields[1:]
		}

		err := fmt.Errorf("%w: repeated; write each directive once", ErrDirective)
		if !seen[name] {
			err = dirs.parse(name, fields, checkPackages)
		}

		if err != nil {
			return Directives{}, fmt.Errorf("%s: %w", comment.Text, err)
		}

		seen[name] = true
	}

	return dirs, nil
}

// parse reads the directive name with its fields into dirs, checking guest packages with checkPackages.
func (dirs *Directives) parse(name string, fields []string, checkPackages func(pkgs []string) error) error {
	var err error

	switch name {
	case "limits":
		dirs.Limits, err = parseLimits(fields)
	case "guest-packages":
		dirs.GuestPackages, err = parsePackages(fields, checkPackages)
	case "validation":
		dirs.Validation, err = parseValidation(fields)
	default:
		err = fmt.Errorf("%w: unknown directive %s%s (want %[2]slimits, %[2]sguest-packages or %[2]svalidation)",
			ErrDirective, prefix, name)
	}

	return err
}

// parseLimits reads the key=value fields of //plg:limits.
func parseLimits(fields []string) (Limits, error) {
	var limits Limits

	if len(fields) == 0 {
		return Limits{}, fmt.Errorf("%w: no limits; write memory=, timeout= or pool=", ErrDirective)
	}

	set := map[string]bool{}

	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")

		switch {
		case !ok:
			return Limits{}, fmt.Errorf("%w: %q is not key=value", ErrDirective, field)
		case set[key]:
			return Limits{}, fmt.Errorf("%w: %s is set twice", ErrDirective, key)
		}

		err := limits.Set(key, value)
		if err != nil {
			return Limits{}, err
		}

		set[key] = true
	}

	return limits, nil
}

// parsePackages reads the import paths of //plg:guest-packages, checked by checkPackages.
func parsePackages(fields []string, checkPackages func(pkgs []string) error) ([]string, error) {
	err := checkPackages(fields)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDirective, err)
	}

	return fields, nil
}

// parseValidation reads the one type name of //plg:validation.
func parseValidation(fields []string) (string, error) {
	if len(fields) != 1 || !token.IsIdentifier(fields[0]) {
		return "", fmt.Errorf("%w: want the name of one type of the contract package", ErrDirective)
	}

	return fields[0], nil
}
