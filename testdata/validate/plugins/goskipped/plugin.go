// Package goskipped is a cohort provider of the gold tier, beside files the Go guest does not load (a name starting
// with "_" or ".", one ending in a GOOS yaegi knows, a generator `// +build ignore` excludes) and with files it
// does load although go build would not: names ending in a GOOS or GOARCH yaegi does not know (tier_riscv64.go,
// users_hurd.go). Every plg validate step reads the files the guest loads.
package goskipped

import (
	"context"

	"example.com/rewards/cohort"
)

// Create keeps the user directory.
func Create(users cohort.Users) (cohort.Provider, error) {
	return &gold{users: users}, nil
}

func (g *gold) IsMember(ctx context.Context, user string) (bool, error) {
	tier, err := g.users.Tier(ctx, user)

	return tier == goldTier, err
}

func (g *gold) List(context.Context, string) (cohort.Page, error) {
	return cohort.Page{}, nil
}
