//go:build e2e && e2e_load

package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/e2e/harness"
	"github.com/heynemann/go-plg/e2e/platform"
)

// The load: a fixed amount of work rather than a duration, so that every run does the same work and the numbers of
// two runs compare.
const (
	// loadUsers is the directory's size; every campaign goes over all of it, gold's cohort a page at a time through
	// its Host, churn-risk's through the vendor's API.
	loadUsers = 10_000
	// warmUpCampaigns run first, unmeasured, so that the measured ones find the pools grown and the guests
	// compiled; the heap's baseline is taken after them.
	warmUpCampaigns = 8
	// loadCampaigns run measured, cycling through loadShapes.
	loadCampaigns = 56
	// campaignsAtOnce run at once, each processing membersAtOnce members at once (Limits.Concurrency): 64 members
	// in flight.
	campaignsAtOnce = 4
	membersAtOnce   = 16
	// Every swapEvery campaigns dispatched, a condition is republished while campaigns run: min-spend (Go),
	// country (Lua) and active-buyer (TypeScript) in turn, each to a released version, then each back, 6 swaps.
	swapEvery = 8
	// failOneIn and hangOneIn are the wallet's faults during the load: one request in failOneIn answered 503, one
	// in hangOneIn applied and held past the call timeout; the platform retries both with the same idempotency
	// key.
	failOneIn = 100
	hangOneIn = 1000
	// loadCallTimeout bounds each plugin call: a held request costs its member that long before its retry.
	loadCallTimeout = time.Second
	// poolMin and poolMax bound each plugin version's instances, so the pools grow under the load. poolMax stays
	// under the contracts' cap (//plg:limits pool=1..4), so that a platform passing plg more than it was given is
	// served, and caught by the pools' invariants, instead of refused.
	poolMin, poolMax = 2, 3
	// instanceMemory is every instance's memory limit (the Go guest's default, for every language), and
	// memoryBudget the platform's: every plugin's pool at its maximum, plus one for the version a swap loads beside
	// the one it replaces.
	instanceMemory = 128 << 20
	loadPlugins    = 6
	memoryBudget   = (loadPlugins + 1) * poolMax * instanceMemory
	// loadProcs is GOMAXPROCS during the test: a CI runner's CPUs, where the thresholds must hold too.
	loadProcs = 4
	// loadPollInterval is how often the platform polls the bucket, which is how a swap reaches it.
	loadPollInterval = 250 * time.Millisecond
	// Every sampleEvery members processed, the pools are sampled.
	sampleEvery = 256
	// releasedVersion is the version a swap releases a condition at (plugin.yaml's version).
	releasedVersion = "1.0.1"
	// percent turns a ratio into a percentage, for the log.
	percent = 100
	// mib is a mebibyte, for the log.
	mib = 1 << 20
)

// The load's thresholds, from the median of three runs on a workstation with GOMAXPROCS 4 (PLAN.md, "E2E: a
// rewards platform under make complex-e2e", has each run's numbers): 7,837 members/s, a p99 of 29.4 ms, no member
// failed, the host's heap 7.5% over its baseline. Each leaves the headroom a 4-CPU CI runner needs: a third of
// the throughput, three times the latency and the heap's growth.
const (
	// minUsersPerSecond is the throughput floor: members processed per second of the load (median 7,837).
	minUsersPerSecond = 2600
	// maxP99 is the ceiling of the members' 99th percentile processing time, their campaign.user spans (median
	// 29.4 ms).
	maxP99 = 90 * time.Millisecond
	// maxFailedUsers is the error budget: the share of the members a failure kept from a decision or a delivery,
	// 0.1%. None failed in the runs measured: retries absorbed every fault injected, 575 of the deliveries'
	// attempts.
	maxFailedUsers = 0.001
	// maxHeapGrowth is how much the host's own heap after a collection (heap.host) may grow over the load, from
	// its baseline after the warm-up (median 7.5%).
	maxHeapGrowth = 0.25
)

