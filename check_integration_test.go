//go:build integration

package plg_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// checkCandidate is the descriptor of a validator-owned Go candidate with the given key and version.
func checkCandidate(key, version string) *plg.Descriptor {
	owner := validatorOwner()

	return &plg.Descriptor{Key: key, Language: plg.LanguageGo, Entrypoint: ".", Version: version, Owner: &owner}
}

// TestCheckDryRunsACandidate checks candidates against a registration validating its plugins and requiring
// ownership: a good one passes with its issues, broken ones (validation issues, a failed Validate, source that
// does not compile, missing ownership, files over the snapshot limits, an invalid descriptor, a language the
// registration does not enable) report why; every check runs Create and Validate in an instance charged beyond
// the reservations and given back, and leaves the catalogue, the registration's reports and its plugins as they
// were. An unknown registration name is an error.
func TestCheckDryRunsACandidate(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t, plg.WithMemoryBudget(2*hostileLimit))
	host := &validationHost{testHost: newTestHost()}
	host.ctr.Store(ctr)

	store := plg.NewMemoryStorage("rules")
	putGo(t, store, "p", "1", sourceFS(goSource(t, "validator")))

	require.NoError(t, plg.Register[testcontract.Fooer](withHostValue(t.Context(), "register"), ctr, "rules",
		plg.WithStorage(store), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit),
		plg.WithHost[testcontract.Host](host), plg.WithRequiredOwnership(), plg.WithGolangPlugins(),
		plg.WithValidation(checkByKey(map[string]testcontract.Check{
			"p": {Mode: "valid"}, "good": {Mode: "host"}, "bad": {Mode: "error"}, "fail": {Mode: "fail"},
			"owner": {Mode: "valid"}, "big": {Mode: "valid"},
		}))))

	serving, ok := plg.ServingReport(ctr, "rules/p")
	require.True(t, ok)

	host.charged.Store(0)

	// untouched asserts the check left the container as it was: one serving instance, its charge and its
	// reservation only, the same serving report, and nothing published or rejected under key.
	untouched := func(key string) {
		t.Helper()

		assert.Equal(t, 1, plg.Instances(ctr), "%s: the check's instance was closed", key)
		assert.Equal(t, uint64(hostileLimit), plg.MemoryInUse(ctr), "%s: its charge was given back", key)
		assert.Equal(t, uint64(hostileLimit), plg.MemoryReserved(ctr), "%s: its reservation was given back", key)

		_, err := plg.Get[testcontract.Fooer](t.Context(), ctr, "rules/"+key)
		require.ErrorIs(t, err, plg.ErrNotFound, "%s is not published", key)

		_, rejected := plg.RejectedReport(ctr, "rules/"+key)
		assert.False(t, rejected, "%s: no rejection is kept", key)

		report, ok := plg.ServingReport(ctr, "rules/p")
		require.True(t, ok)
		assert.Equal(t, serving, report, "the serving version is unchanged")

		var keys []string

		for k := range plg.All[testcontract.Fooer](t.Context(), ctr) {
			keys = append(keys, k)
		}

		assert.Equal(t, []string{"rules/p"}, keys, "%s: the catalogue is unchanged", key)
	}

	// A good candidate, without a version, passes: its Create and Validate ran in an instance of their own,
	// marked as a validation and charged beside the serving instance.
	report, err := plg.Check(withHostValue(t.Context(), "check"), ctr, "rules", checkCandidate("good", ""),
		sourceFS(goSource(t, "validator")))
	require.NoError(t, err)
	assert.Equal(t, "rules/good", report.Key)
	assert.Equal(t, validatorOwner(), report.Owner)
	assert.NotEmpty(t, report.Version, "the content hash, as from a storage")
	require.NoError(t, report.Err)
	assert.Equal(t, []plg.Issue{
		{
			Severity: plg.SeverityInfo,
			Code:     "validator.host",
			Message:  "create=validating check validate=validating check",
		},
	}, report.Issues)
	assert.Equal(t, uint64(2*hostileLimit), host.charged.Load(), "the check's instance was charged")
	untouched("good")

	// failed checks the candidate, which would not load: Check succeeds, the report's Err saying why.
	failed := func(key string, desc *plg.Descriptor, files fstest.MapFS) (plg.Report, error) {
		t.Helper()

		got, checkErr := plg.Check(t.Context(), ctr, "rules", desc, files)
		require.NoError(t, checkErr, key)
		require.Error(t, got.Err, key)
		assert.Equal(t, "rules/"+key, got.Key)
		untouched(key)

		return got, got.Err
	}

	report, err = failed("bad", checkCandidate("bad", "2"), sourceFS(goSource(t, "validator")))
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Equal(t, "2", report.Version)
	assert.Equal(t, []plg.Issue{
		{Severity: plg.SeverityError, Code: "validator.broken", Message: "broken", Field: "max"},
		{Severity: plg.SeverityWarning, Code: "validator.slow", Message: "slow"},
	}, report.Issues)

	_, err = failed("fail", checkCandidate("fail", "2"), sourceFS(goSource(t, "validator")))
	require.ErrorIs(t, err, plg.ErrValidation)
	assert.Contains(t, err.Error(), "validation broke")

	_, err = failed("broken", checkCandidate("broken", "2"), sourceFS("package broken\n\nfunc Create( {\n"))
	require.NotErrorIs(t, err, plg.ErrValidation, "it never got as far")

	noOwner := checkCandidate("owner", "2")
	noOwner.Owner = nil
	report, err = failed("owner", noOwner, sourceFS(goSource(t, "validator")))
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	require.Len(t, report.Issues, 1)
	assert.Equal(t, plg.CodeOwnershipMissing, report.Issues[0].Code)
	assert.Equal(t, plg.SeverityError, report.Issues[0].Severity)

	big := sourceFS(goSource(t, "validator"))
	for i := range plg.MaxPluginEntries {
		big["data/"+strconv.Itoa(i)] = &fstest.MapFile{}
	}

	_, err = failed("big", checkCandidate("big", "2"), big)
	require.ErrorIs(t, err, plg.ErrPluginTooLarge)

	noEntrypoint := checkCandidate("noentry", "2")
	noEntrypoint.Entrypoint = ""
	_, err = failed("noentry", noEntrypoint, sourceFS(goSource(t, "validator")))
	require.ErrorIs(t, err, plg.ErrInvalidDescriptor)

	lua := checkCandidate("lua", "2")
	lua.Language = plg.LanguageLua
	_, err = failed("lua", lua, fstest.MapFS{"init.lua": {Data: []byte("return {}")}})
	require.ErrorIs(t, err, plg.ErrUnsupportedLanguage)

	// An unknown registration is an error, with no report.
	report, err = plg.Check(t.Context(), ctr, "nope", checkCandidate("good", "2"), sourceFS(goSource(t, "validator")))
	require.ErrorIs(t, err, plg.ErrNotFound)
	assert.Zero(t, report)
	untouched("good")
}

