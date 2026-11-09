//go:build integration

package plg_test

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// TestOneStorageServesTwoInterfaces registers one storage holding a Fooer and a Shaper plugin under a Fooer and a
// Shaper registration, in every language: each registration serves the plugin implementing its own interface and
// skips the other, which Info describes as skipped for implementing another interface, at Register and at every
// reload.
func TestOneStorageServesTwoInterfaces(t *testing.T) {
	t.Parallel()

	for _, lang := range contractLanguages() {
		t.Run(lang.name(), func(t *testing.T) {
			t.Parallel()

			ctr := newContainer(t)
			store := lang.storage(t, "shared", map[string]string{"fooer": "fooer", "shaper": "shaper"})

			require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "fooers", plg.WithStorage(store),
				plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)))
			require.NoError(t, plg.Register[testcontract.Shaper](t.Context(), ctr, "shapers", plg.WithStorage(store),
				plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)))

			assertServesItsOwn := func(t *testing.T) {
				t.Helper()

				greeting, err := getFooer(t, ctr, "fooers/fooer").Foo(t.Context(), "shared")
				require.NoError(t, err)
				assert.Contains(t, greeting, "shared")

				shaper, err := plg.Get[testcontract.Shaper](t.Context(), ctr, "shapers/shaper")
				require.NoError(t, err)

				bumped, err := shaper.Bump(t.Context(), testcontract.Order{Lines: []testcontract.Line{{Qty: 1}}}, 2)
				require.NoError(t, err)
				require.Len(t, bumped.Lines, 1)
				assert.Equal(t, 3, bumped.Lines[0].Qty)

				for _, key := range []string{"fooers/shaper", "shapers/fooer"} {
					_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, key)
					require.ErrorIs(t, err, plg.ErrNotFound, key)

					state := inspect(t, ctr, key)
					assert.Equal(t, plg.StatusSkipped, state.Status, key)
					assert.Equal(t, lang.lang, state.Language, key)
					assert.Equal(t, "shared", state.Storage, key)
					require.NotNil(t, state.Rejection, key)
					require.ErrorIs(t, state.Rejection.Err, plg.ErrOtherInterface, key)
				}

				assert.ErrorContains(t, inspect(t, ctr, "fooers/shaper").Rejection.Err,
					"shaper implements github.com/heynemann/go-plg/internal/testcontract.Shaper, not "+
						"github.com/heynemann/go-plg/internal/testcontract.Fooer")
			}

			assertServesItsOwn(t)
			require.NoError(t, ctr.Reload(t.Context()))
			assertServesItsOwn(t)
			assert.Equal(t, 2, plg.Instances(ctr), "one instance each: no skipped plugin was loaded")
		})
	}
}

