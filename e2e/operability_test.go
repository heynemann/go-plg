//go:build e2e

package e2e_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/e2e/harness"
	"github.com/heynemann/go-plg/e2e/platform"
)

// Two campaigns running at once, while the wallet answers 503 to some deliveries, are each one trace, from
// campaign.run down to the wallet: every span of a campaign's trace descends from its campaign.run; its cohort's
// List calls are the run's children, each reading the directory through the Host (gold) or the vendor's API
// (churn-risk) below it; every credit the wallet applied carries the traceparent of a plg.http POST of that
// campaign's trace, below the Deliver call of the credited member's campaign.user, below the run. The 503s are
// failed Deliver calls of their member, followed by the one that delivered: every reward gets one attempt more than
// the 503s, so that whichever members they fall on, even all on one, every reward is delivered.
func TestEachCampaignIsOneTraceDownToTheWallet(t *testing.T) {
	t.Parallel()

	failed := 3
	rig := harness.NewRig(t, &harness.Options{Limits: platform.Limits{Attempts: failed + 1}})
	campaigns := []platform.Campaign{springGold("traced-spring-gold"), winBack("traced-win-back")}
	results := make([]platform.Result, len(campaigns))

	rig.Wallet.FailNext(failed)

	var running sync.WaitGroup

	for i := range campaigns {
		running.Go(func() {
			var err error

			results[i], err = rig.Platform.Run(t.Context(), &campaigns[i])
			assert.NoError(t, err, campaigns[i].ID)
		})
	}

	running.Wait()

	retried, retries := 0, 0
	cohortCalls := map[string]string{"gold": "plg.host ", "churn-risk": "plg.http GET"}

	for i := range campaigns {
		campaign := &campaigns[i]
		expected := harness.Expect(t, rig.Users, campaign)
		retries += results[i].Retries
		expected.Result.Retries = results[i].Retries // which campaign's member drew the 503s varies; their sum is asserted
		assert.Equal(t, expected.Result, results[i], campaign.ID)

		run := campaignRun(t, rig.Telemetry, campaign)
		spans := rig.Telemetry.Trace(run.TraceID())
		byID := map[trace.SpanID]tracetest.SpanStub{}

		for i := range spans {
			byID[spans[i].SpanContext.SpanID()] = spans[i]
		}

		for i := range spans {
			assert.True(t, descends(byID, &spans[i], run.SpanID()), "%s: %s descends from campaign.run", campaign.ID,
				spans[i].Name)
		}

		children := byParent(spans)
		lists := named(children[run.SpanID()], "plg.call "+platform.Cohorts+"/"+campaign.Cohort+".List")
		assert.Len(t, lists, harness.ExpectPages(t, rig.Users, campaign), campaign.ID)

		for _, list := range lists {
			below := children[list.SpanContext.SpanID()]
			require.NotEmpty(t, below, "%s: List reads its source", campaign.ID)
			assert.True(t, strings.HasPrefix(below[0].Name, cohortCalls[campaign.Cohort]), below[0].Name)
		}

		for key, credit := range rig.Wallet.CreditsOf(campaign.ID) {
			post := byID[traceparentSpan(t, rig.Wallet.Traceparent(key), run.TraceID())]
			call := byID[post.Parent.SpanID()]
			member := byID[call.Parent.SpanID()]

			assert.Equal(t, "plg.http POST", post.Name, key)
			assert.Equal(t, "201", attr(post.Attributes, "http.response.status_code"), key)
			assert.Equal(t, "plg.call "+platform.Deliveries+"/wallet.Deliver", call.Name, key)
			assert.Equal(t, "campaign.user", member.Name, key)
			assert.Equal(t, credit.User, attr(member.Attributes, "user.id"), key)
			assert.Equal(t, run.SpanID(), member.Parent.SpanID(), key)
		}

		for _, post := range named(spans, "plg.http POST") {
			if attr(post.Attributes, "http.response.status_code") != "503" {
				continue
			}

			retried++

			call := byID[post.Parent.SpanID()]
			assert.Equal(t, codes.Error, call.Status.Code, "a 503 fails its Deliver call")

			attempts := named(children[call.Parent.SpanID()], call.Name)
			assert.Greater(t, len(attempts), 1, "its member's reward was delivered again")
		}
	}

	assert.Equal(t, failed, retried, "every 503 is a failed attempt in its campaign's trace")
	assert.Equal(t, failed, retries, "every 503 is one retry, counted in its campaign's result")
}

