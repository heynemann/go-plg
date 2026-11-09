package harness

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/e2e/platform"
)

// What the teams' conditions require, as their sources say.
const (
	// MinSpendCents is min-spend's (growth, Go) minimum yearly spend.
	MinSpendCents = 10000
	// MinOrders is active-buyer's (payments, TypeScript) minimum yearly orders.
	MinOrders = 3
)

// CohortRules returns the teams' cohort providers, by key, written again in Go from what each says it does: the
// oracle's side of every comparison with the platform. A test adding a plugin adds its rule here.
func CohortRules() map[string]func(User) bool {
	return map[string]func(User) bool{
		// gold (growth, Go): the directory's gold-tier users.
		"gold": func(user User) bool { return user.Tier == Gold },
		// churn-risk (crm, Lua): the vendor's churn-risk segment.
		"churn-risk": func(user User) bool { return user.ChurnRisk },
	}
}

// ConditionRules returns the teams' eligibility conditions, by key, written again in Go.
func ConditionRules() map[string]func(User) bool {
	return map[string]func(User) bool{
		// min-spend (growth, Go): a yearly spend of at least MinSpendCents.
		"min-spend": func(user User) bool { return user.SpendCents >= MinSpendCents },
		// country (crm, Lua): users in Brazil or Portugal.
		"country": func(user User) bool { return user.Country == "BR" || user.Country == "PT" },
		// active-buyer (payments, TypeScript): at least MinOrders yearly orders.
		"active-buyer": func(user User) bool { return user.Orders >= MinOrders },
	}
}

// Expected is what a campaign must do: its Result, and the wallet's credits after it, by idempotency key.
type Expected struct {
	Credits map[string]Credit
	Result  platform.Result
}

// Expect computes, from dir's users and the rules above, what campaign must do when every plugin answers and
// every delivery succeeds at its first attempt: the cohort's members, those every condition accepts, each
// credited once.
func Expect(tb testing.TB, dir *Directory, campaign *platform.Campaign) Expected {
	tb.Helper()

	inCohort, ok := CohortRules()[campaign.Cohort]
	require.True(tb, ok, "no rule for the cohort %q", campaign.Cohort)

	conditions := make([]func(User) bool, 0, len(campaign.Conditions))

	for _, key := range campaign.Conditions {
		rule, ok := ConditionRules()[key]
		require.True(tb, ok, "no rule for the condition %q", key)

		conditions = append(conditions, rule)
	}

	expected := Expected{Credits: map[string]Credit{}, Result: platform.Result{Failures: map[string]int{}}}

	for _, user := range dir.Users() {
		if !inCohort(user) {
			continue
		}

		expected.Result.Members++

		if !all(conditions, user) {
			continue
		}

		expected.Result.Eligible++
		expected.Result.Delivered++
		expected.Credits[campaign.ID+"/"+user.ID] = Credit{
			User: user.ID, Campaign: campaign.ID, Points: campaign.Reward.Points, Memo: campaign.Reward.Memo,
		}
	}

	return expected
}

func all(rules []func(User) bool, user User) bool {
	for _, rule := range rules {
		if !rule(user) {
			return false
		}
	}

	return true
}
