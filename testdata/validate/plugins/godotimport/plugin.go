// Package godotimport dot-imports the contract: its Create takes the Host by its bare name.
package godotimport

import (
	"context"

	. "example.com/rewards/cohort"
)

type gold struct{ users Users }

// Create keeps the user directory.
func Create(users Users) (Provider, error) {
	return &gold{users: users}, nil
}

func (g *gold) IsMember(ctx context.Context, user string) (bool, error) {
	tier, err := g.users.Tier(ctx, user)

	return tier == "gold", err
}

func (g *gold) List(context.Context, string) (Page, error) {
	return Page{}, nil
}