// descends reports whether span is ancestor, or descends from it through spans.
func descends(spans map[trace.SpanID]tracetest.SpanStub, span *tracetest.SpanStub, ancestor trace.SpanID) bool {
	for {
		if span.SpanContext.SpanID() == ancestor {
			return true
		}

		parent, ok := spans[span.Parent.SpanID()]
		if !ok {
			return false
		}

		span = &parent
	}
}

// traceparentSpan returns the span ID of a W3C traceparent, requiring its trace to be id.
func traceparentSpan(t *testing.T, traceparent string, id trace.TraceID) trace.SpanID {
	t.Helper()

	parts := strings.Split(traceparent, "-")
	require.Len(t, parts, 4, "a traceparent: %q", traceparent)
	require.Equal(t, id.String(), parts[1], "the campaign's trace")

	span, err := trace.SpanIDFromHex(parts[2])
	require.NoError(t, err)

	return span
}

// Every plugin call is measured against its plugin's owning team, whatever its outcome: three campaigns using
// plugins of four teams, where the fraud team's screening fails for some members (its error) and its velocity
// panics for others, record plg's call counts, latencies and errors exactly as the oracle attributes the calls
// (key, method, outcome, team, org, company), and velocity's panics as fraud's.
func TestMetricsAttributeEveryOutcomeToItsOwner(t *testing.T) {
	t.Parallel()

	rig := harness.NewRig(t, &harness.Options{Teams: append(harness.Teams(), harness.Fraud)})
	campaigns := []platform.Campaign{
		{
			ID: "metrics-screened", Cohort: "gold", Conditions: []string{"screening", "min-spend"},
			Deliverer: "wallet", Reward: platform.Reward{Points: 5, Memo: "Measured"},
		},
		{
			ID: "metrics-velocity", Cohort: "churn-risk", Conditions: []string{"velocity", "active-buyer"},
			Deliverer: "wallet", Reward: platform.Reward{Points: 5, Memo: "Measured"},
		},
		winBack("metrics-win-back"),
	}
	calls := map[harness.Calls]int64{}

	for i := range campaigns {
		expected := harness.Expect(t, rig.Users, &campaigns[i])

		result, err := rig.Platform.Run(t.Context(), &campaigns[i])
		require.NoError(t, err)
		assert.Equal(t, expected.Result, result, campaigns[i].ID)

		for key, n := range harness.ExpectCalls(t, rig.Users, &campaigns[i]) {
			calls[key] += n
		}
	}

	failures := map[harness.Calls]int64{}
	teams := map[string]bool{}
	panics := int64(0)

	for key, n := range calls {
		teams[key.Team] = true

		if key.Outcome != "ok" {
			failures[key] = n
		}

		if key.Outcome == harness.OutcomePanic {
			panics += n
		}
	}

	require.Len(t, teams, 4, "the calls are of four teams")
	require.Len(t, failures, 2, "a failing outcome of each of screening and velocity")

	assert.Equal(t, calls, rig.Telemetry.Calls(t), "plg.plugin.calls")
	assert.Equal(t, calls, rig.Telemetry.Measured(t, "plg.plugin.call.duration"), "plg.plugin.call.duration")
	assert.Equal(t, failures, rig.Telemetry.Measured(t, "plg.plugin.call.errors"), "plg.plugin.call.errors")
	assert.Equal(t, map[harness.Panics]int64{{
		Key: platform.Conditions + "/velocity", Phase: string(plg.PhaseCall), Team: harness.Fraud, Org: harness.Org,
		Company: harness.Company,
	}: panics}, rig.Telemetry.Panics(t), "plg.plugin.panics")
}

