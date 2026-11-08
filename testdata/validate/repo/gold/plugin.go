// Package gold is a cohort provider of the users whose loyalty tier is gold, by the repository's shared lib.
package gold

import (
	"context"
	"errors"

	"example.com/rewards/cohort"

	"example.com/rewardsrepo/lib"
)

type gold struct{ users cohort.Users }

// Create checks the shared package answers; it calls no Host method.
func Create(users cohort.Users) (cohort.Provider, error) {
	if !lib.IsGold("gold") {
		return nil, errors.New("the shared lib does not tell gold")
	}

	return &gold{users: users}, nil
}

func (g *gold) IsMember(ctx context.Context, user string) (bool, error) {
	tier, err := g.users.Tier(ctx, user)

	return lib.IsGold(tier), err
}

func (g *gold) List(context.Context, string) (cohort.Page, error) {
	return cohort.Page{}, nil
}
