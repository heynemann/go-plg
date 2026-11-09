// Package cohort is a rewards platform's contract for cohort providers: plugins that tell whether a user belongs to
// a cohort and list its users a page at a time.
package cohort

import "context"

// Provider is implemented by cohort provider plugins.
type Provider interface {
	// IsMember reports whether user belongs to the cohort.
	IsMember(ctx context.Context, user string) (bool, error)
	// List returns the page of the cohort's users after cursor ("" for the first).
	List(ctx context.Context, cursor string) (Page, error)
}

// Page is a page of a cohort's users.
type Page struct {
	Users []string `json:"users"`
	Next  string   `json:"next"`
}

// Users is the Host: the platform's user directory.
type Users interface {
	// Tier returns the user's loyalty tier.
	Tier(ctx context.Context, user string) (string, error)
}

// Broken breaks the interface contract: its method takes no context.Context. No plugin can implement it.
type Broken interface {
	Do(name string) error
}