// A panicking condition and a hanging deliverer stay within their plugins while another campaign runs to the
// oracle's result, and each shows its owning team what happened, through plg status and plg events (the CLI built
// from this checkout, reading the platform's admin handler through a profile): the fraud team's velocity panics
// for every member without orders, the partners team's voucher never returns from a large reward and is stopped
// at the call timeout, attempt after attempt. Their campaigns count each failure against them. Both are held
// mid-run once their plugin has failed, each stopped at a member's directory lookup (velocity's campaign at gold's
// last member, voucher's at its last pilot outside gold), and the healthy campaign runs then, inside both of their
// campaign.run spans, to exactly the oracle's result; then both go on. Status lists each with its team and its
// panics (and every other plugin with none), a team's view only its plugins, each plugin's last panic and
// on-call; events list each panic after the load. Every campaign processes one member at a time: voucher spins in
// its guest until the call timeout, holding a CPU, so that one spins at most while other tests run beside it.
func TestFailuresStayWithTheirPluginsAndReachTheirTeams(t *testing.T) {
	t.Parallel()

	events := harness.NewEvents()
	gold, pilot := goldUsers(), lastPilotOutsideGold(t)

	var goldStop, pilotStop *harness.Paused

	rig := harness.NewRig(t, &harness.Options{
		Teams:  append(harness.Teams(), harness.Fraud, harness.Partners),
		Limits: platform.Limits{CallTimeout: hangTimeout, Concurrency: 1},
		Configure: func(cfg *platform.Config) {
			goldStop = harness.Pause(cfg.Users, gold[len(gold)-1].ID)
			pilotStop = harness.Pause(goldStop, pilot)
			cfg.Users, cfg.OnEvent = pilotStop, events.OnEvent
		},
	})
	velocity, voucher := platform.Conditions+"/velocity", platform.Deliveries+"/voucher"
	broken := []platform.Campaign{{
		ID: "velocity-check", Cohort: "gold", Conditions: []string{"velocity"}, Deliverer: "wallet",
		Reward: platform.Reward{Points: 20, Memo: "Steady spender"},
	}, {
		ID: "partner-pilot", Cohort: "pilot", Deliverer: "voucher",
		Reward: platform.Reward{Points: 5000, Memo: "Partner voucher"},
	}}
	results := make([]platform.Result, len(broken))

	var running sync.WaitGroup

	// A failure before the release ends the test's context, which ends the stopped lookups and the campaigns.
	t.Cleanup(running.Wait)

	for i := range broken {
		running.Go(func() {
			var err error

			results[i], err = rig.Platform.Run(t.Context(), &broken[i])
			assert.NoError(t, err, broken[i].ID)
		})
	}

	goldStop.Await(t)
	pilotStop.Await(t)

	awaitPanics(t, events, velocity, voucher)

	healthy := springGold("isolated-spring-gold")
	expectedHealthy := harness.Expect(t, rig.Users, &healthy)

	result, err := rig.Platform.Run(t.Context(), &healthy)
	require.NoError(t, err)
	assert.Equal(t, expectedHealthy.Result, result, "the healthy campaign ran while both broken ones were held")
	assert.Equal(t, expectedHealthy.Credits, rig.Wallet.CreditsOf(healthy.ID))

	goldStop.Release()
	pilotStop.Release()
	running.Wait()

	healthyRun := runSpan(t, rig.Telemetry, &healthy)
	for i := range broken {
		run := runSpan(t, rig.Telemetry, &broken[i])
		assert.True(t, run.StartTime.Before(healthyRun.StartTime) && healthyRun.EndTime.Before(run.EndTime),
			"%s ran within %s", healthy.ID, broken[i].ID)
	}

	expectedPanicking := harness.Expect(t, rig.Users, &broken[0])
	panics := expectedPanicking.Result.Failures[velocity]
	require.Positive(t, panics)
	assert.Equal(t, expectedPanicking.Result, results[0], "velocity's panics counted against it")
	assert.Equal(t, expectedPanicking.Credits, rig.Wallet.CreditsOf(broken[0].ID))

	pilots := len(harness.PilotMembers())
	attempts := pilots * 3 // platform.Limits' default Attempts
	assert.Equal(t, platform.Result{
		Failures: map[string]int{voucher: pilots}, Members: pilots, Eligible: pilots, Retries: attempts - pilots,
	}, results[1], "every attempt of every pilot stopped at the timeout, counted against voucher")

	admin := rig.NewAdmin(t)
	status := admin.Table(t, "status")
	require.Equal(t, []string{"KEY", "STATUS", "VERSION", "LANGUAGE", "TEAM", "PANICS", "PROBLEM"}, status[0])

	for _, row := range status[1:] {
		want := map[string]int{velocity: panics, voucher: attempts}[row[0]]
		assert.Equal(t, []string{"serving", harness.PluginTeams()[row[0]], strconv.Itoa(want), "-"},
			[]string{row[1], row[4], row[5], row[6]}, "plg status: %s", row[0])
	}

	fraud := admin.Plugins(t, "-team", harness.Fraud)
	keys := make([]string, 0, len(fraud))

	for _, plugin := range fraud {
		keys = append(keys, plugin.Key)
	}

	assert.Equal(t, []string{platform.Conditions + "/screening", velocity, platform.Conditions + "/watchlist"}, keys,
		"plg status -team fraud: the team's plugins only")

	assertShownToTeam(t, admin, velocity, &shown{
		team: harness.Fraud, method: "Eligible", message: "integer divide by zero", panics: panics,
	})
	assertShownToTeam(t, admin, voucher, &shown{
		team: harness.Partners, method: "Deliver", message: "deadline", panics: attempts,
	})

	table := admin.Table(t, "events", voucher)
	require.Len(t, table, 1+1+attempts, "plg events %s: the header, the load, each stop", voucher)
	assert.Equal(t, "panicked", table[len(table)-1][1])
}