// TestLoadRewardsPlatformMeetsItsSLOs runs the platform under sustained mixed traffic: 56 campaigns, 4 at once,
// over a 10,000-user directory, in every language (gold and min-spend in Go, churn-risk and country in Lua,
// active-buyer and the wallet in TypeScript), a condition republished every 8 campaigns and picked up by a poll,
// and the wallet failing or holding a fraction of its requests. It asserts the service levels (throughput, the
// members' p99 latency, the failed members within the error budget, no reward lost or duplicated) and the
// invariants: the host's heap back near its baseline, every pool of the bounds and memory limit the platform was
// given and within them, and the instances' memory limits together within the budget, whenever sampled, every
// fault the wallet injected retried, every event a version loaded or retired or an instance a held request
// discarded, and no goroutine left once the platform closes.
//
//nolint:paralleltest // it measures throughput, latency, the heap and the goroutines: nothing may run beside it
func TestLoadRewardsPlatformMeetsItsSLOs(t *testing.T) {
	procs := runtime.GOMAXPROCS(loadProcs)

	t.Cleanup(func() { runtime.GOMAXPROCS(procs) })

	probe := newProbe()
	rig := harness.NewRig(t, &harness.Options{
		Users: loadUsers, PollInterval: loadPollInterval, Limits: loadLimits(), Configure: probe.configure(t),
	})
	container := rig.Platform.Container()
	probe.pools.container.Store(container)

	plan := prepareSwaps(t, rig)
	shapes := expectShapes(t, rig)

	probe.reset(phaseMembers(shapes, warmUpCampaigns))

	warmUp := runCampaigns(t, rig, probe, shapes, "warm-up", warmUpCampaigns, nil)
	verifyPhase(t, rig, probe, shapes, &warmUp)

	probe.reset(phaseMembers(shapes, loadCampaigns))

	baseline := measureHeap(container)

	rig.Wallet.FailOneIn(failOneIn)
	rig.Wallet.HangOneIn(hangOneIn)

	load := runCampaigns(t, rig, probe, shapes, "load", loadCampaigns, plan)
	rig.Wallet.FailOneIn(0)
	rig.Wallet.HangOneIn(0)

	totals := verifyPhase(t, rig, probe, shapes, &load)
	after := measureHeap(container)

	probe.pools.sample()
	logPlugins(t, container)
	assertIdle(t, container)

	slos := measureLoad(t, rig, probe, &load, &totals, baseline, after)
	assertSLOs(t, &slos, &totals)

	closing, cancel := context.WithTimeout(t.Context(), harness.CloseTimeout)
	defer cancel()

	require.NoError(t, rig.Platform.Close(closing), "the platform closed, every instance given back")

	leaked := goleak.Find(probe.goroutines, goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"))
	t.Logf("goroutines: %d before the platform started, %d after it closed", probe.goroutineCount,
		runtime.NumGoroutine())
	probe.drainEvents()
	t.Logf("events: %v", probe.kinds)

	assertInvariants(t, rig, probe, &load, &slos, &totals, len(plan), leaked)
}

// loadLimits are the platform's limits under the load.
func loadLimits() platform.Limits {
	return platform.Limits{
		Concurrency: membersAtOnce, CallTimeout: loadCallTimeout, PoolMin: poolMin, PoolMax: poolMax,
		MemoryLimit: instanceMemory, MemoryBudget: memoryBudget,
	}
}

// loadShapes are the campaigns the load cycles through: both cohorts, every condition, in every language.
func loadShapes() []platform.Campaign {
	return []platform.Campaign{{
		Cohort: "gold", Conditions: []string{"min-spend", "country", "active-buyer"}, Deliverer: "wallet",
		Reward: platform.Reward{Points: 500, Memo: "Spring bonus for gold members"},
	}, {
		Cohort: "churn-risk", Conditions: []string{"active-buyer"}, Deliverer: "wallet",
		Reward: platform.Reward{Points: 200, Memo: "We miss you"},
	}, {
		Cohort: "gold", Conditions: []string{"country"}, Deliverer: "wallet",
		Reward: platform.Reward{Points: 300, Memo: "Gold members in Brazil and Portugal"},
	}, {
		Cohort: "churn-risk", Conditions: []string{"min-spend", "country"}, Deliverer: "wallet",
		Reward: platform.Reward{Points: 400, Memo: "Big spenders we miss"},
	}}
}

// shape is a campaign of the load's cycle, without its ID, and what the oracle expects of it.
type shape struct {
	// eligible are the members every condition accepts: those it credits.
	eligible map[string]bool
	campaign platform.Campaign
	expected platform.Result
}

// expectShapes returns the load's shapes with the oracle's expectations over the rig's directory.
func expectShapes(t *testing.T, rig *harness.Rig) []shape {
	t.Helper()

	campaigns := loadShapes()
	shapes := make([]shape, len(campaigns))

	for i := range campaigns {
		expected := harness.Expect(t, rig.Users, &campaigns[i])
		shapes[i] = shape{campaign: campaigns[i], expected: expected.Result, eligible: map[string]bool{}}

		for _, credit := range expected.Credits {
			shapes[i].eligible[credit.User] = true
		}
	}

	return shapes
}

// phaseMembers is how many members a phase of n campaigns processes.
func phaseMembers(shapes []shape, n int) int {
	members := 0

	for i := range n {
		members += shapes[i%len(shapes)].expected.Members
	}

	return members
}

// swap republishes a condition: the artifact published and the version it serves.
type swap struct {
	key, file, version string
}

// prepareSwaps returns the load's swaps: min-spend (Go), country (Lua) and active-buyer (TypeScript) released at
// releasedVersion by their teams (plugin.yaml's version committed, the team's CI run again), then each one's
// first artifact again, at the version the platform serves now.
func prepareSwaps(t *testing.T, rig *harness.Rig) []swap {
	t.Helper()

	conditions := []struct{ team, name string }{
		{harness.Growth, "min-spend"}, {harness.CRM, "country"}, {harness.Payments, "active-buyer"},
	}
	released := make([]swap, 0, 2*len(conditions))
	restored := make([]swap, 0, len(conditions))

	for _, condition := range conditions {
		team, key := rig.Teams[condition.team], platform.Conditions+"/"+condition.name

		state, err := plg.Info(rig.Platform.Container(), key)
		require.NoError(t, err)

		first := keep(t, filepath.Join(team.Dir, "dist", condition.name+".plg"))
		restored = append(restored, swap{key: key, file: first, version: state.Version})

		manifest, err := os.ReadFile(filepath.Join(team.Dir, condition.name, "plugin.yaml"))
		require.NoError(t, err)
		team.Write(t, condition.name+"/plugin.yaml", string(manifest)+"version: "+releasedVersion+"\n")
		team.Commit(t, "Release "+condition.name+" "+releasedVersion)

		dist := team.CI(t, rig.Environ)
		released = append(released, swap{
			key: key, file: keep(t, filepath.Join(dist, condition.name+".plg")), version: releasedVersion,
		})
	}

	return append(released, restored...)
}

// keep copies the artifact file into a directory of t's, out of reach of the CI runs that follow.
func keep(t *testing.T, file string) string {
	t.Helper()

	data, err := os.ReadFile(file) //nolint:gosec // G304: an artifact a team's CI packed
	require.NoError(t, err)

	kept := filepath.Join(t.TempDir(), filepath.Base(file))
	require.NoError(t, os.WriteFile(kept, data, 0o600)) //nolint:gosec // G703: the test's own directory

	return kept
}

// phase is what a run of campaigns did.
type phase struct {
	// outcomes are the campaigns', in their order.
	outcomes []outcome
	// swaps are the swaps' times, from the artifact's publication to plg's event of its version loaded.
	swaps []time.Duration
	// elapsed is the phase's time, from the first campaign's start to the last one's end.
	elapsed time.Duration
}

// outcome is what one campaign returned.
type outcome struct {
	err      error
	campaign platform.Campaign
	result   platform.Result
}

// runCampaigns runs n campaigns named after name, campaignsAtOnce at once, cycling through the shapes, and swaps
// a condition, as plan says, every swapEvery campaigns dispatched, while the campaigns before it run.
func runCampaigns(
	t *testing.T, rig *harness.Rig, watch *probe, shapes []shape, name string, n int, plan []swap,
) phase {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	jobs, swaps := make(chan int), make(chan int, len(plan))
	done := phase{outcomes: make([]outcome, n)}

	var workers, dispatcher sync.WaitGroup

	// A swap failing ends the test here: the campaigns end with ctx, then the dispatcher and the workers.
	defer workers.Wait()
	defer dispatcher.Wait()
	defer cancel()

	start := time.Now()

	for range campaignsAtOnce {
		workers.Go(func() {
			for i := range jobs {
				campaign := shapes[i%len(shapes)].campaign
				campaign.ID = fmt.Sprintf("%s-%02d", name, i)
				result, err := rig.Platform.Run(ctx, &campaign)
				done.outcomes[i] = outcome{campaign: campaign, result: result, err: err}
			}
		})
	}

	dispatcher.Go(func() { dispatch(ctx, jobs, swaps, n, len(plan)) })

	for next := range swaps {
		done.swaps = append(done.swaps, swapCondition(t, rig, watch, &plan[next]))
	}

	dispatcher.Wait()
	workers.Wait()

	done.elapsed = time.Since(start)

	return done
}

// dispatch hands the campaigns 0 to n-1 to jobs, and the swaps 0 to planned-1 to swaps, swap s just before
// campaign (s+1)×swapEvery, until ctx ends; then it closes both.
func dispatch(ctx context.Context, jobs, swaps chan<- int, n, planned int) {
	defer close(jobs)
	defer close(swaps)

	for i := range n {
		if next := i/swapEvery - 1; i%swapEvery == 0 && next >= 0 && next < planned {
			swaps <- next
		}

		select {
		case jobs <- i:
		case <-ctx.Done():
			return
		}
	}
}

// swapCondition publishes the swap's artifact and waits for the platform's poll to load its version, which plg's
// loaded event says. It returns the time between the two.
func swapCondition(t *testing.T, rig *harness.Rig, watch *probe, next *swap) time.Duration {
	t.Helper()

	start := time.Now()

	require.Equal(t, next.key, harness.PublishArtifact(t, rig.S3, rig.Bucket, next.file))
	watch.awaitLoaded(t, next.key, next.version)

	return time.Since(start)
}

// totals are what a phase's campaigns did together, checked against the oracle.
type totals struct {
	failures    map[string]int // by the key of what failed
	lost        []string       // the idempotency keys of the rewards lost
	unexpected  []string       // the idempotency keys of the credits the oracle does not make
	members     int
	failed      int // members a failure kept from a decision or a delivery
	undelivered int // of them, those the deliverer failed
	delivered   int
	retries     int
	duplicates  int
}

// verifyPhase checks each campaign of the phase against the oracle (verifyCampaign), then has the wallet forget
// its credits, and returns their totals.
func verifyPhase(t *testing.T, rig *harness.Rig, watch *probe, shapes []shape, done *phase) totals {
	t.Helper()

	sum := totals{failures: map[string]int{}}

	for i := range done.outcomes {
		out := &done.outcomes[i]
		require.NoError(t, out.err, "%s ran to its end", out.campaign.ID)

		verifyCampaign(t, rig, watch, &shapes[i%len(shapes)], out, &sum)
		rig.Wallet.Forget(out.campaign.ID)

		sum.members += out.result.Members
		sum.delivered += out.result.Delivered
		sum.retries += out.result.Retries
		sum.duplicates += out.result.Duplicates

		for key, n := range out.result.Failures {
			sum.failures[key] += n
			sum.failed += n
		}
	}

	return sum
}

// verifyCampaign checks one campaign against its shape's oracle: every member listed, the failures it counted
// those of its members' spans, every member the oracle accepts eligible and delivered but those a failure kept
// from it. It adds to sum the members the deliverer failed, and the rewards lost and credits unexpected
// (compareCredits).
func verifyCampaign(t *testing.T, rig *harness.Rig, watch *probe, want *shape, out *outcome, sum *totals) {
	t.Helper()

	id, deliverer := out.campaign.ID, platform.Deliveries+"/"+out.campaign.Deliverer
	failed := watch.failedOf(id)
	undecided, undelivered := 0, 0

	for user, key := range failed {
		switch {
		case key == deliverer:
			undelivered++
		case want.eligible[user]:
			undecided++
		default: // undecided, but the oracle rejects the member anyway
		}
	}

	failures := 0
	for _, n := range out.result.Failures {
		failures += n
	}

	assert.Equal(t, want.expected.Members, out.result.Members, "%s listed every member", id)
	assert.Equal(t, len(failed), failures, "%s counted the failures its members' spans report", id)
	assert.Equal(t, len(want.eligible)-undecided, out.result.Eligible, "%s found the oracle's eligible", id)
	assert.Equal(t, out.result.Eligible-undelivered, out.result.Delivered, "%s delivered the eligible", id)

	compareCredits(&out.campaign, want.eligible, rig.Wallet.CreditsOf(id), failed, sum)

	sum.undelivered += undelivered
}

// compareCredits compares the wallet's credits of the campaign with the oracle's, the eligible members' reward.
// It adds to sum the rewards lost, the oracle's credits the wallet does not hold though no failure kept their
// member from a delivery, and the credits unexpected: of members the oracle does not credit, under another key, of
// another reward, or of a member a failure kept from a decision.
func compareCredits(
	campaign *platform.Campaign, eligible map[string]bool, credits map[string]harness.Credit,
	failed map[string]string, sum *totals,
) {
	deliverer := platform.Deliveries + "/" + campaign.Deliverer

	for user := range eligible {
		key := campaign.ID + "/" + user
		credit, credited := credits[key]
		reward := harness.Credit{
			User: user, Campaign: campaign.ID, Memo: campaign.Reward.Memo, Points: campaign.Reward.Points,
		}

		switch {
		case !credited && failed[user] == "":
			sum.lost = append(sum.lost, key)
		case credited && (credit != reward || failed[user] != "" && failed[user] != deliverer):
			sum.unexpected = append(sum.unexpected, key)
		default: // credited as the oracle says, or a failure kept the member from it
		}

		delete(credits, key)
	}

	for key := range credits {
		sum.unexpected = append(sum.unexpected, key)
	}
}

// heap is the host's heap after a collection.
type heap struct {
	// alloc is the heap's live bytes (runtime.MemStats.HeapAlloc).
	alloc uint64
	// guests is how much of it the instances' linear memory takes, as the pools last sampled it.
	guests uint64
	// allocated and mallocs are the bytes and objects allocated so far (TotalAlloc, Mallocs).
	allocated, mallocs uint64
}

// host is the heap's live bytes but the guests' linear memory: what the platform and plg hold beside the
// instances, whose number varies with the load (between the pools' bounds) and whose memory the budget bounds.
func (h heap) host() uint64 {
	return h.alloc - h.guests
}

// measureHeap collects the garbage and measures the heap. It collects twice: sync.Pool keeps what one collection
// frees for one more.
func measureHeap(container *plg.Container) heap {
	runtime.GC() //nolint:revive // call-to-gc: the heap's live bytes are measured after a collection
	runtime.GC() //nolint:revive // call-to-gc: and after one more, which frees what sync.Pool kept

	var stats runtime.MemStats

	runtime.ReadMemStats(&stats)

	measured := heap{alloc: stats.HeapAlloc, allocated: stats.TotalAlloc, mallocs: stats.Mallocs}

	states := plg.Plugins(container)
	for i := range states {
		measured.guests += states[i].Pool.Memory
	}

	return measured
}

// slos are the load's measured service levels.
type slos struct {
	p50, p95, p99, slowest time.Duration
	usersPerSecond         float64
	failedRatio            float64
	heapGrowth             float64
}

// measureLoad computes the load's service levels and logs every number measured.
func measureLoad(
	t *testing.T, rig *harness.Rig, watch *probe, load *phase, sum *totals, baseline, after heap,
) slos {
	t.Helper()

	latencies := watch.latencies[:min(int(watch.ended.Load()), len(watch.latencies))]
	slices.Sort(latencies)

	measured := slos{
		usersPerSecond: float64(sum.members) / load.elapsed.Seconds(),
		p50:            percentile(latencies, 0.50),
		p95:            percentile(latencies, 0.95),
		p99:            percentile(latencies, 0.99),
		slowest:        percentile(latencies, 1),
		failedRatio:    float64(sum.failed) / float64(sum.members),
		heapGrowth:     float64(after.host())/float64(baseline.host()) - 1,
	}

	t.Logf("load: %d campaigns (%d at once, %d members each at once) over %d users, GOMAXPROCS %d: %d members "+
		"in %v, %.0f users/s", loadCampaigns, campaignsAtOnce, membersAtOnce, loadUsers, loadProcs, sum.members,
		load.elapsed.Round(time.Millisecond), measured.usersPerSecond)
	t.Logf("latency per member (%d campaign.user spans): p50 %v, p95 %v, p99 %v, slowest %v", len(latencies),
		measured.p50, measured.p95, measured.p99, measured.slowest)
	t.Logf("failed members: %d (%.3f%%), by plugin %v; lost rewards %d, unexpected credits %d", sum.failed,
		measured.failedRatio*percent, sum.failures, len(sum.lost), len(sum.unexpected))
	t.Logf("deliveries: %d credited, %d retries, %d duplicates suppressed; the wallet: %d requests, %d answered "+
		"503, %d held past the call timeout, %d repeats, %d refused", sum.delivered, sum.retries, sum.duplicates,
		rig.Wallet.Requests(), rig.Wallet.Unavailable(), rig.Wallet.Held(), rig.Wallet.Repeats(),
		rig.Wallet.Refused())
	t.Logf("swaps: %d, publication to loaded %v", len(load.swaps), load.swaps)
	t.Logf("pools: %d samples, peak %d instances, %d calls waiting, %d MiB of guest memory (budget %d MiB), "+
		"peaks by plugin %v; %s under its minimum while refilling in %d samples", watch.pools.samples.Load(),
		watch.pools.instances.Load(), watch.pools.waiting.Load(), watch.pools.memory.Load()/mib,
		memoryBudget/mib, watch.pools.peaks(), discardedKey, watch.pools.refilling.Load())
	t.Logf("heap after GC: %d MiB after the warm-up, %d MiB of it guest memory; %d MiB after the load, %d MiB "+
		"guest memory; the host's own %d MiB, then %d MiB: %+.1f%%", baseline.alloc/mib, baseline.guests/mib,
		after.alloc/mib, after.guests/mib, baseline.host()/mib, after.host()/mib, measured.heapGrowth*percent)
	t.Logf("allocated during the load: %d MiB, %.1f KiB and %.0f objects per member",
		(after.allocated-baseline.allocated)/mib,
		float64(after.allocated-baseline.allocated)/float64(sum.members)/(1<<10),
		float64(after.mallocs-baseline.mallocs)/float64(sum.members))

	return measured
}

// percentile returns the q-th quantile of sorted (q from 0 to 1): its nearest-rank value.
func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}

	rank := int(math.Ceil(q * float64(len(sorted))))

	return sorted[min(max(rank, 1), len(sorted))-1]
}