// TestPinnedContractVersionsAreChecked registers Lua plugins pinned to versions of the contract module against a
// program built with it at v1.4.0: plugins of the same major version and no newer minor load with no issue; a newer
// minor or another major version is rejected at Register, at a reload (the last good version serving on) and by
// Check, with an error issue naming both versions.
func TestPinnedContractVersionsAreChecked(t *testing.T) {
	t.Parallel()

	const built = "v1.4.0"

	lang := luaLanguage()
	pinned := func(store *plg.MemoryStorage, key, version string) {
		t.Helper()

		desc := lang.descriptor(t, key, "fooer")
		desc.Implements = "github.com/heynemann/go-plg/internal/testcontract.Fooer@" + version
		desc.Owner = new(validatorOwner())
		require.NoError(t, store.Put(&desc, lang.files("fooer")))
	}
	register := func(ctr *plg.Container, name string, store *plg.MemoryStorage) error {
		return plg.RegisterBuiltWith[testcontract.Fooer](t.Context(), ctr, name, built, plg.WithStorage(store),
			plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit))
	}

	var logs bytes.Buffer

	ctr := newContainer(t, plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))

	compatible := plg.NewMemoryStorage("compatible")
	for key, version := range map[string]string{
		"older": "v1.0.0", "same": "v1.4.0", "patch": "v1.4.9", "pre": "v1.4.0-rc.1",
		"pseudo": "v1.3.1-0.20261108120000-0123456789ab",
	} {
		pinned(compatible, key, version)
	}

	require.NoError(t, register(ctr, "ok", compatible))

	for _, key := range []string{"older", "same", "patch", "pre", "pseudo"} {
		state := inspect(t, ctr, "ok/"+key)
		assert.Equal(t, plg.StatusServing, state.Status, key)
		assert.Empty(t, state.Load.Issues, "%s: no issue, not even the local checkout's", key)

		_, err := getFooer(t, ctr, "ok/"+key).Foo(t.Context(), key)
		require.NoError(t, err, key)
	}

	incompatible := func(version, why string) string {
		return plg.CodeContractVersion + ": github.com/heynemann/go-plg: incompatible contract version: the plugin " +
			"is written against " + version + ", " + why + " the service's v1.4.0"
	}

	for version, why := range map[string]string{
		"v1.5.0": "a minor version newer than", "v2.0.0": "a major version other than",
		"v0.9.0": "a major version other than",
	} {
		store := plg.NewMemoryStorage("rejected")
		pinned(store, "p", version)

		err := register(ctr, "rejected", store)
		require.ErrorIs(t, err, plg.ErrLoad, version)
		require.ErrorIs(t, err, plg.ErrBlockingIssue, version)
		require.ErrorContains(t, err, incompatible(version, why), version)
		assert.Equal(t, 5, plg.Instances(ctr), "%s: no instance started", version)
	}

	// An update to another major version is rejected by the reload: the last good version serves on.
	pinned(compatible, "same", "v2.1.0")
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "ok"), plg.ErrBlockingIssue)

	state := inspect(t, ctr, "ok/same")
	assert.Equal(t, plg.StatusServingLastGood, state.Status)
	require.NotNil(t, state.Rejection)
	assert.Equal(t, []plg.Issue{{
		Severity: plg.SeverityError, Code: plg.CodeContractVersion, Field: "implements",
		Message: "github.com/heynemann/go-plg: incompatible contract version: the plugin is written against v2.1.0, " +
			"a major version other than the service's v1.4.0",
	}}, state.Rejection.Issues)

	// Check judges a candidate's pin as a load would.
	candidate := lang.descriptor(t, "next", "fooer")
	candidate.Implements = "github.com/heynemann/go-plg/internal/testcontract.Fooer@v1.6.0"
	report, err := plg.Check(t.Context(), ctr, "ok", &candidate, lang.files("fooer"))
	require.NoError(t, err)
	require.ErrorIs(t, report.Err, plg.ErrBlockingIssue)
	require.ErrorContains(t, report.Err, incompatible("v1.6.0", "a minor version newer than"))

	require.NoError(t, ctr.Close(t.Context())) // every record is written

	records := issueRecords(t, logs.Bytes(), "ok/same")
	require.Len(t, records, 1)
	assert.Equal(t, "ERROR", records[0].Level)
	assert.Equal(t, plg.CodeContractVersion, records[0].Code)
	assert.Equal(t, "v1", records[0].Version, "the same version: a change of implements is a change of descriptor")
}

// TestALocalCheckoutOfTheContractAcceptsWithAWarning registers a plugin pinned to any version in this test binary,
// whose contract module is its main module, built from a checkout (version (devel)): the pin cannot be checked, so
// the plugin loads with a warning issue naming the pin, logged at Warn.
func TestALocalCheckoutOfTheContractAcceptsWithAWarning(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := newContainer(t, plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	lang := luaLanguage()
	store := plg.NewMemoryStorage("devel")
	desc := lang.descriptor(t, "p", "fooer")
	desc.Implements = "github.com/heynemann/go-plg/internal/testcontract.Fooer@v9.3.0"
	desc.Owner = new(validatorOwner())
	require.NoError(t, store.Put(&desc, lang.files("fooer")))

	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "devel", plg.WithStorage(store),
		plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)))

	want := plg.Issue{
		Severity: plg.SeverityWarning, Code: plg.CodeContractDevel, Field: "implements",
		Message: "the service's contract module github.com/heynemann/go-plg is built from a local checkout, version " +
			"(devel): the plugin's v9.3.0 is not checked",
	}
	state := inspect(t, ctr, "devel/p")
	assert.Equal(t, plg.StatusServing, state.Status)
	assert.Equal(t, []plg.Issue{want}, state.Load.Issues)

	require.NoError(t, ctr.Close(t.Context())) // every record is written

	assert.Equal(t, []issueRecord{{
		Owner: map[string]any{"team": "payments", "email": "payments@example.com"}, Level: "WARN", Key: "devel/p",
		Version: "v1", Severity: "warning", Code: want.Code, Field: want.Field, Issue: want.Message,
	}}, issueRecords(t, logs.Bytes(), "devel/p"))
}
