package testcontract

import "context"

// Counter is the struct the Embedded alias denotes.
type Counter struct {
	N int `json:"n"`
}

// Embedded is an alias of Counter. A struct embedding it has a field named Embedded whose type is Counter, a
// different struct type than one embedding Counter: reflection reports only Counter.
type Embedded = Counter

// AliasHost is the service side the Aliased test plugin calls back into.
type AliasHost interface {
	// Double returns c's N doubled.
	Double(ctx context.Context, c struct {
		Embedded

		By int
	}) (struct{ *Embedded }, error)
}

// Aliased is implemented by plugins whose signatures embed an alias in anonymous structs, whose Create takes
// the AliasHost.
type Aliased interface {
	// Bump returns c with N incremented by By.
	Bump(ctx context.Context, c struct {
		Embedded

		By int
	}) (struct {
		Embedded

		By int
	}, error)
	// Double returns what AliasHost.Double returns for c.
	Double(ctx context.Context, c struct {
		Embedded

		By int
	}) (struct{ *Embedded }, error)
}