// logPlugins logs each plugin's version and pool.
func logPlugins(t *testing.T, container *plg.Container) {
	t.Helper()

	states := plg.Plugins(container)
	for i := range states {
		state, pool := &states[i], &states[i].Pool
		t.Logf("%s %s (%s): %s, %d instances [%d, %d], %d grown, %d refilled, %d MiB", state.Key, state.Version,
			state.Language, state.Status, pool.Instances, pool.Min, pool.Max, pool.Grows, pool.Refills,
			pool.Memory/mib)
	}
}

// assertIdle asserts that every plugin's pool is idle once the load ended: each call gave its instance back, none
// is in use, none is waiting for one. An instance a call never gave back would leak there.
func assertIdle(t *testing.T, container *plg.Container) {
	t.Helper()

	states := plg.Plugins(container)
	for i := range states {
		pool := &states[i].Pool
		assert.Zero(t, pool.InUse, "%s: no instance in use", states[i].Key)
		assert.Zero(t, pool.CallsInFlight, "%s: no call in flight", states[i].Key)
		assert.Zero(t, pool.Waiting, "%s: no call waiting", states[i].Key)
	}
}

// assertSLOs asserts the load's service levels.
func assertSLOs(t *testing.T, measured *slos, sum *totals) {
	t.Helper()

	assert.GreaterOrEqual(t, measured.usersPerSecond, float64(minUsersPerSecond), "throughput, users/s")
	assert.LessOrEqual(t, measured.p99, maxP99, "p99 latency per member")
	assert.LessOrEqual(t, measured.failedRatio, maxFailedUsers, "failed members: the error budget")
	assert.Empty(t, sum.lost, "no reward lost")
	assert.Empty(t, sum.unexpected, "no reward duplicated, or credited where the oracle does not")
}

