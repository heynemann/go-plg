//go:build e2e

package e2e_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/e2e/harness"
	"github.com/heynemann/go-plg/e2e/platform"
)

// pollInterval is how often the platforms of the scenarios publishing while they run poll the bucket.
const pollInterval = 100 * time.Millisecond

// A team new to the platform publishes a condition while the platform runs, and the platform's next poll serves
// it, without a restart: a campaign naming it fails before it is published (not found), and after the poll's
// Loaded event, which names its team, the same platform runs it to the oracle's result and credits.
func TestANewTeamsConditionGoesLiveOnTheNextPoll(t *testing.T) {
	t.Parallel()

	events := harness.NewEvents()
	rig := harness.NewRig(t, &harness.Options{
		PollInterval: pollInterval, Configure: func(cfg *platform.Config) { cfg.OnEvent = events.OnEvent },
	})
	campaign := platform.Campaign{
		ID: "north-america", Cohort: "gold", Conditions: []string{"north-america"}, Deliverer: "wallet",
		Reward: platform.Reward{Points: 75, Memo: "Hola, hello"},
	}

	_, err := rig.Platform.Run(t.Context(), &campaign)
	require.ErrorIs(t, err, plg.ErrNotFound, "no team has published north-america yet")

	team := harness.CopyTeam(t, harness.Marketplace)
	key := harness.PublishArtifact(t, rig.S3, rig.Bucket, filepath.Join(team.CI(t, rig.Environ), "north-america.plg"))
	require.Equal(t, platform.Conditions+"/north-america", key)

	loaded := events.Wait(t, func(event plg.Event) bool { return event.Kind == plg.EventLoaded && event.Key == key })
	assert.Equal(t, harness.Marketplace, loaded.Owner.Team.Name, "the Loaded event names the new team")

	expected := harness.Expect(t, rig.Users, &campaign)
	require.Positive(t, expected.Result.Eligible)

	result, err := rig.Platform.Run(t.Context(), &campaign)
	require.NoError(t, err)
	assert.Equal(t, expected.Result, result)
	assert.Equal(t, expected.Credits, rig.Wallet.CreditsOf(campaign.ID))
}

// A platform built with v1.1.0 of the contracts (a minor release: eligibility.User gains Locale) serves the
// teams' plugins, every one pinned to v1.0.0: the campaigns command, built against the release served by a
// GOPROXY (harness.BuildCampaigns), reports v1.1.0 in its build info, loads every plugin without a contract issue
// (where the tests' own platform, built against the contracts' checkout, warns that it cannot check the pins), and
// runs both of the smoke test's campaigns to the oracle's results and credits.
func TestAPlatformAtANewerMinorServesOlderPins(t *testing.T) {
	t.Parallel()

	rig := harness.NewRig(t, nil)
	program := harness.BuildCampaigns(t, harness.ContractV1Minor)
	campaigns := []platform.Campaign{springGold("minor-spring-gold"), winBack("minor-win-back")}

	report := harness.RunCampaigns(t, program, rig.Spec(t, campaigns...))
	assert.Equal(t, harness.ContractV1Minor, report.Contract, "the program is built with the release")
	assert.Contains(t, report.UserFields, "locale", "from the release's source: v1.1.0's eligibility.User")

	keys := make([]string, 0, len(report.Plugins))

	for _, plugin := range report.Plugins {
		keys = append(keys, plugin.Key)
		assert.Equal(t, string(plg.StatusServing), plugin.Status, plugin.Key)
		assert.Empty(t, plugin.Issues, "%s: no contract issue, the pin checked", plugin.Key)

		devel, err := plg.Info(rig.Platform.Container(), plugin.Key)
		require.NoError(t, err)
		require.NotNil(t, devel.Load, plugin.Key)
		require.NotEmpty(t, devel.Load.Issues, plugin.Key)
		assert.Equal(t, plg.CodeContractDevel, devel.Load.Issues[0].Code,
			"%s: the checkout's platform cannot check the pin", plugin.Key)
	}

	assert.ElementsMatch(t, rig.Published, keys, "every plugin pinned to v1.0.0 served")

	for i := range campaigns {
		expected := harness.Expect(t, rig.Users, &campaigns[i])
		assert.Empty(t, report.Errors[i], campaigns[i].ID)
		assert.Equal(t, expected.Result, report.Results[i], campaigns[i].ID)
		assert.Equal(t, expected.Credits, rig.Wallet.CreditsOf(campaigns[i].ID), campaigns[i].ID)
	}
}