// awaitPanics waits for a panicked event of each plugin of keys.
func awaitPanics(t *testing.T, events *harness.Events, keys ...string) {
	t.Helper()

	pending := map[string]bool{}
	for _, key := range keys {
		pending[key] = true
	}

	for len(pending) > 0 {
		panicked := events.Wait(t, func(event plg.Event) bool {
			return event.Kind == plg.EventPanicked && pending[event.Key]
		})
		delete(pending, panicked.Key)
	}
}

// lastPilotOutsideGold returns the last pilot the pilot cohort lists who is not a gold user, after at least one
// other: a campaign of the pilots processing one at a time reaches it after another one's delivery, and no gold
// campaign looks it up.
func lastPilotOutsideGold(t *testing.T) string {
	t.Helper()

	pilots, gold := harness.PilotMembers(), map[string]bool{}
	for _, user := range goldUsers() {
		gold[user.ID] = true
	}

	for i := len(pilots) - 1; i > 0; i-- {
		if !gold[pilots[i]] {
			return pilots[i]
		}
	}

	require.FailNow(t, "every pilot but the first is gold")

	return ""
}

// shown is what plg status and plg events must show of a plugin that panicked.
type shown struct {
	team, method, message string
	panics                int
}

// assertShownToTeam checks that plg status shows the plugin key's team, on-call and last panic (in a call of the
// method, with the message), and plg events its load and each panic, oldest first, every event naming the team.
func assertShownToTeam(t *testing.T, admin *harness.Admin, key string, want *shown) {
	t.Helper()

	plugin := admin.Plugin(t, key)
	assert.Equal(t, want.team, plugin.Owner.Team.Name, key)
	assert.Equal(t, want.team+"-oncall", plugin.Owner.Contact.Oncall, key)
	require.NotNil(t, plugin.LastPanic, key)
	assert.Equal(t, string(plg.PhaseCall), plugin.LastPanic.Phase, key)
	assert.Equal(t, want.method, plugin.LastPanic.Method, key)
	assert.Contains(t, plugin.LastPanic.Message, want.message, key)

	history := admin.Events(t, key)
	kinds := map[string]int{}

	for i := range history {
		kinds[history[i].Kind]++

		assert.Equal(t, want.team, history[i].Owner.Team.Name, "%s: every event names the team", key)
	}

	require.NotEmpty(t, history, "plg events: %s", key)
	assert.Equal(t, string(plg.EventLoaded), history[0].Kind, "%s: plg events, oldest first", key)
	assert.Equal(t, map[string]int{string(plg.EventLoaded): 1, string(plg.EventPanicked): want.panics}, kinds,
		"plg events: %s", key)
}