// assertInvariants asserts the load's invariants; swaps is how many swaps the load planned.
func assertInvariants(
	t *testing.T, rig *harness.Rig, watch *probe, load *phase, measured *slos, sum *totals, swaps int, leaked error,
) {
	t.Helper()

	assert.LessOrEqual(t, measured.heapGrowth, maxHeapGrowth, "the host's heap after the load, from its baseline")
	assert.Positive(t, watch.pools.samples.Load(), "the pools were sampled")
	assert.Zero(t, watch.pools.breaches.Load(), "every pool of the configured bounds and within them, the "+
		"instances' memory limits within the budget; the first breach: %s", watch.pools.firstBreach())
	assert.Len(t, load.swaps, swaps, "every swap went live")

	held, unavailable := rig.Wallet.Held(), rig.Wallet.Unavailable()
	require.Positive(t, held, "the wallet held requests")
	require.Positive(t, unavailable, "the wallet answered 503s")
	assert.Equal(t, held+unavailable, int64(sum.retries+sum.undelivered),
		"every request the wallet failed or held was retried, but a failed member's last")

	assert.Equal(t, map[plg.EventKind]int{
		plg.EventLoaded: loadPlugins + swaps, plg.EventRetired: swaps + loadPlugins, plg.EventPanicked: int(held),
	}, watch.kinds, "every event a version loaded (6 by New, one per swap), retired (one per swap, 6 by Close), or "+
		"the deliverer's instance a held request's call timeout discarded")
	assert.Empty(t, watch.unexpected, "no event but loads, retirements and the deliverer's discarded instances")
	assert.NoError(t, leaked, "no goroutine the platform started outlives its Close")
}

