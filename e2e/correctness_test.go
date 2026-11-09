//go:build e2e

package e2e_test

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"example.com/rewards/cohort"
	"example.com/rewards/eligibility"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/e2e/harness"
	"github.com/heynemann/go-plg/e2e/platform"
)

// hangTimeout is the call timeout of the scenarios whose calls hang: what a hanging call costs, long enough for
// every healthy call under the race detector.
const hangTimeout = 2 * time.Second

// springGold is the smoke test's first campaign under another ID: the Go cohort paging the directory, a condition
// in each language, the wallet.
func springGold(id string) platform.Campaign {
	return platform.Campaign{
		ID: id, Cohort: "gold", Conditions: []string{"min-spend", "country", "active-buyer"}, Deliverer: "wallet",
		Reward: platform.Reward{Points: 500, Memo: "Spring bonus for gold members"},
	}
}

// winBack is the smoke test's second campaign under another ID: the Lua cohort asking the vendor's API.
func winBack(id string) platform.Campaign {
	return platform.Campaign{
		ID: id, Cohort: "churn-risk", Conditions: []string{"active-buyer"}, Deliverer: "wallet",
		Reward: platform.Reward{Points: 200, Memo: "We miss you"},
	}
}

// The wallet times out on a reward and answers 503 to it, and the reward is still credited exactly once: the
// platform retries with the same idempotency key, the wallet applies the key once, and the platform reports the
// retries and the duplicate. One member at a time, so that every failure falls, in order, on the first eligible
// member's reward: two 503s; an attempt the wallet applies but whose answer the call timeout cuts; another the
// wallet finds applied, cut the same way; a fifth answered 200, a duplicate. Its five Deliver calls are its
// member's span's children, and the request the wallet applied is the third attempt's.
func TestRewardsAreCreditedExactlyOnceDespiteTimeoutsAnd503s(t *testing.T) {
	t.Parallel()

	rig := harness.NewRig(t, &harness.Options{Limits: platform.Limits{
		Concurrency: 1, CallTimeout: hangTimeout, DeliveryAttempts: 5,
	}})
	campaign := springGold("exactly-once")
	expected := harness.Expect(t, rig.Users, &campaign)

	rig.Wallet.FailNext(2)
	rig.Wallet.HangNext(2)

	result, err := rig.Platform.Run(t.Context(), &campaign)
	require.NoError(t, err)
	t.Logf("%s: %+v", campaign.ID, result)

	expected.Result.Retries, expected.Result.Duplicates = 4, 1
	assert.Equal(t, expected.Result, result, "every reward delivered, after 4 retries and a duplicate")
	assert.Equal(t, expected.Credits, rig.Wallet.CreditsOf(campaign.ID), "every eligible member credited once")
	assert.EqualValues(t, 2, rig.Wallet.Repeats(), "the two attempts after the applied one repeated its key")
	assert.EqualValues(t, len(expected.Credits)+4, rig.Wallet.Requests(), "4 more requests than rewards")

	first := slices.Sorted(maps.Keys(expected.Credits))[0]
	attempts := deliverAttempts(t, rig, &campaign, expected.Credits[first].User)
	require.Len(t, attempts, 5, "the retried reward's five attempts, its member's span's children")

	for i, attempt := range attempts[:4] {
		assert.Equal(t, codes.Error, attempt.call.Status.Code, "attempt %d failed", i+1)
	}

	assert.Equal(t, codes.Unset, attempts[4].call.Status.Code, "the fifth attempt delivered")

	for i, status := range []string{"503", "503", "", "", "200"} {
		require.Len(t, attempts[i].posts, 1, "attempt %d made one request", i+1)
		assert.Equal(t, status, attr(attempts[i].posts[0].Attributes, "http.response.status_code"),
			"attempt %d's answer", i+1)
	}

	applied := attempts[2].posts[0].SpanContext
	assert.Equal(t, fmt.Sprintf("00-%s-%s-01", applied.TraceID(), applied.SpanID()), rig.Wallet.Traceparent(first),
		"the wallet applied the third attempt's request")
}

// attempt is one Deliver call of a member and the requests it made.
type attempt struct {
	call  tracetest.SpanStub
	posts tracetest.SpanStubs
}

