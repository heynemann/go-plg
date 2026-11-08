// Package json is a contract named like a global plg declares, for the typings' namespaces.
package json

import "context"

// Codec is the interface plugins implement.
type Codec interface {
	Encode(ctx context.Context, value Value) (string, error)
}

// Value is a value.
type Value struct {
	Text string
}