// A plugin written against the contracts' next major (example.com/rewards/v2, its condition deciding on batches)
// is skipped by the v1 platform, visibly: plg.Info and plg status (the built CLI, through the admin handler) show
// it skipped for implementing another interface, with its team; its events (plg events) are one Skipped event; a
// campaign naming it fails before running (not found), and the v1 plugins beside it serve.
func TestAPluginOfTheNextMajorIsSkippedVisibly(t *testing.T) {
	t.Parallel()

	rig := harness.NewRig(t, &harness.Options{Teams: append(harness.Teams(), harness.NextGen)})
	key := platform.Conditions + "/batch-spend"
	require.Contains(t, rig.Published, key)

	state, err := plg.Info(rig.Platform.Container(), key)
	require.NoError(t, err)
	assert.Equal(t, plg.StatusSkipped, state.Status)
	require.NotNil(t, state.Rejection)
	require.ErrorIs(t, state.Rejection.Err, plg.ErrOtherInterface)

	admin := rig.NewAdmin(t)
	row := tableRow(t, admin.Table(t, "status"), key)
	assert.Equal(t, []string{"skipped", "lua", harness.NextGen, "0"}, []string{row[1], row[3], row[4], row[5]},
		"plg status: skipped, its language, its team, no panic")
	assert.Equal(t, state.Rejection.Err.Error(), row[6], "plg status: why")

	shown := admin.Plugin(t, key)
	assert.Equal(t, string(plg.StatusSkipped), shown.Status)
	assert.Equal(t, harness.NextGen, shown.Owner.Team.Name)
	assert.Equal(t, "nextgen-oncall", shown.Owner.Contact.Oncall, "who to call")
	assert.Nil(t, shown.Pool, "no instance runs")

	events := admin.Events(t, key)
	require.Len(t, events, 1, "plg events")
	assert.Equal(t, string(plg.EventSkipped), events[0].Kind)
	require.NotNil(t, events[0].Report)
	require.NotNil(t, events[0].Report.Error)
	assert.Equal(t, state.Rejection.Err.Error(), *events[0].Report.Error)

	_, err = rig.Platform.Run(t.Context(), &platform.Campaign{
		ID: "batch", Cohort: "gold", Conditions: []string{"batch-spend"}, Deliverer: "wallet",
	})
	require.ErrorIs(t, err, plg.ErrNotFound, "a campaign cannot name it")

	campaign := springGold("beside-v2")
	expected := harness.Expect(t, rig.Users, &campaign)

	result, err := rig.Platform.Run(t.Context(), &campaign)
	require.NoError(t, err)
	assert.Equal(t, expected.Result, result, "the v1 plugins serve")
}

// tableRow returns the row of plg status's table whose first cell is key.
func tableRow(t *testing.T, rows [][]string, key string) []string {
	t.Helper()

	i := slices.IndexFunc(rows, func(row []string) bool { return row[0] == key })
	require.NotEqual(t, -1, i, "plg status lists %s", key)

	return rows[i]
}

