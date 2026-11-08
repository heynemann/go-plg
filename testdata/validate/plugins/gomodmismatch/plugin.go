// Package gomodmismatch requires another contract version in its go.mod than plugin.yaml pins.
package gomodmismatch

import (
	"context"

	"example.com/rewards/cohort"
)

type provider struct{}

// Create returns a provider of nobody.
func Create() (cohort.Provider, error) {
	return provider{}, nil
}

func (provider) IsMember(context.Context, string) (bool, error) { return false, nil }

func (provider) List(context.Context, string) (cohort.Page, error) { return cohort.Page{}, nil }
