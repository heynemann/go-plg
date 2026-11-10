// Package minspend is an eligibility condition written in Go: users who spent at least 100.00 in the last year
// qualify.
package minspend

import (
	"context"
	"fmt"

	"example.com/rewards/eligibility"

	"github.com/heynemann/go-plg/plugin"
)

// minimumCents is the yearly spend the condition requires.
const minimumCents = 10000

type condition struct{}

// Create returns the condition.
func Create() (eligibility.Condition, error) {
	return condition{}, nil
}

// Eligible accepts a user whose yearly spend reaches the minimum.
func (condition) Eligible(_ context.Context, subject eligibility.Subject) (eligibility.Decision, error) {
	spent := subject.User.SpendCents
	if spent < minimumCents {
		return eligibility.Decision{Reason: fmt.Sprintf("spent %d cents, under %d", spent, minimumCents)}, nil
	}

	return eligibility.Decision{Eligible: true, Reason: fmt.Sprintf("spent %d cents", spent)}, nil
}

// Validate rejects the condition when the platform lets conditions require less spend than it does.
func (condition) Validate(_ context.Context, limits eligibility.Limits) ([]plugin.Issue, error) {
	if minimumCents > limits.MaxSpendCents {
		return []plugin.Issue{{
			Severity: plugin.Error,
			Code:     "min-spend.minimum",
			Message:  fmt.Sprintf("requires %d cents, over the %d cents limit", minimumCents, limits.MaxSpendCents),
			Field:    "minimumCents",
		}}, nil
	}

	return nil, nil
}
