// Package recursive is a contract whose recursive type reaches a time.Time only after its recursion.
package recursive

import (
	"context"
	"time"
)

// ValueFirst hands A back by value before handing it back through a pointer.
type ValueFirst interface {
	Alpha(ctx context.Context) (A, error)
	Beta(ctx context.Context) (*A, error)
}

// PointerFirst hands A back through a pointer before handing it back by value.
type PointerFirst interface {
	Alpha(ctx context.Context) (*A, error)
	Beta(ctx context.Context) (A, error)
}

// A refers to itself before its time.
type A struct {
	Next *A
	At   time.Time
}