// probe watches the platform under load through the platform's own signals: its campaign.user spans (it is the
// span processor of the platform's only tracer), plg's pools (plg.Plugins, sampled as members are processed) and
// plg's events.
type probe struct {
	runs   *xsync.Map[trace.SpanID, string] // the campaign.run spans started: their campaign's ID
	failed *xsync.Map[string, string]       // "<campaign ID>/<member>" → the key of what failed the member
	events *harness.Events                  // what WithOnEvent passes
	pools  *poolWatch
	// kinds counts the events read from events, by kind, and unexpected describes those that are neither loads
	// nor retirements; only the test's goroutine reads events.
	kinds      map[plg.EventKind]int
	goroutines goleak.Option // the goroutines that ran before the platform started
	unexpected []string
	// latencies are the campaign.user spans' durations, in the order they ended since the last reset, ended of
	// them.
	latencies      []time.Duration
	ended          atomic.Int64
	goroutineCount int
}

var _ sdktrace.SpanProcessor = (*probe)(nil)

// newProbe returns a probe watching nothing yet.
func newProbe() *probe {
	return &probe{
		runs: xsync.NewMap[trace.SpanID, string](), failed: xsync.NewMap[string, string](),
		pools:  &poolWatch{byPlugin: xsync.NewMap[string, int]()},
		events: harness.NewEvents(), kinds: map[plg.EventKind]int{},
	}
}

