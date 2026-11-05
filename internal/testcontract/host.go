package testcontract

import (
	"context"
	"time"
)

// User is what Host.Lookup returns: a struct with a nested pointer, a slice and a time.
type User struct {
	Since   time.Time `json:"since"`
	Manager *User     `json:"manager"`
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Tags    []string  `json:"tags"`
}

// Host is the service side the Hosted test plugins call back into, registered with plg.WithHost[Host].
type Host interface {
	// Lookup returns the user with the given id.
	Lookup(ctx context.Context, id string) (User, error)
	// Value returns what the service finds in ctx, so tests can tell which context a host call ran with.
	Value(ctx context.Context) (string, error)
	// Fail returns an error with message msg, or nil when msg is empty.
	Fail(ctx context.Context, msg string) error
	// Sum returns base plus every value of more.
	Sum(ctx context.Context, base int, more ...int) (int, error)
	// Keys returns keys unchanged: a map whose values encode as nothing.
	Keys(ctx context.Context, keys map[string]struct{}) (map[string]struct{}, error)
	// Size returns the length of b, or -1 when b is nil.
	Size(ctx context.Context, b []byte) (int, error)
}

// Hosted is implemented by plugins whose Create takes the Host.
type Hosted interface {
	// Describe returns the host's user with the given id, its name prefixed with "described ".
	Describe(ctx context.Context, id string) (User, error)
	// Relay calls Host.Fail with msg and returns the message of the error it got, or "" without one.
	Relay(ctx context.Context, msg string) (string, error)
	// Value returns what Host.Value returns.
	Value(ctx context.Context) (string, error)
	// Origin returns what Host.Value returned while Create ran.
	Origin(ctx context.Context) (string, error)
	// Total returns Host.Sum(base, more...).
	Total(ctx context.Context, base int, more ...int) (int, error)
	// Keys returns Host.Keys(keys).
	Keys(ctx context.Context, keys map[string]struct{}) (map[string]struct{}, error)
}
