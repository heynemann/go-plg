// Package clashes is a contract whose names clash: a parameter named like the package its method is the first to
// use.
package clashes

import (
	"context"
	"time"
)

// Lease is the interface plugins implement.
type Lease interface {
	Expiry(ctx context.Context, time time.Duration) (time.Time, error)
}
