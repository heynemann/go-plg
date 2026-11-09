// Package gogoroutine is a cohort provider that warms a cache in goroutines, which the Go guest does not run.
package gogoroutine

import (
	"context"

	"example.com/rewards/cohort"
)

type gold struct{ users cohort.Users }

// Create warms the cache in the background.
func Create(users cohort.Users) (cohort.Provider, error) {
	g := &gold{users: users}
	go g.warm()

	return g, nil
}

func (g *gold) warm() {
	for _, user := range []string{"alice", "bob"} {
		go func() { _, _ = g.users.Tier(context.Background(), user) }()
	}
}

func (g *gold) IsMember(ctx context.Context, user string) (bool, error) {
	tier, err := g.users.Tier(ctx, user)

	return tier == "gold", err
}

func (g *gold) List(context.Context, string) (cohort.Page, error) {
	return cohort.Page{}, nil
}
