// Package velocity is an eligibility condition written in Go: users whose average order is at most 40.00 qualify;
// a larger average looks like a burst of fraudulent spend.
//
// It is broken on purpose: it divides the spend by the orders without checking that there are any, so a user
// without orders panics the plugin (integer divide by zero). The platform's end-to-end tests watch that panic stay
// within the plugin, counted against it and shown to its team.
package velocity

import (
	"context"
	"fmt"

	"example.com/rewards/eligibility"

	"github.com/heynemann/go-plg/plugin"
)

// maxAverageCents is the largest average order the condition accepts.
const maxAverageCents = 4000

type condition struct{}

// Create returns the condition.
func Create() (eligibility.Condition, error) {
	return condition{}, nil
}

// Eligible accepts a user whose average order is at most maxAverageCents.
func (condition) Eligible(_ context.Context, subject eligibility.Subject) (eligibility.Decision, error) {
	average := subject.User.SpendCents / int64(subject.User.Orders)
	if average > maxAverageCents {
		return eligibility.Decision{
			Reason: fmt.Sprintf("average order of %d cents, over %d", average, maxAverageCents),
		}, nil
	}

	return eligibility.Decision{Eligible: true, Reason: fmt.Sprintf("average order of %d cents", average)}, nil
}

// Validate has nothing to check: the condition requires neither spend nor orders.
func (condition) Validate(context.Context, eligibility.Limits) ([]plugin.Issue, error) {
	return nil, nil
}