// deliverAttempts returns the Deliver calls under the campaign.user span of the user in campaign's trace, in the
// order they started, with their requests.
func deliverAttempts(t *testing.T, rig *harness.Rig, campaign *platform.Campaign, user string) []attempt {
	t.Helper()

	member := campaignUser(t, rig, campaign, user)
	children := byParent(rig.Telemetry.Trace(member.SpanContext.TraceID()))
	calls := named(children[member.SpanContext.SpanID()], "plg.call "+platform.Deliveries+"/"+campaign.Deliverer+
		".Deliver")
	slices.SortFunc(calls, func(a, b tracetest.SpanStub) int { return a.StartTime.Compare(b.StartTime) })

	attempts := make([]attempt, 0, len(calls))
	for i := range calls {
		attempts = append(attempts, attempt{
			call: calls[i], posts: named(children[calls[i].SpanContext.SpanID()], "plg.http POST"),
		})
	}

	return attempts
}

// campaignUser returns the campaign.user span of the user in campaign's one trace.
func campaignUser(t *testing.T, rig *harness.Rig, campaign *platform.Campaign, user string) tracetest.SpanStub {
	t.Helper()

	run := campaignRun(t, rig.Telemetry, campaign)
	users := slices.DeleteFunc(named(byParent(rig.Telemetry.Trace(run.TraceID()))[run.SpanID()], "campaign.user"),
		func(span tracetest.SpanStub) bool { return attr(span.Attributes, "user.id") != user })
	require.Len(t, users, 1, "one campaign.user span of %s", user)

	return users[0]
}

// campaignRun returns the span context of campaign's one campaign.run span.
func campaignRun(t *testing.T, telemetry *harness.Telemetry, campaign *platform.Campaign) trace.SpanContext {
	t.Helper()

	runs := slices.DeleteFunc(telemetry.SpansNamed("campaign.run"), func(run tracetest.SpanStub) bool {
		return attr(run.Attributes, "campaign.id") != campaign.ID
	})
	require.Len(t, runs, 1, "one campaign.run span of %s", campaign.ID)

	return runs[0].SpanContext
}

// A cohort is listed whole, each member once, whatever its size against the page size: the everyone cohort
// returns the directory a page of cohort.MaxPageSize users at a time, and platforms serving directories of every
// size around one and two pages (an empty one too) credit every user once, after as many List calls as the
// directory has pages.
func TestCohortsAreListedWholeAtPageBoundaries(t *testing.T) {
	t.Parallel()

	rig := harness.NewRig(t, &harness.Options{Teams: []string{harness.Insights, harness.Payments}, Users: 1})
	page := cohort.MaxPageSize

	for _, size := range []int{0, 1, page - 1, page, page + 1, 2*page - 1, 2 * page, 2*page + 1} {
		t.Run(fmt.Sprintf("%d users", size), func(t *testing.T) {
			t.Parallel()

			dir := harness.NewDirectory(size)
			telemetry := harness.NewTelemetry(t)
			served := rig.NewPlatform(t, &harness.Options{
				Limits: platform.Limits{PoolMin: 1, PoolMax: 1, ConcurrentCalls: platformConcurrency},
				Configure: func(cfg *platform.Config) {
					cfg.Users, cfg.TracerProvider, cfg.MeterProvider = dir, telemetry.TracerProvider,
						telemetry.MeterProvider
				},
			})
			campaign := platform.Campaign{
				ID: fmt.Sprintf("pages-%d", size), Cohort: "everyone", Deliverer: "wallet",
				Reward: platform.Reward{Points: 1, Memo: "Thanks for being here"},
			}
			expected := harness.Expect(t, dir, &campaign)
			require.Equal(t, size, expected.Result.Delivered, "every user is a member and eligible")

			result, err := served.Run(t.Context(), &campaign)
			require.NoError(t, err)

			assert.Equal(t, expected.Result, result, "every user listed once, credited at the first attempt")
			assert.Equal(t, expected.Credits, rig.Wallet.CreditsOf(campaign.ID), "every user credited once")
			assert.Equal(t, harness.ExpectCalls(t, dir, &campaign), telemetry.Calls(t),
				"one List call per page of the directory, %d", harness.ExpectPages(t, dir, &campaign))
		})
	}
}

// platformConcurrency is the default Limits.Concurrency: how many members a campaign processes at once, and so
// how many calls one Lua or TypeScript instance can be asked to serve at once.
const platformConcurrency = 16

