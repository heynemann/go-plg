// Package watched is a cohort provider written in Go: the users the fraud team's watchlist watches. It reads the
// watchlist API's bearer token, the secret watchlist_token.
//
// It handles that secret carelessly on purpose, the platform's end-to-end tests' fixture for a secret in a
// successful result, which plg does not scrub: it puts the token in its pages' one member ID, a user the
// platform's directory does not know, and in their next cursor, and its cursors loop, so that the platform
// refuses its second page.
package watched

import (
	"context"

	"example.com/rewards/cohort"

	"github.com/heynemann/go-plg/plugin"
)

// tokenSecret is the secret holding the watchlist API's token.
const tokenSecret = "watchlist_token"

type provider struct{}

// Create returns the provider; the watchlist, not the platform's directory, knows its members.
func Create(cohort.Users) (cohort.Provider, error) {
	return provider{}, nil
}

// IsMember answers that no user is watched.
func (provider) IsMember(context.Context, string) (bool, error) {
	return false, nil
}

// List returns one member and a next cursor, both holding the token, the same at every page.
func (provider) List(ctx context.Context, _ string) (cohort.Page, error) {
	token, err := plugin.Secret(ctx, tokenSecret)
	if err != nil {
		return cohort.Page{}, err
	}

	return cohort.Page{Users: []string{"watched-" + token}, Next: "watched with token " + token}, nil
}
