// Package everyone is a cohort provider written in Go: every user of the platform's directory. Each List call
// reads one page of the directory, through its Host (cohort.Users), at the largest size a page may have
// (cohort.MaxPageSize), and returns its users: the cohort's pages are the directory's.
package everyone

import (
	"context"

	"example.com/rewards/cohort"
)

type provider struct {
	users cohort.Users
}

// Create keeps the platform's user directory.
func Create(users cohort.Users) (cohort.Provider, error) {
	return provider{users: users}, nil
}

// IsMember looks the user up: every user the directory knows is a member.
func (p provider) IsMember(ctx context.Context, user string) (bool, error) {
	found, err := p.users.Lookup(ctx, user)
	if err != nil {
		return false, err
	}

	return found.ID != "", nil
}

// List returns the users of the directory's page at cursor, with the directory's next cursor.
func (p provider) List(ctx context.Context, cursor string) (cohort.Page, error) {
	page, err := p.users.Page(ctx, cursor, cohort.MaxPageSize)
	if err != nil {
		return cohort.Page{}, err
	}

	members := make([]string, 0, len(page.Users))
	for _, user := range page.Users {
		members = append(members, user.ID)
	}

	return cohort.Page{Users: members, Next: page.Next}, nil
}