// TestCheckCreatesWithoutValidation checks candidates against a registration without WithValidation: Create
// runs, in an instance charged beyond the reservations, so a candidate whose Create fails is reported.
func TestCheckCreatesWithoutValidation(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	store := plg.NewMemoryStorage("plain")
	putGo(t, store, "p", "1", sourceFS(goSource(t, "fooer")))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "plain", plg.WithStorage(store),
		plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)))

	report, err := plg.Check(t.Context(), ctr, "plain", checkCandidate("q", "2"), sourceFS(goSource(t, "fooer")))
	require.NoError(t, err)
	assert.Empty(t, report.Issues)

	failing := strings.Replace(goSource(t, "fooer"), "func Create() (testcontract.Fooer, error) {",
		"func Create() (testcontract.Fooer, error) {\n\treturn nil, errors.New(\"no Create today\")\n", 1)
	require.NotEqual(t, goSource(t, "fooer"), failing, "the fooer's Create was found")

	if !strings.Contains(failing, `"errors"`) {
		failing = strings.Replace(failing, "import (", "import (\n\t\"errors\"", 1)
	}

	report, err = plg.Check(t.Context(), ctr, "plain", checkCandidate("q", "3"), sourceFS(failing))
	require.NoError(t, err)
	require.Error(t, report.Err)
	assert.Contains(t, report.Err.Error(), "no Create today")
	assert.Equal(t, 1, plg.Instances(ctr))
	assert.Equal(t, uint64(hostileLimit), plg.MemoryInUse(ctr))
	assert.Equal(t, uint64(hostileLimit), plg.MemoryReserved(ctr))
}

// TestCheckNeedsRoomBeyondTheReservations checks a candidate in a container whose budget the registration's
// reservation fills: Check fails at once with ErrMemoryBudget, its report saying so, leaving the serving
// plugin alone. Checks under an ended context fail with its error, and against a closed container with ErrClosed.
func TestCheckNeedsRoomBeyondTheReservations(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t, plg.WithMemoryBudget(hostileLimit))
	require.NoError(t, registerValidator(t, ctr, golang(), "full", "p", newTestHost(),
		testcontract.Check{Mode: "valid"}))

	report, err := plg.Check(t.Context(), ctr, "full", checkCandidate("p", "2"), sourceFS(goSource(t, "validator")))
	require.ErrorIs(t, err, plg.ErrMemoryBudget)
	require.ErrorIs(t, report.Err, plg.ErrMemoryBudget)
	assert.Equal(t, 1, plg.Instances(ctr))
	assert.Equal(t, uint64(hostileLimit), plg.MemoryInUse(ctr))
	assert.Equal(t, uint64(hostileLimit), plg.MemoryReserved(ctr))

	origin, err := getFooer(t, ctr, "full/p").Foo(t.Context(), "origin")
	require.NoError(t, err)
	assert.Equal(t, "register", origin)

	ended, cancel := context.WithCancel(t.Context())
	cancel()

	report, err = plg.Check(ended, ctr, "full", checkCandidate("p", "2"), sourceFS(goSource(t, "validator")))
	require.ErrorIs(t, err, context.Canceled, "an ended context ends the check")
	require.ErrorIs(t, report.Err, context.Canceled)

	require.NoError(t, ctr.Close(t.Context()))

	_, err = plg.Check(t.Context(), ctr, "full", checkCandidate("p", "2"), sourceFS(goSource(t, "validator")))
	require.ErrorIs(t, err, plg.ErrClosed)
}
