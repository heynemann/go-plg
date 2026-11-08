// Package wrongsignature declares IsMember with another result type than the contract's.
package wrongsignature

import (
	"context"

	"example.com/rewards/cohort"
)

type provider struct{}

// Create returns a provider whose IsMember returns a string.
func Create() (cohort.Provider, error) {
	return provider{}, nil
}

func (provider) IsMember(context.Context, string) (string, error) { return "yes", nil }

func (provider) List(context.Context, string) (cohort.Page, error) { return cohort.Page{}, nil }
