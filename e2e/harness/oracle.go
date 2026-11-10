package harness

import (
	"slices"
	"testing"

	"example.com/rewards/cohort"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/e2e/platform"
)

// What the teams' conditions require, as their sources say.
const (
	// MinSpendCents is min-spend's (growth, Go) minimum yearly spend.
	MinSpendCents = 10000
	// MinOrders is active-buyer's (payments, TypeScript) minimum yearly orders.
	MinOrders = 3
	// LoyalGoldOrders and LoyalSilverCents are the loyalty rule's (insights: loyal-go, loyal-lua, loyal-ts)
	// minimum yearly orders of a gold member and minimum yearly spend of a silver one.
	LoyalGoldOrders  = 2
	LoyalSilverCents = 15000
	// MaxAverageOrderCents is velocity's (fraud, Go) largest average order.
	MaxAverageOrderCents = 4000
)

// PilotMembers are the pilot cohort's (partners, Lua) members, its whole list.
func PilotMembers() []string {
	return []string{"u000003", "u000012", "u000016"}
}

// The outcomes plg counts a failed call under (plg.plugin.calls), as a condition's failure rule says.
const (
	// OutcomeError is a call that returned the plugin's own error.
	OutcomeError = "error"
	// OutcomePanic is a call that ended in a plg.PanicError: a guest panic, or a call stopped at its deadline.
	OutcomePanic = "panic"
)

// CohortRules returns the teams' cohort providers, by key, written again in Go from what each says it does: the
// oracle's side of every comparison with the platform. A test adding a plugin adds its rule here.
func CohortRules() map[string]func(User) bool {
	return map[string]func(User) bool{
		// gold (growth, Go): the directory's gold-tier users.
		"gold": func(user User) bool { return user.Tier == Gold },
		// churn-risk (crm, Lua): the vendor's churn-risk segment.
		"churn-risk": func(user User) bool { return user.ChurnRisk },
		// everyone (insights, Go): the whole directory.
		"everyone": func(User) bool { return true },
		// pilot (partners, Lua): its fixed list.
		"pilot": func(user User) bool { return slices.Contains(PilotMembers(), user.ID) },
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
		// loyal-go, loyal-lua and loyal-ts (insights): one loyalty rule in each language.
		"loyal-go":  Loyal,
		"loyal-lua": Loyal,
		"loyal-ts":  Loyal,
		// screening (fraud, Lua): users with orders (it fails for users in Mexico: ConditionFailures).
		"screening": func(user User) bool { return user.Orders > 0 },
		// velocity (fraud, Go): an average order of at most MaxAverageOrderCents (it panics for users without
		// orders: ConditionFailures).
		"velocity": func(user User) bool {
			return user.Orders > 0 && user.SpendCents/int64(user.Orders) <= MaxAverageOrderCents
		},
		// watchlist (fraud, Go): everyone it does not fail for (ConditionFailures) but the users it watches, those
		// with one order.
		"watchlist": func(user User) bool { return user.Orders != 1 },
		// north-america (marketplace, TypeScript): users in the United States or Mexico.
		"north-america": func(user User) bool { return user.Country == "US" || user.Country == "MX" },
	}
}

// Loyal is the loyalty rule of insights' loyal-go, loyal-lua and loyal-ts: gold members with at least
// LoyalGoldOrders yearly orders, and silver members with at least LoyalSilverCents of yearly spend.
func Loyal(user User) bool {
	switch user.Tier {
	case Gold:
		return user.Orders >= LoyalGoldOrders
	case Silver:
		return user.SpendCents >= LoyalSilverCents
	default:
		return false
	}
}

// ConditionFailures returns, by key, the conditions that fail for some users, written again in Go: the outcome
// plg counts the call under for a user it fails for (OutcomeError, OutcomePanic), "" for one it decides on.
func ConditionFailures() map[string]func(User) string {
	return map[string]func(User) string{
		// screening (fraud, Lua) has no screening data for Mexico, and errors.
		"screening": func(user User) string {
			if user.Country == "MX" {
				return OutcomeError
			}

			return ""
		},
		// velocity (fraud, Go) divides by the user's orders, and panics without any.
		"velocity": func(user User) string {
			if user.Orders == 0 {
				return OutcomePanic
			}

			return ""
		},
		// watchlist (fraud, Go) panics for users without orders and errors for users in Mexico.
		"watchlist": func(user User) string {
			switch {
			case user.Orders == 0:
				return OutcomePanic
			case user.Country == "MX":
				return OutcomeError
			default:
				return ""
			}
		},
	}
}

// PluginTeams returns the team owning each plugin, by platform key, as its plugin.yaml says; every plugin is of the
// org rewards, of the company Example Shop.
func PluginTeams() map[string]string {
	return map[string]string{
		"cohorts/gold": Growth, "conditions/min-spend": Growth,
		"cohorts/churn-risk": CRM, "conditions/country": CRM,
		"deliveries/wallet": Payments, "conditions/active-buyer": Payments,
		"cohorts/everyone": Insights, "conditions/loyal-go": Insights, "conditions/loyal-lua": Insights,
		"conditions/loyal-ts":  Insights,
		"conditions/screening": Fraud, "conditions/velocity": Fraud, "conditions/watchlist": Fraud,
		"cohorts/watched": Fraud,
		"cohorts/pilot":   Partners, "deliveries/voucher": Partners,
		"conditions/north-america": Marketplace,
		"conditions/batch-spend":   NextGen,
	}
}

// The org and company of every team's plugins.
const (
	Org     = "rewards"
	Company = "Example Shop"
)

// Expected is what a campaign must do: its Result, and the wallet's credits after it, by idempotency key.
type Expected struct {
	Credits map[string]Credit
	Result  platform.Result
}

