// Package rewards is the rewards platform's contract at major version 2, at the module's root: its import path
// ends in v2, its name is rewards. It declares two interfaces that could be a Host.
package rewards

import "context"

// Provider is implemented by cohort provider plugins.
type Provider interface {
	// IsMember reports whether user belongs to the cohort.
	IsMember(ctx context.Context, user string) (bool, error)
}

// Users is a Host: the platform's user directory.
type Users interface {
	// Tier returns the user's loyalty tier.
	Tier(ctx context.Context, user string) (string, error)
}

// Ledger is another Host: the platform's points ledger.
type Ledger interface {
	// Balance returns the user's points.
	Balance(ctx context.Context, user string) (int, error)
}
