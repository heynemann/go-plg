// Package rules is a contract whose interfaces carry plg directives, beside the same interfaces without them.
package rules

import "context"

// Rule applies a rule to a number. Its plugins run under plg's default limits.
type Rule interface {
	Apply(ctx context.Context, n int) (int, error)
}

// TightRule is Rule under tight limits.
//
//plg:limits memory=16MiB timeout=300ms pool=1..2
type TightRule interface {
	Apply(ctx context.Context, n int) (int, error)
}

// Parser returns the name of a document's root element. Its Go plugins run in the default Go guest.
type Parser interface {
	Root(ctx context.Context, doc string) (string, error)
}

// XMLParser is Parser whose Go plugins may also import encoding/xml.
//
//plg:guest-packages encoding/xml
type XMLParser interface {
	Root(ctx context.Context, doc string) (string, error)
}

// Timed is Rule whose plugins' Create must finish within 2s; their memory limit is plg's default.
//
//plg:limits timeout=2s
type Timed interface {
	Apply(ctx context.Context, n int) (int, error)
}