// Expect computes, from dir's users and the rules above, what campaign must do when every delivery succeeds at
// its first attempt: the cohort's members, the conditions asked in order until one says no or fails (its failure
// counted against it), those every condition accepts, each credited once.
func Expect(tb testing.TB, dir *Directory, campaign *platform.Campaign) Expected {
	tb.Helper()

	inCohort, conditions := rules(tb, campaign)
	expected := Expected{Credits: map[string]Credit{}, Result: platform.Result{Failures: map[string]int{}}}

	for _, user := range dir.Users() {
		if !inCohort(user) {
			continue
		}

		expected.Result.Members++

		decision := decide(conditions, user)
		if decision.failed != "" {
			expected.Result.Failures[platform.Conditions+"/"+decision.failed]++
		}

		if !decision.eligible {
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

// ExpectCalls computes, from dir's users and the rules above, the plugin calls campaign makes when every
// delivery succeeds at its first attempt, by what plg's call metrics (plg.plugin.calls) attribute each to: the
// cohort's List once per page (ExpectPages), each condition asked about the members every condition before it
// accepted (ok, or the outcome its failure rule gives), Deliver once per eligible member.
func ExpectCalls(tb testing.TB, dir *Directory, campaign *platform.Campaign) map[Calls]int64 {
	tb.Helper()

	inCohort, conditions := rules(tb, campaign)
	calls := map[Calls]int64{call(platform.Cohorts+"/"+campaign.Cohort, "List", "ok"): int64(ExpectPages(tb, dir,
		campaign))}

	for _, user := range dir.Users() {
		if !inCohort(user) {
			continue
		}

		decision := decide(conditions, user)
		for i := range decision.asked {
			outcome := "ok"
			if i == decision.asked-1 && decision.outcome != "" {
				outcome = decision.outcome
			}

			calls[call(platform.Conditions+"/"+conditions[i].key, "Eligible", outcome)]++
		}

		if decision.eligible {
			calls[call(platform.Deliveries+"/"+campaign.Deliverer, "Deliver", "ok")]++
		}
	}

	return calls
}

// ExpectAsked computes, from dir's users and the rules above, how many of campaign's conditions each member of its
// cohort is asked, by user ID: those in order until one says no or fails, every one when all accept.
func ExpectAsked(tb testing.TB, dir *Directory, campaign *platform.Campaign) map[string]int {
	tb.Helper()

	inCohort, conditions := rules(tb, campaign)
	asked := map[string]int{}

	for _, user := range dir.Users() {
		if inCohort(user) {
			asked[user.ID] = decide(conditions, user).asked
		}
	}

	return asked
}

// ExpectPages computes how many pages campaign's cohort lists over dir's users, as each provider's source pages
// them: gold the directory 100 users at a time, churn-risk the vendor's segment vendorPageSize members at a time,
// everyone the directory cohort.MaxPageSize users at a time, pilot one page; an empty listing is one empty page.
func ExpectPages(tb testing.TB, dir *Directory, campaign *platform.Campaign) int {
	tb.Helper()

	inCohort, _ := rules(tb, campaign)
	members := 0

	for _, user := range dir.Users() {
		if inCohort(user) {
			members++
		}
	}

	switch campaign.Cohort {
	case "gold":
		return pages(len(dir.Users()), GoldPageSize)
	case "churn-risk":
		return pages(members, vendorPageSize)
	case "everyone":
		return pages(len(dir.Users()), cohort.MaxPageSize)
	case "pilot":
		return 1
	default:
		require.Fail(tb, "no paging rule for the cohort "+campaign.Cohort)

		return 0
	}
}

// GoldPageSize is how many directory users each of gold's List calls reads.
const GoldPageSize = 100

// pages is how many pages of size hold n items, one empty page when there is none.
func pages(n, size int) int {
	if n == 0 {
		return 1
	}

	return (n-1)/size + 1
}

// call is the attribution of a call to the plugin key's method with outcome.
func call(key, method, outcome string) Calls {
	return Calls{Key: key, Method: method, Outcome: outcome, Team: PluginTeams()[key], Org: Org, Company: Company}
}

// conditionRule is a campaign's condition, as the oracle's rules say.
type conditionRule struct {
	accepts func(User) bool
	fails   func(User) string
	key     string
}

// rules returns the rules of the campaign's cohort and conditions.
func rules(tb testing.TB, campaign *platform.Campaign) (func(User) bool, []conditionRule) {
	tb.Helper()

	inCohort, ok := CohortRules()[campaign.Cohort]
	require.True(tb, ok, "no rule for the cohort %q", campaign.Cohort)

	conditions := make([]conditionRule, 0, len(campaign.Conditions))

	for _, key := range campaign.Conditions {
		rule, ok := ConditionRules()[key]
		require.True(tb, ok, "no rule for the condition %q", key)

		fails := ConditionFailures()[key]
		if fails == nil {
			fails = func(User) string { return "" }
		}

		conditions = append(conditions, conditionRule{accepts: rule, fails: fails, key: key})
	}

	return inCohort, conditions
}

// decision is what a campaign's conditions decide about a member.
type decision struct {
	// outcome is the failed condition's call outcome, failed its key; both empty when none failed.
	outcome, failed string
	// asked is how many conditions were asked.
	asked int
	// eligible is whether every condition accepted the member.
	eligible bool
}

// decide asks conditions about user in order, as the platform does, up to the first that says no or fails.
func decide(conditions []conditionRule, user User) decision {
	for i, condition := range conditions {
		if outcome := condition.fails(user); outcome != "" {
			return decision{asked: i + 1, outcome: outcome, failed: condition.key}
		}

		if !condition.accepts(user) {
			return decision{asked: i + 1}
		}
	}

	return decision{asked: len(conditions), eligible: true}
}