// Conditions combine with AND, whatever their order: over the gold cohort, the members credited for two
// conditions are exactly those credited for each alone (fewer than either), in either order, and so for three;
// with none, every member. The second condition is asked only about the members the first accepted, which plg's
// call counts show. Each campaign's result and credits are also the oracle's.
func TestConditionsCombineWithAnd(t *testing.T) {
	t.Parallel()

	rig := harness.NewRig(t, nil)
	credited := map[string]map[string]bool{} // by the conditions, joined: the users credited
	calls := map[harness.Calls]int64{}
	members := 0

	for i, conditions := range [][]string{
		nil,
		{"country"},
		{"active-buyer"},
		{"min-spend"},
		{"country", "active-buyer"},
		{"active-buyer", "country"},
		{"country", "active-buyer", "min-spend"},
	} {
		campaign := platform.Campaign{
			ID: fmt.Sprintf("and-%d", i), Cohort: "gold", Conditions: conditions, Deliverer: "wallet",
			Reward: platform.Reward{Points: 10, Memo: "Combined"},
		}
		expected := harness.Expect(t, rig.Users, &campaign)

		if conditions == nil {
			members = expected.Result.Members
		}

		result, err := rig.Platform.Run(t.Context(), &campaign)
		require.NoError(t, err)
		assert.Equal(t, expected.Result, result, "%v", conditions)
		assert.Equal(t, expected.Credits, rig.Wallet.CreditsOf(campaign.ID), "%v", conditions)

		users := map[string]bool{}
		for _, credit := range rig.Wallet.CreditsOf(campaign.ID) {
			users[credit.User] = true
		}

		credited[fmt.Sprint(conditions)] = users

		for key, n := range harness.ExpectCalls(t, rig.Users, &campaign) {
			calls[key] += n
		}
	}

	everyone, country, active, spend := credited["[]"], credited["[country]"], credited["[active-buyer]"],
		credited["[min-spend]"]
	both := intersect(country, active)

	assert.Len(t, everyone, members, "every member, without conditions")
	assert.Less(t, len(both), min(len(country), len(active)), "two conditions credit fewer than either")
	assert.Equal(t, both, credited["[country active-buyer]"], "two conditions credit those both accept")
	assert.Equal(t, both, credited["[active-buyer country]"], "in either order")
	assert.Equal(t, intersect(both, spend), credited["[country active-buyer min-spend]"], "and so for three")
	assert.Equal(t, calls, rig.Telemetry.Calls(t), "each condition asked about those the ones before accepted")
}

// intersect returns the users both a and b hold.
func intersect(a, b map[string]bool) map[string]bool {
	both := map[string]bool{}

	for user := range a {
		if b[user] {
			both[user] = true
		}
	}

	return both
}

// The insights team's loyalty rule, ported to Go, Lua and TypeScript, decides alike in every language: asked
// about every user of the directory (each tier, and amounts whose cents each language must format alike), the
// three ports give the same decision and the same reason, the oracle's; and a campaign over the whole directory
// with each port credits the same users, the oracle's.
func TestGoLuaAndTypeScriptPortsOfARuleAgree(t *testing.T) {
	t.Parallel()

	rig := harness.NewRig(t, &harness.Options{Teams: []string{harness.Insights, harness.Payments}})
	ports := []string{"loyal-go", "loyal-lua", "loyal-ts"}
	conditions := make([]eligibility.Condition, 0, len(ports))

	for _, port := range ports {
		condition, err := plg.Get[eligibility.Condition](t.Context(), rig.Platform.Container(),
			platform.Conditions+"/"+port)
		require.NoError(t, err)

		conditions = append(conditions, condition)
	}

	reasons := map[string]bool{}

	for _, user := range rig.Users.Users() {
		subject := eligibility.Subject{Campaign: "parity", User: eligibility.User{
			ID: user.ID, Country: user.Country, Tier: user.Tier, SpendCents: user.SpendCents, Orders: user.Orders,
		}}
		decisions := make([]eligibility.Decision, 0, len(ports))

		for _, condition := range conditions {
			decision, err := condition.Eligible(t.Context(), subject)
			require.NoError(t, err)

			decisions = append(decisions, decision)
		}

		assert.Equal(t, harness.Loyal(user), decisions[0].Eligible, "%s: the rule's decision", user.ID)
		assert.Equal(t, decisions[0], decisions[1], "%s: Lua decides as Go", user.ID)
		assert.Equal(t, decisions[0], decisions[2], "%s: TypeScript decides as Go", user.ID)

		reasons[decisions[0].Reason] = true
	}

	assert.Greater(t, len(reasons), 100, "the users met every branch of the rule, with their own amounts")

	credited := make([]map[string]bool, 0, len(ports))

	for _, port := range ports {
		campaign := platform.Campaign{
			ID: port, Cohort: "everyone", Conditions: []string{port}, Deliverer: "wallet",
			Reward: platform.Reward{Points: 50, Memo: "Loyalty bonus"},
		}
		expected := harness.Expect(t, rig.Users, &campaign)

		result, err := rig.Platform.Run(t.Context(), &campaign)
		require.NoError(t, err)
		assert.Equal(t, expected.Result, result, port)
		assert.Equal(t, expected.Credits, rig.Wallet.CreditsOf(campaign.ID), port)

		users := map[string]bool{}
		for _, credit := range rig.Wallet.CreditsOf(campaign.ID) {
			users[credit.User] = true
		}

		credited = append(credited, users)
	}

	assert.Equal(t, credited[0], credited[1], "the Lua port's campaign credits the Go port's users")
	assert.Equal(t, credited[0], credited[2], "the TypeScript port's campaign credits the Go port's users")
}

