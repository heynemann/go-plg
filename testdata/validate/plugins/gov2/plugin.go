// Package gov2 is a cohort provider of gold users, importing the v2 contract without a name: rewards is the
// package's name, not the last element of its path.
package gov2

import (
	"context"

	"example.com/rewards/v2"
)

type gold struct{ users rewards.Users }

// Create keeps the user directory; it calls nothing yet.
func Create(users rewards.Users) (rewards.Provider, error) {
	return &gold{users: users}, nil
}

func (g *gold) IsMember(ctx context.Context, user string) (bool, error) {
	tier, err := g.users.Tier(ctx, user)

	return tier == "gold", err
}