// A version whose validation fails keeps the last good one serving, and its owner hears of it: the growth team
// raises min-spend's minimum over the platform's limit (its own test cases, with a platform allowing it, pass,
// so its CI packs it), the version is published and the platform reloads; the reload fails for the error issue,
// the Rejected event and plg status (the team's view, from the built CLI) name the team, the version and the
// issue, the first version serves on, and a campaign still decides with it, as the oracle does.
func TestAValidationFailingVersionKeepsTheLastGoodServing(t *testing.T) {
	t.Parallel()

	events := harness.NewEvents()
	rig := harness.NewRig(t, &harness.Options{
		Configure: func(cfg *platform.Config) { cfg.OnEvent = events.OnEvent },
	})
	key := platform.Conditions + "/min-spend"

	good, err := plg.Info(rig.Platform.Container(), key)
	require.NoError(t, err)

	team := rig.Teams[harness.Growth]
	edit(t, team, "min-spend/plugin.go", "minimumCents = 10000", "minimumCents = 25000")
	team.Write(t, "min-spend/tests/min-spend.yaml", `cases:
  - name: a user who spent exactly the minimum qualifies
    call: Eligible
    input:
      subject:
        campaign: spring
        user:
          id: u000001
          country: BR
          tier: gold
          spendCents: 25000
          orders: 2
    expect:
      result:
        eligible: true
        reason: spent 25000 cents
  - name: a platform allowing the minimum accepts the condition
    call: Validate
    input:
      in:
        countries:
          - BR
        maxSpendCents: 25000
        maxOrders: 10
    expect:
      issues: []
`)
	team.Commit(t, "Raise min-spend's minimum to 250.00")
	harness.PublishArtifact(t, rig.S3, rig.Bucket, filepath.Join(team.CI(t, rig.Environ), "min-spend.plg"))

	err = rig.Platform.Reload(t.Context())
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorIs(t, err, plg.ErrBlockingIssue)

	rejected := events.Wait(
		t,
		func(event plg.Event) bool { return event.Kind == plg.EventRejected && event.Key == key },
	)
	assert.NotEqual(t, good.Version, rejected.Version, "the new version was rejected")
	assert.Equal(t, harness.Growth, rejected.Owner.Team.Name, "the Rejected event names the team")
	assert.Equal(t, "growth-oncall", rejected.Owner.Contact.Oncall)
	require.NotNil(t, rejected.Report)
	require.NotEmpty(t, rejected.Report.Issues, "the contract's warning comes first")
	assert.Equal(t, []plg.Issue{
		{
			Severity: plg.SeverityError,
			Code:     "min-spend.minimum",
			Message:  "requires 25000 cents, over the 20000 cents limit",
			Field:    "minimumCents",
		},
	}, rejected.Report.Issues[1:], "the platform's limit, after the contract's warning")

	state, err := plg.Info(rig.Platform.Container(), key)
	require.NoError(t, err)
	assert.Equal(t, plg.StatusServingLastGood, state.Status)
	assert.Equal(t, good.Version, state.Version, "the first version serves on")
	assert.Equal(t, rejected.Version, state.RejectedVersion)

	admin := rig.NewAdmin(t)
	status := admin.Table(t, "status", "-team", harness.Growth)
	require.Len(t, status, 3, "plg status -team growth: the header, gold and min-spend")

	row := tableRow(t, status, key)
	assert.Equal(t, []string{"serving_last_good", good.Version, harness.Growth}, []string{row[1], row[2], row[4]})
	assert.Contains(t, row[6], "min-spend.minimum: requires 25000 cents, over the 20000 cents limit")

	campaign := springGold("last-good")
	expected := harness.Expect(t, rig.Users, &campaign)

	result, err := rig.Platform.Run(t.Context(), &campaign)
	require.NoError(t, err)
	assert.Equal(t, expected.Result, result, "min-spend still requires 100.00")
	assert.Equal(t, expected.Credits, rig.Wallet.CreditsOf(campaign.ID))
}

