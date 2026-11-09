//go:build integration

package plg_test

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// registerInspected registers the Lua validator plugin under name/key for each key, version 1, from a memory
// storage named "<name>-store" that it returns, with WithPool(1, 1) under hostileLimit, validated with checks.
func registerInspected(t *testing.T, ctr *plg.Container, name string, checks *liveChecks, keys []string,
	opts ...plg.RegisterOption,
) *plg.MemoryStorage {
	t.Helper()

	store := plg.NewMemoryStorage(name + "-store")
	for _, key := range keys {
		putValidator(t, store, luaLanguage(), key, "1")
	}

	opts = append([]plg.RegisterOption{
		plg.WithStorage(store), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit),
		plg.WithHost[testcontract.Host](newTestHost()), plg.WithValidation(checks.input()),
	}, opts...)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, name, opts...))

	return store
}

// TestInfoFollowsAPluginThroughEveryStatus takes Lua plugins through real loads, on-demand validations and
// reloads, and checks every field Info gives at each step: serving (its pool with a call in flight too),
// degraded by a failed validation, then by a panicking one (counted with its PanicError), serving again, serving
// its last good version beside a rejected update (a failed validation of that version outweighing it), then the
// update served with the key's panic count kept; a new plugin whose validation panics at its load, rejected and
// counted, gone once unlisted; and a version an on-demand validation unpublished, retired until a reload serves
// it again. Plugins lists what Info describes, in key order.
func TestInfoFollowsAPluginThroughEveryStatus(t *testing.T) {
	t.Parallel()

	const key = "live/p"

	ctr := newContainer(t)
	checks := newLiveChecks(map[string]string{key: "valid", "live/boom": "panic", "gone/p": "valid"})

	before := time.Now()
	store := registerInspected(t, ctr, "live", checks, []string{"p"}, plg.WithConcurrentCalls(4))
	after := time.Now()

	served := inspect(t, ctr, key)
	require.NotNil(t, served.Load)
	assert.Equal(t, plg.Report{
		At: served.Load.At, Owner: validatorOwner(), Key: key, Version: "1", Issues: []plg.Issue{develIssue()},
		Duration: served.Load.Duration,
	}, *served.Load, "a clean load, but for the test binary's contract checkout")
	assert.False(t, served.Load.At.Before(before) || served.LoadedAt.After(after), "loaded by the Register")
	assert.Equal(t, served.Load.At.Add(served.Load.Duration), served.LoadedAt)
	assert.Positive(t, served.Pool.Memory, "the instance's linear memory")

	want := plg.PluginState{
		LoadedAt: served.LoadedAt, Load: served.Load, Key: key, Registration: "live", Storage: "live-store",
		Language: plg.LanguageLua, Status: plg.StatusServing, Version: "1", Owner: validatorOwner(),
		Pool: plg.PoolStats{
			Min: 1, Max: 1, Instances: 1, IdleSlots: 4, MemoryLimit: hostileLimit, Memory: served.Pool.Memory,
		},
	}
	assert.Equal(t, want, served)

	release, err := plg.Hold(t.Context(), ctr, key) // a call in flight, on one of the instance's four lanes
	require.NoError(t, err)

	busy := want
	busy.Pool.InUse, busy.Pool.CallsInFlight, busy.Pool.IdleSlots = 1, 1, 3
	assert.Equal(t, busy, inspect(t, ctr, key))

	release()

	validate := func(mode string) plg.Report {
		t.Helper()

		checks.set(map[string]string{key: mode})

		report, validateErr := plg.Validate(t.Context(), ctr, key)
		require.NoError(t, validateErr, mode)

		return report
	}

	failed := validate("error")
	want.Status, want.Validation = plg.StatusDegraded, &failed
	assert.Equal(t, want, inspect(t, ctr, key), "degraded by a failed validation")

	panicked := validate("panic")

	var panicErr *plg.PanicError

	require.ErrorAs(t, panicked.Err, &panicErr)

	want.Validation, want.Panics, want.LastPanic = &panicked, 1, panicErr
	assert.Equal(t, want, inspect(t, ctr, key), "a panicking validation degrades it too, and counts")
	assert.Same(t, panicErr, inspect(t, ctr, key).LastPanic)

	passed := validate("valid")
	want.Status, want.Validation = plg.StatusServing, &passed
	assert.Equal(t, want, inspect(t, ctr, key), "serving again once a validation passes")

	// An update failing its validation at the reload: version 1 serves on, its reports kept.
	checks.set(map[string]string{key: "error"})
	putValidator(t, store, luaLanguage(), "p", "2")
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "live"), plg.ErrBlockingIssue)

	lastGood := inspect(t, ctr, key)
	require.NotNil(t, lastGood.Rejection)
	require.ErrorIs(t, lastGood.Rejection.Err, plg.ErrBlockingIssue)
	assert.Equal(t, plg.Report{
		At: lastGood.Rejection.At, Err: lastGood.Rejection.Err, Owner: validatorOwner(), Key: key, Version: "2",
		Issues: []plg.Issue{
			develIssue(),
			{Severity: plg.SeverityError, Code: "validator.broken", Message: "broken", Field: "max"},
			{Severity: plg.SeverityWarning, Code: "validator.slow", Message: "slow"},
		},
		Duration: lastGood.Rejection.Duration,
	}, *lastGood.Rejection)

	want.Status, want.Rejection, want.RejectedVersion = plg.StatusServingLastGood, lastGood.Rejection, "2"
	assert.Equal(t, want, lastGood)

	degraded := validate("error")
	want.Status, want.Validation = plg.StatusDegraded, &degraded
	assert.Equal(t, want, inspect(t, ctr, key), "a failed validation of the version served outweighs the update")

	checks.set(map[string]string{key: "valid"})
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "live"))

	updated := inspect(t, ctr, key)
	require.NotNil(t, updated.Load)
	assert.Equal(t, "2", updated.Load.Version)
	assert.True(t, updated.LoadedAt.After(served.LoadedAt))
	assert.Equal(t, plg.PluginState{
		LoadedAt: updated.LoadedAt, Load: updated.Load, LastPanic: panicErr, Key: key, Registration: "live",
		Storage: "live-store", Language: plg.LanguageLua, Status: plg.StatusServing, Version: "2",
		Owner: validatorOwner(), Panics: 1,
		Pool: plg.PoolStats{
			Min: 1, Max: 1, Instances: 1, IdleSlots: 4, MemoryLimit: hostileLimit, Memory: updated.Pool.Memory,
		},
	}, updated, "version 2 serves, nothing rejected or validated yet; the key's panics are kept")

	// A new plugin whose validation panics at its load: rejected, its panic counted.
	putValidator(t, store, luaLanguage(), "boom", "1")
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "live"), plg.ErrValidation)

	_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, "live/boom")
	require.ErrorIs(t, err, plg.ErrNotFound)

	boom := inspect(t, ctr, "live/boom")
	require.NotNil(t, boom.Rejection)
	assert.Equal(t, "1", boom.Rejection.Version)

	var boomPanic *plg.PanicError

	require.ErrorAs(t, boom.Rejection.Err, &boomPanic)
	assert.Equal(t, "Validate", boomPanic.Method)
	assert.Equal(t, plg.PluginState{
		Rejection: boom.Rejection, LastPanic: boomPanic, Key: "live/boom", Registration: "live",
		Storage: "live-store", Language: plg.LanguageLua, Status: plg.StatusRejected, RejectedVersion: "1",
		Owner: validatorOwner(), Panics: 1,
	}, boom)
	assert.Equal(t, updated, inspect(t, ctr, key), "the version served is untouched")

	// A version an on-demand validation unpublishes is retired until a reload serves it again.
	registerInspected(t, ctr, "gone", checks, []string{"p"}, plg.WithOnDemandValidationPolicy(plg.Unpublish))
	checks.set(map[string]string{"gone/p": "error"})

	unpublished, err := plg.Validate(t.Context(), ctr, "gone/p")
	require.NoError(t, err)
	require.ErrorIs(t, unpublished.Err, plg.ErrBlockingIssue)

	retired := inspect(t, ctr, "gone/p")
	assert.Equal(t, plg.PluginState{
		Rejection: &unpublished, Key: "gone/p", Registration: "gone", Storage: "gone-store",
		Language: plg.LanguageLua, Status: plg.StatusRetired, RejectedVersion: "1", Owner: validatorOwner(),
	}, retired)
	assert.Equal(t, []plg.PluginState{retired, boom, updated}, plg.Plugins(ctr), "in key order")

	store.Delete("boom")
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "live"))

	_, err = plg.Info(ctr, "live/boom")
	require.ErrorIs(t, err, plg.ErrNotFound, "no longer listed")

	checks.set(map[string]string{"gone/p": "valid"})
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "gone"))

	back := inspect(t, ctr, "gone/p")
	assert.Equal(t, plg.StatusServing, back.Status, "served again")
	assert.Equal(t, "1", back.Version)
	assert.Nil(t, back.Rejection)
	assert.Equal(t, []plg.PluginState{back, updated}, plg.Plugins(ctr))
}

