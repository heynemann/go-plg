// Package loyal is an eligibility condition written in Go: the insights team's loyalty rule. Gold members qualify
// with at least two orders in the last year, silver members with at least 150.00 of spend, and no one else; the
// reason says which, with the amounts. The team keeps the rule in Go, Lua and TypeScript (loyal-go, loyal-lua,
// loyal-ts), every port held to the same test cases, and the platform's tests to the same decisions over every user.
package loyal

import (
	"context"
	"fmt"

	"example.com/rewards/eligibility"

	"github.com/heynemann/go-plg/plugin"
)

const (
	// minGoldOrders is the yearly orders a gold member needs.
	minGoldOrders = 2
	// minSilverCents is the yearly spend a silver member needs.
	minSilverCents = 15000
)

type condition struct{}

// Create returns the condition.
func Create() (eligibility.Condition, error) {
	return condition{}, nil
}

// Eligible applies the loyalty rule to the subject's user.
func (condition) Eligible(_ context.Context, subject eligibility.Subject) (eligibility.Decision, error) {
	user := subject.User

	switch user.Tier {
	case "gold":
		if user.Orders < minGoldOrders {
			return eligibility.Decision{
				Reason: fmt.Sprintf("gold member with %d orders, under %d", user.Orders, minGoldOrders),
			}, nil
		}

		return eligibility.Decision{Eligible: true, Reason: fmt.Sprintf("gold member with %d orders", user.Orders)}, nil
	case "silver":
		if user.SpendCents < minSilverCents {
			return eligibility.Decision{Reason: fmt.Sprintf("silver member who spent %s, under %s",
				amount(user.SpendCents), amount(minSilverCents))}, nil
		}

		return eligibility.Decision{
			Eligible: true, Reason: "silver member who spent " + amount(user.SpendCents),
		}, nil
	default:
		return eligibility.Decision{Reason: user.Tier + " members are not in the loyalty programme"}, nil
	}
}

// Validate rejects the condition when the platform lets conditions require less spend than silver members need.
func (condition) Validate(_ context.Context, limits eligibility.Limits) ([]plugin.Issue, error) {
	if minSilverCents > limits.MaxSpendCents {
		return []plugin.Issue{
			{
				Severity: plugin.Error,
				Code:     "loyal.spend",
				Message: "requires " + amount(
					minSilverCents,
				) + " of spend, over the limit of " + amount(
					limits.MaxSpendCents,
				),
				Field: "minSilverCents",
			},
		}, nil
	}

	return nil, nil
}

// amount writes cents as a decimal amount: 15000 is 150.00.
func amount(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}
