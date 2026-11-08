// Package gotimed is a rule doubling its number: its Create returns at once.
package gotimed

import (
	"context"

	"example.com/directives/rules"
)

type double struct{}

// Create returns the rule.
func Create() (rules.Timed, error) {
	return double{}, nil
}

func (double) Apply(_ context.Context, n int) (int, error) {
	return n * 2, nil
}