// The CRM team hot-swaps its country condition while a campaign runs, and the swap changes the decisions of the
// members after it only, with no call failing: the campaign processes its gold members one at a time and stops
// at the middle one (its directory lookup waits); the team adds Mexico to the condition, its CI packs it, it is
// published and the platform reloads; the campaign goes on. Members before the stop are decided by the first
// version (Brazil and Portugal), the stopped one and those after by the second (Mexico too): the credits are
// exactly that, every country call ran on the version its member's place says (its span's version), and every
// call succeeded.
func TestAHotSwapMidCampaignChangesLaterUsersOnly(t *testing.T) {
	t.Parallel()

	gold := goldUsers()
	stop := len(gold) / 2

	var paused *harness.Paused

	rig := harness.NewRig(t, &harness.Options{
		Limits: platform.Limits{Concurrency: 1},
		Configure: func(cfg *platform.Config) {
			paused = harness.Pause(cfg.Users, gold[stop].ID)
			cfg.Users = paused
		},
	})
	campaign := platform.Campaign{
		ID: "hot-swap", Cohort: "gold", Conditions: []string{"country"}, Deliverer: "wallet",
		Reward: platform.Reward{Points: 40, Memo: "Hot off the press"},
	}
	key := platform.Conditions + "/country"

	before, err := plg.Info(rig.Platform.Container(), key)
	require.NoError(t, err)

	type ran struct {
		err    error
		result platform.Result
	}

	done := make(chan ran, 1)

	go func() {
		result, failed := rig.Platform.Run(t.Context(), &campaign)
		done <- ran{result: result, err: failed}
	}()

	select {
	case <-paused.Reached():
	case ended := <-done:
		require.FailNow(t, "the campaign ended before the stop", "%+v", ended)
	}

	team := rig.Teams[harness.CRM]
	edit(t, team, "country/plugin.lua", `{ "BR", "PT" }`, `{ "BR", "PT", "MX" }`)
	edit(
		t,
		team,
		"country/tests/country.yaml",
		`"lives in US, not in BR or PT"`,
		`"lives in US, not in BR or PT or MX"`,
	)
	edit(t, team, "country/tests/country.yaml", `message: "targets PT, which the platform does not serve"}]`,
		`message: "targets PT, which the platform does not serve"},`+
			` {severity: error, code: country.unserved, message: "targets MX, which the platform does not serve"}]`)
	edit(t, team, "country/tests/country.yaml", "countries: [US, PT, BR]", "countries: [US, PT, BR, MX]")
	team.Commit(t, "Run campaigns in Mexico too")
	harness.PublishArtifact(t, rig.S3, rig.Bucket, filepath.Join(team.CI(t, rig.Environ), "country.plg"))
	require.NoError(t, rig.Platform.Reload(t.Context()))

	after, err := plg.Info(rig.Platform.Container(), key)
	require.NoError(t, err)
	require.NotEqual(t, before.Version, after.Version, "the reload published the new version")

	paused.Release()

	ended := <-done
	require.NoError(t, ended.err)

	expected := map[string]harness.Credit{}
	versions := map[string]string{}
	countries := map[bool][]string{false: {"BR", "PT"}, true: {"BR", "PT", "MX"}}
	mexicans := map[bool]int{} // credited users in Mexico, by whether after the stop

	for i, user := range gold {
		swapped := i >= stop
		versions[user.ID] = map[bool]string{false: before.Version, true: after.Version}[swapped]

		if slices.Contains(countries[swapped], user.Country) {
			expected[campaign.ID+"/"+user.ID] = harness.Credit{
				User: user.ID, Campaign: campaign.ID, Points: campaign.Reward.Points, Memo: campaign.Reward.Memo,
			}
		}

		if user.Country == "MX" {
			mexicans[swapped]++
		}
	}

	require.Positive(t, mexicans[false], "members in Mexico before the stop, which the first version refuses")
	require.Positive(t, mexicans[true], "members in Mexico after it, which the second accepts")

	assert.Equal(t, platform.Result{
		Failures: map[string]int{}, Members: len(gold), Eligible: len(expected), Delivered: len(expected),
	}, ended.result)
	assert.Equal(t, expected, rig.Wallet.CreditsOf(campaign.ID), "each member decided by its place's version")

	for calls := range rig.Telemetry.Calls(t) {
		assert.Equal(t, "ok", calls.Outcome, "no call failed: %+v", calls)
	}

	run := campaignRun(t, rig.Telemetry, &campaign)
	children := byParent(rig.Telemetry.Trace(run.TraceID()))
	members := named(children[run.SpanID()], "campaign.user")
	require.Len(t, members, len(gold))

	for _, member := range members {
		user := attr(member.Attributes, "user.id")
		calls := named(children[member.SpanContext.SpanID()], "plg.call "+key+".Eligible")
		require.Len(t, calls, 1, user)
		assert.Equal(
			t,
			versions[user],
			attr(calls[0].Attributes, "version"),
			"%s: decided by its place's version",
			user,
		)
	}
}