// No secret's value reaches the platform's logs, spans, events or admin answers, though plugins use every secret
// the vault holds and one prints and reports its own: campaigns use the CRM vendor's token (churn-risk), the
// wallet's (wallet, answered 503 a few times) and the fraud team's watchlist token, which watchlist prints on
// every call, warns of from Validate, and puts in its errors and panics. Logged at Debug, traced, kept as events
// and served by the admin handler, none of it holds a token; what watchlist leaked is there, as plg's
// "[secret watchlist_token]", in each.
func TestSecretsNeverLeak(t *testing.T) {
	t.Parallel()

	log := harness.NewLog()
	rig := harness.NewRig(t, &harness.Options{
		Teams: append(harness.Teams(), harness.Fraud),
		Configure: func(cfg *platform.Config) {
			cfg.Logger = slog.New(slog.NewJSONHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}))
		},
	})
	campaigns := []platform.Campaign{springGold("secret-spring-gold"), winBack("secret-win-back"), {
		ID: "secret-watchlist", Cohort: "gold", Conditions: []string{"watchlist"}, Deliverer: "wallet",
		Reward: platform.Reward{Points: 1, Memo: "Not on the list"},
	}}

	rig.Wallet.FailNext(2)

	for i := range campaigns {
		expected := harness.Expect(t, rig.Users, &campaigns[i])

		result, err := rig.Platform.Run(t.Context(), &campaigns[i])
		require.NoError(t, err)

		expected.Result.Retries = result.Retries
		assert.Equal(t, expected.Result, result, campaigns[i].ID)
	}

	require.Positive(t, rig.Vendor.Requests(), "the vendor's token was used")
	require.Positive(t, rig.Wallet.Requests(), "the wallet's token was used")

	surfaces := map[string]string{
		"logs":   log.String(),
		"spans":  spansText(rig.Telemetry.Spans.GetSpans()),
		"events": eventsText(t, rig.Platform.Container()),
		"admin":  adminText(t, rig.Platform),
	}

	for surface, text := range surfaces {
		for _, token := range []string{harness.WalletToken, harness.VendorToken, harness.WatchlistToken} {
			assert.NotContains(t, text, token, "%s hold no secret", surface)
		}

		assert.Contains(t, text, "[secret watchlist_token]", "%s hold what watchlist leaked, scrubbed", surface)
	}
}