// TestPluginsIsSafeUnderConcurrentReloads reads Plugins and Info while reloads publish new versions, reject
// updates and remove and add plugins back: every state read is whole and consistent (its version served matches
// its load report, a rejection goes with its rejected version, Plugins is in key order without repeats), and
// once the reloads stop each plugin is described as the last reload left it.
func TestPluginsIsSafeUnderConcurrentReloads(t *testing.T) {
	t.Parallel()

	owner := validatorOwner()
	store := plg.NewMemoryStorage("churn")
	put := func(key, version string, pluginOwner *plg.Owner) {
		t.Helper()

		require.NoError(t, store.Put(&plg.Descriptor{
			Key:        key,
			Implements: implementsOf[testcontract.Fooer](),
			Language:   plg.LanguageLua,
			Entrypoint: "plugin.lua",
			Version:    version,
			Owner:      pluginOwner,
		}, luaLanguage().files("fooer")))
	}

	for _, key := range []string{"a", "b", "c"} {
		put(key, "0", &owner)
	}

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "churn", plg.WithStorage(store),
		plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit), plg.WithRequiredOwnership()))

	done := make(chan struct{})

	var readers sync.WaitGroup

	stop := sync.OnceFunc(func() {
		close(done)
		readers.Wait()
	})
	defer stop() // also when a check below ends the test early

	for range 2 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					readConsistentStates(t, ctr)
				}
			}
		})
	}

	const rounds = 12

	for round := 1; round <= rounds; round++ {
		version := strconv.Itoa(round)
		put("a", version, &owner) // a new version every round

		if round%2 == 1 {
			put("b", version, nil) // rejected: no owner
			store.Delete("c")
			require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "churn"), plg.ErrBlockingIssue, round)
		} else {
			put("b", version, &owner)
			put("c", version, &owner)
			require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "churn"), round)
		}
	}

	stop()

	for _, key := range []string{"churn/a", "churn/b", "churn/c"} {
		state := inspect(t, ctr, key)
		assert.Equal(t, plg.StatusServing, state.Status, key)
		assert.Equal(t, strconv.Itoa(rounds), state.Version, key)
		assert.Nil(t, state.Rejection, key)
	}
}