// The growth team hot-swaps its gold cohort while one of its calls is in flight, and that call, and every other,
// succeeds: the team releases gold 1.0.1 (plugin.yaml's version, its CI run again); a campaign over the gold
// cohort, its members 16 at a time, stops in the List call reading the page at the directory's middle (its Host
// read waits, well within the call timeout); the release is published and the platform reloads; the call goes on.
// plg drains the replaced version: the stopped call, which started before the swap and ends after it, runs to its
// end on it, as do the List calls before it, and the later ones run on 1.0.1; the campaign's result and credits
// are the oracle's, and every call's outcome is ok.
func TestAHotSwapFailsNoCallInFlight(t *testing.T) {
	t.Parallel()

	users := harness.NewDirectory(harness.DefaultUsers).Users()
	cursor := users[len(users)/2-1].ID // the directory's middle, where one of gold's pages starts

	var paused *harness.Paused

	rig := harness.NewRig(t, &harness.Options{Configure: func(cfg *platform.Config) {
		paused = harness.PausePage(cfg.Users, cursor)
		cfg.Users = paused
	}})
	campaign := springGold("swap-in-flight")
	expected := harness.Expect(t, rig.Users, &campaign)
	key := platform.Cohorts + "/gold"

	before, err := plg.Info(rig.Platform.Container(), key)
	require.NoError(t, err)

	team := rig.Teams[harness.Growth]
	manifest, err := os.ReadFile(filepath.Join(team.Dir, "gold", "plugin.yaml"))
	require.NoError(t, err)
	team.Write(t, "gold/plugin.yaml", string(manifest)+"version: 1.0.1\n")
	team.Commit(t, "Release gold 1.0.1")

	release := filepath.Join(team.CI(t, rig.Environ), "gold.plg")

	type ran struct {
		err    error
		result platform.Result
	}

	done := make(chan ran, 1)

	go func() {
		result, failed := rig.Platform.Run(t.Context(), &campaign)
		done <- ran{result: result, err: failed}
	}()

	paused.Await(t)

	swapping := time.Now()

	harness.PublishArtifact(t, rig.S3, rig.Bucket, release)
	require.NoError(t, rig.Platform.Reload(t.Context()))

	swapped := time.Now()
	after, err := plg.Info(rig.Platform.Container(), key)
	require.NoError(t, err)
	require.Equal(t, "1.0.1", after.Version, "the reload swapped gold")

	paused.Release()

	ended := <-done
	require.NoError(t, ended.err)
	assert.Equal(t, expected.Result, ended.result)
	assert.Equal(t, expected.Credits, rig.Wallet.CreditsOf(campaign.ID))

	for calls := range rig.Telemetry.Calls(t) {
		assert.Equal(t, "ok", calls.Outcome, "no call failed: %+v", calls)
	}

	run := campaignRun(t, rig.Telemetry, &campaign)
	lists := named(byParent(rig.Telemetry.Trace(run.TraceID()))[run.SpanID()], "plg.call "+key+".List")
	slices.SortFunc(lists, func(a, b tracetest.SpanStub) int { return a.StartTime.Compare(b.StartTime) })
	require.Len(t, lists, harness.ExpectPages(t, rig.Users, &campaign), "a List call per page")

	stopped := len(users) / 2 / harness.GoldPageSize // the List call reading the page at cursor
	assert.True(t, lists[stopped].StartTime.Before(swapping) && lists[stopped].EndTime.After(swapped),
		"the stopped List call was in flight across the swap")

	for i := range lists {
		want := map[bool]string{true: before.Version, false: after.Version}[i <= stopped]
		assert.Equal(t, want, attr(lists[i].Attributes, "version"), "List call %d", i)
	}
}

// edit replaces the one occurrence of old in the team's file path with replacement, as the team's author would.
func edit(t *testing.T, team *harness.Team, path, old, replacement string) {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(team.Dir, filepath.FromSlash(path)))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(content), old), "%s holds %s once", path, old)

	team.Write(t, path, strings.Replace(string(content), old, replacement, 1))
}

// goldUsers returns the gold users of a rig's default directory, in the order the gold cohort lists them.
func goldUsers() []harness.User {
	var gold []harness.User

	for _, user := range harness.NewDirectory(harness.DefaultUsers).Users() {
		if user.Tier == harness.Gold {
			gold = append(gold, user)
		}
	}

	return gold
}
