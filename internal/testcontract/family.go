package testcontract

import "context"

// Parent and Child are mutually recursive: a Parent holds its Children in a slice, a Child its Parent by
// value. Neither has a field of its own, so a Child's minimum encoding is its Parent's, the length prefix of
// the Children slice, and is known only once both are planned.
type (
	// Parent has Children.
	Parent struct {
		Children []Child
	}
	// Child has a Parent.
	Child struct {
		Parent Parent
	}
)

// Family is implemented by plugins passing mutually recursive structs. Its first method (methods are sorted
// by name) takes a Child: the Go interpreter, reaching Parent first, would lay Child out around a placeholder,
// smaller than its Parent field, and the Go guest rejects such a struct (see internal/wire's fillStruct).
type Family interface {
	// Adopt returns c unchanged.
	Adopt(ctx context.Context, c Child) (Child, error)
	// Mirror returns p unchanged.
	Mirror(ctx context.Context, p Parent) (Parent, error)
}