// OnStart keeps the campaign of each campaign.run span, for the members' spans that are its children.
func (p *probe) OnStart(_ context.Context, span sdktrace.ReadWriteSpan) {
	if span.Name() == "campaign.run" {
		p.runs.Store(span.SpanContext().SpanID(), attr(span.Attributes(), "campaign.id"))
	}
}

// OnEnd records a member's processing time and failure from its campaign.user span, and samples the pools every
// sampleEvery members.
func (p *probe) OnEnd(span sdktrace.ReadOnlySpan) {
	if span.Name() != "campaign.user" {
		return
	}

	n := p.ended.Add(1)
	if n <= int64(len(p.latencies)) {
		p.latencies[n-1] = span.EndTime().Sub(span.StartTime())
	}

	if key := attr(span.Attributes(), "campaign.failed"); key != "" {
		campaign, _ := p.runs.Load(span.Parent().SpanID())
		p.failed.Store(campaign+"/"+attr(span.Attributes(), "user.id"), key)
	}

	if n%sampleEvery == 0 {
		p.pools.sample()
	}
}

// Shutdown does nothing: the probe holds nothing to release.
func (*probe) Shutdown(context.Context) error {
	return nil
}

// ForceFlush does nothing: the probe exports nothing.
func (*probe) ForceFlush(context.Context) error {
	return nil
}