// readConsistentStates reads every plugin of the churn registration through Plugins, then each through Info,
// checking each state read.
func readConsistentStates(t *testing.T, ctr *plg.Container) {
	t.Helper()

	states := plg.Plugins(ctr)
	for i := range states {
		assertConsistent(t, &states[i])

		if i > 0 {
			assert.Less(t, states[i-1].Key, states[i].Key, "in key order, no key twice")
		}
	}

	for _, key := range []string{"churn/a", "churn/b", "churn/c"} {
		state, err := plg.Info(ctr, key)
		if err == nil {
			assertConsistent(t, &state)
		} else {
			assert.ErrorIs(t, err, plg.ErrNotFound, key) // c, between its removal and its return
		}
	}
}

// assertConsistent checks that state, read while reloads run, is whole: what it says of the version served goes
// with its load report, and what it says of a rejection with the rejection's report.
func assertConsistent(t *testing.T, state *plg.PluginState) {
	t.Helper()

	assert.Equal(t, "churn", state.Registration)
	assert.True(t, strings.HasPrefix(state.Key, "churn/"), state.Key)
	assert.Equal(t, plg.LanguageLua, state.Language, state.Key)

	switch state.Status {
	case plg.StatusServing, plg.StatusServingLastGood:
		if assert.NotNil(t, state.Load, state.Key) {
			assert.Equal(t, state.Version, state.Load.Version, state.Key)
			assert.Equal(t, state.Key, state.Load.Key)
			assert.Equal(t, state.Load.At.Add(state.Load.Duration), state.LoadedAt, state.Key)
		}

		assert.Equal(t, 1, state.Pool.Max, state.Key)
		assert.Equal(t, uint64(hostileLimit), state.Pool.MemoryLimit, state.Key)
		assert.Equal(t, state.Status == plg.StatusServingLastGood, state.Rejection != nil, state.Key)
	case plg.StatusRejected:
		assert.Nil(t, state.Load, state.Key)
		assert.Empty(t, state.Version, state.Key)
		assert.Zero(t, state.Pool, state.Key)
		assert.NotNil(t, state.Rejection, state.Key)
	case plg.StatusDegraded, plg.StatusRetired, plg.StatusSkipped:
		assert.Fail(t, "a status no reload here leads to", "%s: %s", state.Key, state.Status)
	default:
		assert.Fail(t, "an unknown status", "%s: %q", state.Key, state.Status)
	}

	if state.Rejection != nil {
		assert.Equal(t, state.Rejection.Version, state.RejectedVersion, state.Key)
		assert.ErrorIs(t, state.Rejection.Err, plg.ErrBlockingIssue, state.Key)
	}
}
