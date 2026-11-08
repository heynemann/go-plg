// Package gocreatesignature declares a Create without the error result.
package gocreatesignature

import (
	"context"

	"example.com/rewards/cohort"
)

type provider struct{}

// Create returns a provider of nobody, and no error.
func Create() cohort.Provider {
	return provider{}
}

func (provider) IsMember(context.Context, string) (bool, error) { return false, nil }

func (provider) List(context.Context, string) (cohort.Page, error) { return cohort.Page{}, nil }