// configure returns the rig's Configure: the platform traces through the probe, passes it its events and logs
// only errors, and the goroutines running before it starts are the baseline. plg logs every fault the load
// injects, a warning for each request the wallet holds beside the error of the call it stops, which the test
// counts from the wallet and plg's events instead.
func (p *probe) configure(t *testing.T) func(cfg *platform.Config) {
	t.Helper()

	return func(cfg *platform.Config) {
		provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p))

		t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.WithoutCancel(t.Context()))) })

		cfg.TracerProvider = provider
		cfg.OnEvent = p.events.OnEvent
		cfg.Logger = slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))
		p.goroutineCount = runtime.NumGoroutine()
		p.goroutines = goleak.IgnoreCurrent()
	}
}

// reset forgets the latencies recorded, making room for members more.
func (p *probe) reset(members int) {
	p.latencies = make([]time.Duration, members)
	p.ended.Store(0)
}

// failedOf returns the members of the campaign a failure kept from a decision or a delivery, with the key of what
// failed.
func (p *probe) failedOf(campaign string) map[string]string {
	failed := map[string]string{}

	for member, key := range p.failed.All() {
		if user, ok := strings.CutPrefix(member, campaign+"/"); ok {
			failed[user] = key
		}
	}

	return failed
}

// awaitLoaded reads the events until plg's loaded event of the key's version; the test fails when it does not come
// within a minute (harness.Events.Wait).
func (p *probe) awaitLoaded(t *testing.T, key, version string) {
	t.Helper()

	p.events.Wait(t, func(event plg.Event) bool {
		p.record(&event)

		return event.Kind == plg.EventLoaded && event.Key == key && event.Version == version
	})
}

// drainEvents reads the events queued.
func (p *probe) drainEvents() {
	drained := p.events.Drain()
	for i := range drained {
		p.record(&drained[i])
	}
}

// record counts an event read, and describes it among the unexpected unless it is a version loaded or retired, or
// a Deliver call of the deliverer stopped at its deadline (what a request the wallet held costs).
func (p *probe) record(event *plg.Event) {
	p.kinds[event.Kind]++

	switch {
	case event.Kind == plg.EventLoaded, event.Kind == plg.EventRetired:
	case event.Kind == plg.EventPanicked && event.Panic.Key == discardedKey &&
		event.Panic.Phase == plg.PhaseCall && event.Panic.Method == "Deliver" &&
		errors.Is(event.Panic, context.DeadlineExceeded):
	case event.Panic != nil:
		p.unexpected = append(p.unexpected, fmt.Sprintf("%s: %v", event.Kind, event.Panic))
	case event.Report != nil:
		p.unexpected = append(p.unexpected, fmt.Sprintf("%s %s %s: %v", event.Kind, event.Key, event.Version,
			event.Report.Err))
	default:
		p.unexpected = append(p.unexpected, fmt.Sprintf("%s %s %s", event.Kind, event.Key, event.Version))
	}
}

