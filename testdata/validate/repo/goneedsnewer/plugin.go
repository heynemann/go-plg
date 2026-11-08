// Package goneedsnewer pins v1.3.0 of the contract, whose Provider has Size, while the repository's go.mod
// requires v1.2.0.
package goneedsnewer

import (
	"context"

	"example.com/rewards/cohort"
)

type sized struct{}

// Create returns the provider.
func Create() (cohort.Provider, error) {
	return sized{}, nil
}

func (sized) IsMember(context.Context, string) (bool, error) { return false, nil }

func (sized) List(context.Context, string) (cohort.Page, error) { return cohort.Page{}, nil }

func (sized) Size(context.Context) (int, error) { return 0, nil }