// A plugin's errors count against it: the fraud team's screening condition (Lua) fails for users in Mexico, and
// every member it fails for is a failure of conditions/screening in the campaign's result, its campaign.user span
// has the error status and names the plugin, is not asked the next condition and is not credited; plg's call
// counts have them as errors of screening. Asked second, it fails only for the members the first condition
// accepted. Both campaigns' results and credits are the oracle's.
func TestAFailingPluginsErrorsCountAgainstIt(t *testing.T) {
	t.Parallel()

	rig := harness.NewRig(t, &harness.Options{Teams: append(harness.Teams(), harness.Fraud)})
	calls := map[harness.Calls]int64{}

	for _, conditions := range [][]string{{"screening", "min-spend"}, {"min-spend", "screening"}} {
		campaign := platform.Campaign{
			ID: fmt.Sprint(conditions), Cohort: "gold", Conditions: conditions, Deliverer: "wallet",
			Reward: platform.Reward{Points: 30, Memo: "Screened"},
		}
		expected := harness.Expect(t, rig.Users, &campaign)
		failures := expected.Result.Failures[platform.Conditions+"/screening"]
		require.Positive(t, failures, "screening fails for some members")

		result, err := rig.Platform.Run(t.Context(), &campaign)
		require.NoError(t, err)
		t.Logf("%s: %+v", campaign.ID, result)
		assert.Equal(t, expected.Result, result, "%v: the failures counted against screening", conditions)
		assert.Equal(t, expected.Credits, rig.Wallet.CreditsOf(campaign.ID), conditions)

		run := campaignRun(t, rig.Telemetry, &campaign)
		children := byParent(rig.Telemetry.Trace(run.TraceID()))
		failed := 0

		for _, member := range named(children[run.SpanID()], "campaign.user") {
			if attr(member.Attributes, "campaign.failed") != platform.Conditions+"/screening" {
				assert.Equal(t, codes.Unset, member.Status.Code, "%v: only failures have the error status", conditions)

				continue
			}

			failed++

			below := children[member.SpanContext.SpanID()]
			screening := named(below, "plg.call "+platform.Conditions+"/screening.Eligible")

			assert.Equal(t, codes.Error, member.Status.Code, "%v: a failed member's span", conditions)
			require.Len(t, screening, 1, "%v: screening was asked", conditions)
			assert.Equal(t, codes.Error, screening[0].Status.Code, "%v: and failed", conditions)
			assert.Len(t, below, slices.Index(conditions, "screening")+1,
				"%v: the conditions up to screening asked, none after it, no delivery", conditions)
		}

		assert.Equal(t, failures, failed, "%v: every failed member's span names screening", conditions)

		for key, n := range harness.ExpectCalls(t, rig.Users, &campaign) {
			calls[key] += n
		}
	}

	assert.Equal(t, calls, rig.Telemetry.Calls(t), "screening's failures are its errors in plg's call counts")
}
