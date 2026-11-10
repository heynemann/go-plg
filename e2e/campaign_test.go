//go:build e2e

package e2e_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/heynemann/go-plg/e2e/harness"
	"github.com/heynemann/go-plg/e2e/platform"
)

// The smoke test of the whole path: the three teams' CIs (plg validate and plg pack, built from this checkout)
// pack their plugins in Go, Lua and TypeScript, the artifacts are published to MinIO, the platform serves them
// from there, and two campaigns over 1,000 users run: one on the Go cohort paging the directory through its Host,
// with a condition in each language, and one on the Lua cohort asking the vendor's API. Each one's result and the
// wallet's credits are the oracle's, each is one trace from campaign.run down to the wallet's HTTP request, and
// plg's call metrics count every call against its plugin's owning team.
func TestRewardsPlatformRunsACampaign(t *testing.T) {
	t.Parallel()

	rig := harness.NewRig(t, nil)
	require.ElementsMatch(t, []string{
		"cohorts/gold", "conditions/min-spend", "cohorts/churn-risk", "conditions/country", "deliveries/wallet",
		"conditions/active-buyer",
	}, rig.Published, "every team published its plugins")

	campaigns := []platform.Campaign{springGold("spring-gold"), winBack("win-back")}
	calls := map[harness.Calls]int64{}

	for i := range campaigns {
		campaign := &campaigns[i]

		expected := harness.Expect(t, rig.Users, campaign)
		require.Positive(t, expected.Result.Eligible, "%s rewards someone", campaign.ID)
		require.Less(t, expected.Result.Eligible, expected.Result.Members, "%s leaves someone out", campaign.ID)

		result, err := rig.Platform.Run(t.Context(), campaign)
		require.NoError(t, err)
		t.Logf("%s: %+v", campaign.ID, result)

		assert.Equal(t, expected.Result, result, "%s did what the oracle says", campaign.ID)
		assert.Equal(t, expected.Credits, rig.Wallet.CreditsOf(campaign.ID), "%s credited the oracle's", campaign.ID)

		assertOneTrace(t, rig, campaign, result)

		for key, n := range harness.ExpectCalls(t, rig.Users, campaign) {
			calls[key] += n
		}
	}

	assert.Equal(t, calls, rig.Telemetry.Calls(t), "every call counted against its plugin's owning team")
	assert.Zero(t, rig.Wallet.Repeats(), "no reward was delivered twice")
	assert.Zero(t, rig.Wallet.Refused(), "the deliverer always sent the wallet's token")
	assert.Zero(t, rig.Vendor.Refused(), "the cohort always sent the vendor's token")
}

// assertOneTrace checks that campaign was one trace: one campaign.run span, the cohort's List calls its children,
// one per page the oracle expects (each making the one Host or HTTP call reading that page), a campaign.user child per
// member, the members' condition calls and the eligible ones' Deliver calls below theirs, and each Deliver's POST
// to the wallet below it, whose traceparent the wallet received.
func assertOneTrace(t *testing.T, rig *harness.Rig, campaign *platform.Campaign, result platform.Result) {
	t.Helper()

	run := campaignRun(t, rig.Telemetry, campaign)
	children := byParent(rig.Telemetry.Trace(run.TraceID()))

	cohortCall := "plg.call " + platform.Cohorts + "/" + campaign.Cohort + ".List"
	lists := named(children[run.SpanID()], cohortCall)
	users := named(children[run.SpanID()], "campaign.user")

	assert.Len(t, lists, harness.ExpectPages(t, rig.Users, campaign), "the cohort's pages are the run's children")
	assert.Len(t, users, result.Members, "a campaign.user span per member, the run's child")
	assert.Len(t, children[run.SpanID()], len(lists)+len(users), "the run has no other child")

	for i := range lists {
		assert.Len(t, children[lists[i].SpanContext.SpanID()], 1, "%s reads one page of its source", cohortCall)
	}

	delivered, asked := 0, harness.ExpectAsked(t, rig.Users, campaign)

	for i := range users {
		if assertMember(t, rig, campaign, &users[i], children, asked) {
			delivered++
		}
	}

	assert.Equal(t, result.Delivered, delivered, "every delivery is in the campaign's trace")
}

// assertMember checks the spans of one member and reports whether it was delivered: its condition calls, as many
// as the oracle's asked says, are campaign.user's children, its Deliver call too when it is eligible, with the POST to the wallet below it.
func assertMember(
	t *testing.T, rig *harness.Rig, campaign *platform.Campaign, user *tracetest.SpanStub,
	children map[trace.SpanID]tracetest.SpanStubs, asked map[string]int,
) bool {
	t.Helper()

	id := attr(user.Attributes, "user.id")
	below := children[user.SpanContext.SpanID()]
	deliveries := named(below, "plg.call "+platform.Deliveries+"/"+campaign.Deliverer+".Deliver")
	want, member := asked[id]
	require.True(t, member, "%s is a member of the cohort", id)
	assert.Equal(t, want, len(below)-len(deliveries), "%s: the conditions asked until one said no", id)

	if attr(user.Attributes, "campaign.eligible") != "true" {
		assert.Empty(t, deliveries, "%s: not eligible, not delivered", id)

		return false
	}

	require.Len(t, deliveries, 1, "%s: delivered once", id)

	posts := named(children[deliveries[0].SpanContext.SpanID()], "plg.http POST")
	require.Len(t, posts, 1, "%s: the deliverer's request to the wallet is its call's child", id)
	assert.Equal(t, "201", attr(posts[0].Attributes, "http.response.status_code"), "%s: credited", id)

	post := posts[0].SpanContext
	assert.Equal(t, fmt.Sprintf("00-%s-%s-01", post.TraceID(), post.SpanID()),
		rig.Wallet.Traceparent(campaign.ID+"/"+id), "%s: the wallet's request continues the campaign's trace", id)

	return true
}

// byParent indexes spans by their parent's span ID.
func byParent(spans tracetest.SpanStubs) map[trace.SpanID]tracetest.SpanStubs {
	children := map[trace.SpanID]tracetest.SpanStubs{}

	for i := range spans {
		parent := spans[i].Parent.SpanID()
		children[parent] = append(children[parent], spans[i])
	}

	return children
}

// named returns the spans of name.
func named(spans tracetest.SpanStubs, name string) tracetest.SpanStubs {
	var found tracetest.SpanStubs

	for i := range spans {
		if spans[i].Name == name {
			found = append(found, spans[i])
		}
	}

	return found
}

// attr returns the value of the attribute key, printed; "" when absent.
func attr(attrs []attribute.KeyValue, key attribute.Key) string {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value.String()
		}
	}

	return ""
}
