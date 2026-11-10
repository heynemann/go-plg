// Package watchlist is an eligibility condition written in Go: users the fraud team's watchlist does not name
// qualify. It reads the watchlist API's bearer token, the secret watchlist_token.
//
// It handles that secret carelessly on purpose, the platform's end-to-end tests' fixture for every way a plugin
// could carry a secret into the platform's logs, spans, events and admin answers: it prints the token on every
// call, warns with it from Validate, and puts it in its error for the users it cannot check (in Mexico) and in
// its panic for those it chokes on (without orders), all of which plg scrubs, and in the reason it refuses the
// users it watches (with one order) for, a successful result plg does not scrub.
package watchlist

import (
	"context"
	"fmt"

	"example.com/rewards/eligibility"

	"github.com/heynemann/go-plg/plugin"
)

// tokenSecret is the secret holding the watchlist API's token.
const tokenSecret = "watchlist_token"

type condition struct{}

// Create returns the condition.
func Create() (eligibility.Condition, error) {
	return condition{}, nil
}

// Eligible checks the subject's user against the watchlist.
func (condition) Eligible(ctx context.Context, subject eligibility.Subject) (eligibility.Decision, error) {
	token, err := plugin.Secret(ctx, tokenSecret)
	if err != nil {
		return eligibility.Decision{}, err
	}

	user := subject.User
	fmt.Printf("checking %s against the watchlist with token %s\n", user.ID, token)

	switch {
	case user.Orders == 0:
		panic("the watchlist choked on " + user.ID + " with token " + token)
	case user.Country == "MX":
		return eligibility.Decision{}, fmt.Errorf("the watchlist does not cover %s (token %s)", user.Country, token)
	case user.Orders == 1:
		return eligibility.Decision{Reason: "on the watchlist (token " + token + ")"}, nil
	default:
		return eligibility.Decision{Eligible: true, Reason: "not on the watchlist"}, nil
	}
}

// Validate warns of the token it is configured with.
func (condition) Validate(ctx context.Context, _ eligibility.Limits) ([]plugin.Issue, error) {
	token, err := plugin.Secret(ctx, tokenSecret)
	if err != nil {
		return nil, err
	}

	return []plugin.Issue{{
		Severity: plugin.Warning, Code: "watchlist.token", Message: "configured with token " + token, Field: "token",
	}}, nil
}