// poolWatch samples every plugin's pool through plg.Plugins and keeps what the invariants need: the breaches and
// the peaks. Safe for concurrent use.
type poolWatch struct {
	container atomic.Pointer[plg.Container]
	breach    atomic.Pointer[string]  // the first breach's description
	byPlugin  *xsync.Map[string, int] // the peak instances of each plugin's versions
	memory    atomic.Uint64           // the peak guest memory, every instance's together
	samples   atomic.Int64
	breaches  atomic.Int64
	refilling atomic.Int64 // the samples of the deliverer under its minimum (discardedKey)
	instances atomic.Int64 // the peak instances, every plugin's together
	waiting   atomic.Int64 // the peak calls waiting for an instance, every plugin's together
}

// discardedKey is the plugin whose instances the load discards: the deliverer, each time a request the wallet
// holds stops its call at the deadline. The pool's refiller replaces each one, a moment after its place is given
// back, so a sample in between finds the pool under its minimum: its bounds are counted with each count read on
// its own (plg.PoolStats), and only the maximum is an invariant at every sample for it.
const discardedKey = platform.Deliveries + "/wallet"

// sample samples every plugin's pool, counting each breach of an invariant: not the load's plugins, a plugin not
// serving, a pool not of the bounds and memory limit the platform was given (poolMin, poolMax, instanceMemory),
// instances out of those bounds, a plugin's guest memory over its pool's maximum, the instances' memory limits
// together over the budget (what plg reserves for them).
func (w *poolWatch) sample() {
	states := plg.Plugins(w.container.Load())
	w.samples.Add(1)

	if len(states) != loadPlugins {
		w.record(fmt.Sprintf("%d plugins, not the load's %d", len(states), loadPlugins))
	}

	var memory, reserved uint64

	instances, waiting := 0, 0

	for i := range states {
		pool, floor := &states[i].Pool, poolMin
		if states[i].Key == discardedKey {
			floor = 0

			if pool.Instances < poolMin {
				w.refilling.Add(1)
			}
		}

		if breach := breachOf(&states[i], floor); breach != "" {
			w.record(breach)
		}

		memory += pool.Memory
		reserved += uint64(pool.Instances) * pool.MemoryLimit //nolint:gosec // G115: a count of instances
		instances += pool.Instances
		waiting += pool.Waiting

		w.byPlugin.Compute(states[i].Key, func(peak int, _ bool) (int, xsync.ComputeOp) {
			return max(peak, pool.Instances), xsync.UpdateOp
		})
	}

	if reserved > memoryBudget {
		w.record(fmt.Sprintf("the instances' memory limits together, %d bytes, are over the budget, %d", reserved,
			memoryBudget))
	}

	raise(&w.instances, int64(instances))
	raise(&w.waiting, int64(waiting))

	for peak := w.memory.Load(); memory > peak && !w.memory.CompareAndSwap(peak, memory); {
		peak = w.memory.Load()
	}
}

// breachOf describes how a plugin's state breaches an invariant, its instances counted from floor at least; ""
// when it does not.
func breachOf(state *plg.PluginState, floor int) string {
	pool := &state.Pool

	switch {
	case state.Status != plg.StatusServing:
		return fmt.Sprintf("%s is %s", state.Key, state.Status)
	case pool.Min != poolMin || pool.Max != poolMax || pool.MemoryLimit != instanceMemory:
		return fmt.Sprintf("%s's pool is [%d, %d] of %d bytes each, not the [%d, %d] of %d the platform was given",
			state.Key, pool.Min, pool.Max, pool.MemoryLimit, poolMin, poolMax, instanceMemory)
	case pool.Instances < floor || pool.Instances > poolMax:
		return fmt.Sprintf("%s has %d instances, out of [%d, %d]", state.Key, pool.Instances, floor, poolMax)
	case pool.Memory > poolMax*instanceMemory:
		return fmt.Sprintf("%s's instances take %d bytes, over %d × %d", state.Key, pool.Memory, poolMax,
			instanceMemory)
	default:
		return ""
	}
}

// record counts a breach, keeping the first one's description.
func (w *poolWatch) record(breach string) {
	w.breaches.Add(1)
	w.breach.CompareAndSwap(nil, &breach)
}

// firstBreach describes the first breach; "" when there was none.
func (w *poolWatch) firstBreach() string {
	if breach := w.breach.Load(); breach != nil {
		return *breach
	}

	return ""
}

// peaks returns the peak instances of each plugin's versions.
func (w *poolWatch) peaks() map[string]int {
	return maps.Collect(w.byPlugin.All())
}

// raise raises peak to value when value is higher.
func raise(peak *atomic.Int64, value int64) {
	for current := peak.Load(); value > current && !peak.CompareAndSwap(current, value); {
		current = peak.Load()
	}
}
