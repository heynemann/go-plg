// Package goaliasimport imports the contract under another name; its entrypoint is its directory.
package goaliasimport

import (
	"context"

	rewards "example.com/rewards/cohort"
)

type gold struct{ users rewards.Users }

// Create keeps the user directory.
func Create(users rewards.Users) (rewards.Provider, error) {
	return &gold{users: users}, nil
}

func (g *gold) IsMember(ctx context.Context, user string) (bool, error) {
	tier, err := g.users.Tier(ctx, user)

	return tier == "gold", err
}

func (g *gold) List(context.Context, string) (rewards.Page, error) {
	return rewards.Page{}, nil
}
