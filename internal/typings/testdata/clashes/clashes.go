// Package clashes is a contract whose names clash: a parameter named like the package its method is the first to
// use, a keyword parameter named like another's renamed keyword, and a generic type instantiated several times,
// two of whose instances would have the same name.
package clashes

import (
	"context"
	"time"
)

// Lease is the interface plugins implement.
type Lease interface {
	Expiry(ctx context.Context, time time.Duration) (time.Time, error)
	Get(ctx context.Context, end string, end_ int) error
	List(ctx context.Context) (Listing, error)
}

// Listing holds pages of different elements.
type Listing struct {
	Items  Page[Item]             `json:"items"`
	Tiers  Page[Tier]             `json:"tiers"`
	Groups Page[[]Item]           `json:"groups"`
	ByName Page[map[string]*Tier] `json:"byName"`
}

// Page is a page of elements.
type Page[T any] struct {
	Entries []T    `json:"entries"`
	Next    string `json:"next"`
}

// Item is an item.
type Item struct {
	Name string `json:"name"`
}

// Tier is a tier.
type Tier struct {
	Level int `json:"level"`
}
