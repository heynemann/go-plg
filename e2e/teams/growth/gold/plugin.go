// Package gold is a cohort provider written in Go: the platform's gold-tier users. It reads the platform's user
// directory through its Host (cohort.Users), a page of it per List call, so a call holds one page whatever the
// directory's size.
package gold

import (
	"context"

	"example.com/rewards/cohort"
)

const (
	tier = "gold"
	// pageSize is how many directory users each List call reads; the page it returns holds their gold ones.
	pageSize = 100
)

type provider struct {
	users cohort.Users
}

// Create keeps the platform's user directory.
func Create(users cohort.Users) (cohort.Provider, error) {
	return provider{users: users}, nil
}

// IsMember looks the user up: a gold user is a member, an unknown one is not.
func (p provider) IsMember(ctx context.Context, user string) (bool, error) {
	found, err := p.users.Lookup(ctx, user)
	if err != nil {
		return false, err
	}

	return found.Tier == tier, nil
}

// List reads the directory's page at cursor and returns its gold users, with the directory's next cursor: a page
// may hold no member while later pages do.
func (p provider) List(ctx context.Context, cursor string) (cohort.Page, error) {
	page, err := p.users.Page(ctx, cursor, pageSize)
	if err != nil {
		return cohort.Page{}, err
	}

	members := []string{}

	for _, user := range page.Users {
		if user.Tier == tier {
			members = append(members, user.ID)
		}
	}

	return cohort.Page{Users: members, Next: page.Next}, nil
}