// A plugin reads only the secrets the vault holds for its platform key, whatever owner and secrets its
// plugin.yaml declares: the fraud team's watchlist, rewritten to claim the payments team as its owner and to read
// the wallet's token (its plugin.yaml, code and tests), passes its own CI and is published, but its Validate
// cannot resolve the token, so the platform rejects it, and the token is nowhere in its events or the admin
// answers.
func TestAPluginClaimingAnotherTeamCannotReadItsSecret(t *testing.T) {
	t.Parallel()

	events, log := harness.NewEvents(), harness.NewLog()
	rig := harness.NewRig(t, &harness.Options{
		Configure: func(cfg *platform.Config) {
			cfg.OnEvent = events.OnEvent
			cfg.Logger = slog.New(slog.NewJSONHandler(log, nil))
		},
	})
	key := platform.Conditions + "/watchlist"

	team := harness.CopyTeam(t, harness.Fraud)
	edit(t, team, "watchlist/plugin.yaml", "name: fraud", "name: "+harness.Payments)

	for _, path := range []string{"watchlist/plugin.yaml", "watchlist/plugin.go", "watchlist/tests/watchlist.yaml"} {
		content, err := os.ReadFile(filepath.Join(team.Dir, filepath.FromSlash(path)))
		require.NoError(t, err)
		require.Contains(t, string(content), "watchlist_token", path)
		team.Write(t, path, strings.ReplaceAll(string(content), "watchlist_token", "wallet_api_token"))
	}

	team.Commit(t, "Read the wallet's token as the payments team")
	harness.PublishArtifact(t, rig.S3, rig.Bucket, filepath.Join(team.CI(t, rig.Environ), "watchlist.plg"))

	err := rig.Platform.Reload(t.Context())
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, `Validate returned an error: plg: secret "wallet_api_token" could not be resolved`)

	rejected := events.Wait(
		t,
		func(event plg.Event) bool { return event.Key == key && event.Kind == plg.EventRejected },
	)
	assert.Equal(t, harness.Payments, rejected.Owner.Team.Name, "the plugin claimed the payments team")

	state, err := plg.Info(rig.Platform.Container(), key)
	require.NoError(t, err)
	assert.Equal(t, plg.StatusRejected, state.Status, "the impostor never serves")
	assert.Contains(t, log.String(), "the vault holds no such secret: wallet_api_token of conditions/watchlist",
		"the vault refused it by its platform key")

	for surface, text := range map[string]string{
		"logs": log.String(), "events": eventsText(t, rig.Platform.Container()), "admin": adminText(t, rig.Platform),
	} {
		assert.NotContains(t, text, harness.WalletToken, "%s hold no wallet token", surface)
	}
}

// spansText writes out everything spans hold that a plugin's words could reach: names, attributes, status
// descriptions and events.
func spansText(spans tracetest.SpanStubs) string {
	var text strings.Builder

	for i := range spans {
		span := &spans[i]
		fmt.Fprintln(&text, span.Name, span.Status.Description)

		for _, kv := range span.Attributes {
			fmt.Fprintln(&text, kv.Key, kv.Value.String())
		}

		for _, event := range span.Events {
			fmt.Fprintln(&text, event.Name)

			for _, kv := range event.Attributes {
				fmt.Fprintln(&text, kv.Key, kv.Value.String())
			}
		}
	}

	return text.String()
}

// eventsText writes out the events the container keeps of every plugin (plg.Events): their reports' errors and
// issues and their panics, value, guest output and stack included.
func eventsText(t *testing.T, container *plg.Container) string {
	t.Helper()

	var text strings.Builder

	plugins := plg.Plugins(container)
	for i := range plugins {
		events, err := plg.Events(container, plugins[i].Key)
		require.NoError(t, err)

		for j := range events {
			event := &events[j]
			fmt.Fprintln(&text, event.Kind, event.Key, event.Version)

			if event.Report != nil {
				fmt.Fprintln(&text, event.Report.Err)

				for _, issue := range event.Report.Issues {
					fmt.Fprintln(&text, issue.Code, issue.Message, issue.Field)
				}
			}

			if event.Panic != nil {
				fmt.Fprintln(
					&text,
					event.Panic.Error(),
					event.Panic.Value,
					event.Panic.Guest,
					string(event.Panic.Stack),
				)
			}
		}
	}

	return text.String()
}

// adminText returns every answer of the platform's admin handler: the plugins, each plugin and its events.
func adminText(t *testing.T, served *platform.Platform) string {
	t.Helper()

	handler := served.AdminHandler()
	get := func(path string) string {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, path)

		return response.Body.String()
	}

	plugins := plg.Plugins(served.Container())
	answers := make([]string, 0, 1+2*len(plugins))
	answers = append(answers, get("/plugins"))

	for i := range plugins {
		answers = append(answers, get("/plugins/"+plugins[i].Key), get("/plugins/"+plugins[i].Key+"/events"))
	}

	return strings.Join(answers, "\n")
}
