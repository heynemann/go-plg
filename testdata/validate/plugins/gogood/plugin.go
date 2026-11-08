// Package gogood is a cohort provider of the users whose loyalty tier is gold.
package gogood

import (
	"context"

	"example.com/rewards/cohort"
)

type gold struct{ users cohort.Users }

// Create keeps the user directory; it calls nothing yet.
func Create(users cohort.Users) (cohort.Provider, error) {
	return &gold{users: users}, nil
}

func (g *gold) IsMember(ctx context.Context, user string) (bool, error) {
	tier, err := g.users.Tier(ctx, user)

	return tier == "gold", err
}

func (g *gold) List(context.Context, string) (cohort.Page, error) {
	return cohort.Page{}, nil
}
