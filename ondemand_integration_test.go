//go:build integration

package plg_test

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"maps"
	"sync/atomic"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// liveChecks is a WithValidation function giving each plugin, by key, the validator mode the test sets, which
// it changes between validations; it counts the inputs it gives.
type liveChecks struct {
	modes atomic.Pointer[map[string]string]
	asked atomic.Int64
}

func newLiveChecks(modes map[string]string) *liveChecks {
	checks := &liveChecks{}
	checks.set(modes)

	return checks
}

// set replaces the modes of the keys given, keeping the others.
func (l *liveChecks) set(modes map[string]string) {
	next := map[string]string{}
	if old := l.modes.Load(); old != nil {
		maps.Copy(next, *old)
	}

	maps.Copy(next, modes)
	l.modes.Store(&next)
}

// input is the WithValidation function.
func (l *liveChecks) input() func(context.Context, plg.PluginInfo) (testcontract.Check, error) {
	return func(_ context.Context, info plg.PluginInfo) (testcontract.Check, error) {
		l.asked.Add(1)

		return testcontract.Check{Mode: (*l.modes.Load())[info.Key]}, nil
	}
}

// registerLive registers lang's validator test plugin as name/key for each key, version 1, with WithPool(1, 1)
// under hostileLimit, validated with checks.
func registerLive(t *testing.T, ctr *plg.Container, lang contractLanguage, name string, checks *liveChecks,
	keys []string, opts ...plg.RegisterOption,
) error {
	t.Helper()

	store := plg.NewMemoryStorage(name)
	for _, key := range keys {
		putValidator(t, store, lang, key, "1")
	}

	opts = append([]plg.RegisterOption{
		plg.WithStorage(store), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit),
		plg.WithHost[testcontract.Host](newTestHost()), plg.WithValidation(checks.input()),
	}, opts...)

	return plg.Register[testcontract.Fooer](t.Context(), ctr, name, opts...)
}

// validatorTags are the owner attributes of the validator plugins' metrics.
func validatorTags() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("team", "payments"), attribute.String("org", ""), attribute.String("company", ""),
	}
}

// validationPoint is the attribute set of a validation metric of key's plugin (a Go one).
func validationPoint(key, trigger, name, value string) []attribute.KeyValue {
	return append(validatorTags(), attribute.String("key", key), attribute.String("language", "go"),
		attribute.String("trigger", trigger), attribute.String(name, value))
}

// logMessages counts the log records (JSON lines) about key by message.
func logMessages(t *testing.T, logs []byte, key string) map[string]int {
	t.Helper()

	counts := map[string]int{}

	lines := bufio.NewScanner(bytes.NewReader(logs))
	for lines.Scan() {
		var rec struct {
			Msg string `json:"msg"`
			Key string `json:"key"`
		}

		require.NoError(t, sonic.Unmarshal(lines.Bytes(), &rec))

		if rec.Key == key {
			counts[rec.Msg]++
		}
	}

	require.NoError(t, lines.Err())

	return counts
}

// TestOnDemandValidationDegradesUntilAPassingOne validates a served plugin on demand under the default policy:
// each validation runs in a throwaway instance charged beyond the reservations and given back, the serving
// instance untouched; error issues, then a panicking Validate, mark the version degraded while it keeps
// serving, and a later passing validation clears it. The metrics count every run by outcome and its issues by
// severity (a Check's dry run apart), and the log records the issues and the changes.
func TestOnDemandValidationDegradesUntilAPassingOne(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	reader := newMetricsReader(t)
	ctr := newContainer(t, plg.WithMeterProvider(reader.provider), plg.WithMemoryBudget(2*hostileLimit),
		// slog's JSON handler serializes its writes; the test reads once the container is closed.
		plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))

	const key = "deg/p"

	checks := newLiveChecks(map[string]string{key: "valid"})
	require.NoError(t, registerLive(t, ctr, golang(), "deg", checks, []string{"p"}))

	fooer := getFooer(t, ctr, key)

	count, err := fooer.Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	validate := func(mode string) plg.Report {
		t.Helper()

		checks.set(map[string]string{key: mode})

		report, validateErr := plg.Validate(t.Context(), ctr, key)
		require.NoError(t, validateErr, mode)
		assert.Equal(t, key, report.Key, mode)
		assert.Equal(t, "1", report.Version, mode)
		assert.Equal(t, validatorOwner(), report.Owner, mode)
		assert.Positive(t, report.Duration, mode)
		assert.Equal(t, 1, plg.Instances(ctr), "%s: the validation instance was closed", mode)
		assert.Equal(t, uint64(hostileLimit), plg.MemoryInUse(ctr), "%s: its charge was given back", mode)
		assert.Equal(t, uint64(hostileLimit), plg.MemoryReserved(ctr), "%s: so was its extra room", mode)

		state := inspect(t, ctr, key)
		require.NotNil(t, state.Validation, mode)
		assert.Equal(t, report, *state.Validation, "%s: the report is kept with the version", mode)

		status := plg.StatusServing
		if report.Err != nil {
			status = plg.StatusDegraded
		}

		assert.Equal(t, status, state.Status, "%s: the version is degraded while it failed", mode)

		return report
	}

	assert.Nil(t, inspect(t, ctr, key).Validation, "no on-demand validation ran yet")

	report := validate("valid")
	require.NoError(t, report.Err)
	assert.Empty(t, report.Issues)

	report = validate("error")
	require.ErrorIs(t, report.Err, plg.ErrBlockingIssue)
	assert.Equal(t, []plg.Issue{
		{Severity: plg.SeverityError, Code: "validator.broken", Message: "broken", Field: "max"},
		{Severity: plg.SeverityWarning, Code: "validator.slow", Message: "slow"},
	}, report.Issues)

	count, err = fooer.Count(t.Context())
	require.NoError(t, err, "a degraded version keeps serving")
	assert.Equal(t, 2, count)

	report = validate("panic")
	require.ErrorIs(t, report.Err, plg.ErrValidation)

	var panicErr *plg.PanicError

	require.ErrorAs(t, report.Err, &panicErr)
	assert.Equal(t, "Validate", panicErr.Method)
	assert.Equal(t, key, panicErr.Key)

	report = validate("warn")
	require.NoError(t, report.Err, "warnings pass")
	assert.Len(t, report.Issues, 2)

	// A dry run of a candidate validates too, counted as such.
	owner := validatorOwner()
	checked, err := plg.Check(t.Context(), ctr, "deg", &plg.Descriptor{
		Key: "p", Implements: implementsOf[testcontract.Fooer](), Language: plg.LanguageGo, Entrypoint: ".",
		Version: "2", Owner: &owner,
	}, sourceFS(goSource(t, "validator")))
	require.NoError(t, err)
	require.NoError(t, checked.Err)
	assert.Equal(t, 1, plg.Instances(ctr))

	count, err = fooer.Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, count, "the serving instance kept its state through every validation")

	validated, err := fooer.Foo(t.Context(), "validated")
	require.NoError(t, err)
	assert.Equal(t, "false", validated, "Validate never ran in the serving instance")

	got := reader.collect(t)

	runs := func(trigger, outcome string) int64 {
		return got.int64Value(t, "plg.plugin.validations", validationPoint(key, trigger, "outcome", outcome)...)
	}
	issues := func(severity plg.Severity) int64 {
		return got.int64Value(t, "plg.plugin.validation.issues",
			validationPoint(key, "on_demand", "severity", string(severity))...)
	}

	assert.Equal(t, int64(1), runs("load", "passed"))
	assert.Equal(t, int64(2), runs("on_demand", "passed"))
	assert.Equal(t, int64(2), runs("on_demand", "failed"))
	assert.Equal(t, int64(1), runs("check", "passed"))
	assert.Equal(t, int64(1), issues(plg.SeverityError))
	assert.Equal(t, int64(2), issues(plg.SeverityWarning))
	assert.Equal(t, int64(1), issues(plg.SeverityInfo))

	require.NoError(t, ctr.Close(t.Context())) // every record is written

	messages := logMessages(t, logs.Bytes(), key)
	assert.Equal(t, 2, messages["plg: plugin degraded: its validation failed"])
	assert.Equal(t, 1, messages["plg: plugin validation passed again"])

	records := issueRecords(t, logs.Bytes(), key)
	codes := make([]string, 0, len(records))

	for _, rec := range records {
		codes = append(codes, rec.Level+" "+rec.Code)
	}

	assert.Equal(t, []string{
		"WARN " + plg.CodeContractDevel, // the load's: the test binary's contract is a local checkout
		"ERROR validator.broken", "WARN validator.slow", "WARN validator.slow", "INFO validator.note",
		"WARN " + plg.CodeContractDevel, "WARN validator.slow", "INFO validator.note", // the check's
	}, codes)
}

// TestOnDemandValidationUnpublishesUntilAPassingReload validates a served plugin on demand under Unpublish: a
// failing validation removes it from the catalogue at once while its call in flight drains, then its instance
// and reservation are released; the report is the key's rejection. A reload that still fails validation keeps it
// out, and one that passes publishes it again.
func TestOnDemandValidationUnpublishesUntilAPassingReload(t *testing.T) {
	t.Parallel()

	const key = "gone/p"

	ctr := newContainer(t)
	checks := newLiveChecks(map[string]string{key: "valid"})
	require.NoError(t, registerLive(t, ctr, golang(), "gone", checks, []string{"p"},
		plg.WithOnDemandValidationPolicy(plg.Unpublish)))

	fooer := getFooer(t, ctr, key)

	report, err := plg.Validate(t.Context(), ctr, key)
	require.NoError(t, err)
	require.NoError(t, report.Err, "a passing validation leaves the version published")
	getFooer(t, ctr, key)

	release, err := plg.Hold(t.Context(), ctr, key) // a call in flight
	require.NoError(t, err)

	checks.set(map[string]string{key: "error"})

	report, err = plg.Validate(t.Context(), ctr, key)
	require.NoError(t, err)
	require.ErrorIs(t, report.Err, plg.ErrBlockingIssue)

	_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, key)
	require.ErrorIs(t, err, plg.ErrNotFound)

	_, err = fooer.Count(t.Context())
	require.ErrorIs(t, err, plg.ErrNotFound, "calls through an adapter obtained earlier fail too")

	_, err = plg.Validate(t.Context(), ctr, key)
	require.ErrorIs(t, err, plg.ErrNotFound)

	retired := inspect(t, ctr, key)
	assert.Equal(t, plg.StatusRetired, retired.Status)
	require.NotNil(t, retired.Rejection, "the failed validation is the key's rejection")
	assert.Equal(t, report, *retired.Rejection)

	assert.Equal(t, 1, plg.Retired(ctr), "the version drains its call in flight")
	assert.Equal(t, 1, plg.Instances(ctr))

	release()
	require.NoError(t, plg.AwaitRetired(t.Context(), ctr))
	assert.Zero(t, plg.Instances(ctr), "the drained version's instance was closed")
	assert.Zero(t, plg.MemoryInUse(ctr))
	assert.Zero(t, plg.MemoryReserved(ctr), "and its reservation given back")

	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "gone"), plg.ErrBlockingIssue,
		"the reload loads it again, and its validation still fails")

	_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, key)
	require.ErrorIs(t, err, plg.ErrNotFound)
	assert.Zero(t, plg.Instances(ctr))

	rejected := inspect(t, ctr, key)
	assert.Equal(t, plg.StatusRejected, rejected.Status, "the reload's failed load is the rejection now")
	require.NotNil(t, rejected.Rejection)
	require.ErrorIs(t, rejected.Rejection.Err, plg.ErrBlockingIssue)

	checks.set(map[string]string{key: "valid"})
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "gone"))

	count, err := getFooer(t, ctr, key).Count(t.Context())
	require.NoError(t, err, "a passing reload publishes the plugin again")
	assert.Equal(t, 1, count, "from a fresh instance")

	served := inspect(t, ctr, key)
	assert.Equal(t, plg.StatusServing, served.Status)
	assert.Equal(t, "1", served.Version)
	assert.Nil(t, served.Rejection)
	assert.Nil(t, served.Validation, "the new version has no on-demand validation yet")
}

// TestValidateAllCoversEveryValidatingRegistration validates every plugin of two validating registrations, in
// key order, and skips a registration without validation, which Validate refuses; stopping the iteration stops
// the validations, and so does a context that ended.
func TestValidateAllCoversEveryValidatingRegistration(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	checks := newLiveChecks(map[string]string{"a/p": "valid", "a/q": "error", "b/p": "warn"})
	require.NoError(t, registerLive(t, ctr, golang(), "b", checks, []string{"p"}))

	checks.set(map[string]string{"a/q": "valid"}) // a/q fails on demand only
	require.NoError(t, registerLive(t, ctr, golang(), "a", checks, []string{"p", "q"}))
	checks.set(map[string]string{"a/q": "error"})

	registerFooers(t, ctr, "plain", map[string]string{"p": "fooer"}, plg.WithPool(1, 1),
		plg.WithMemoryLimit(hostileLimit))

	asked := checks.asked.Load()
	reports := map[string]plg.Report{}

	var keys []string

	for key, report := range plg.ValidateAll(t.Context(), ctr) {
		keys = append(keys, key)
		reports[key] = report
	}

	assert.Equal(t, []string{"a/p", "a/q", "b/p"}, keys)
	assert.Equal(t, asked+3, checks.asked.Load(), "one validation per validating plugin")
	require.NoError(t, reports["a/p"].Err)
	require.ErrorIs(t, reports["a/q"].Err, plg.ErrBlockingIssue)
	require.NoError(t, reports["b/p"].Err)
	assert.Len(t, reports["b/p"].Issues, 2)

	for _, key := range keys {
		assert.Equal(t, key, reports[key].Key)

		kept := inspect(t, ctr, key).Validation
		require.NotNil(t, kept, key)
		assert.Equal(t, reports[key].At, kept.At, "%s: each report is kept with its version", key)
	}

	_, err := plg.Validate(t.Context(), ctr, "plain/p")
	require.ErrorIs(t, err, plg.ErrNoValidation)

	asked = checks.asked.Load()

	for key := range plg.ValidateAll(t.Context(), ctr) {
		assert.Equal(t, "a/p", key)

		break
	}

	assert.Equal(t, asked+1, checks.asked.Load(), "stopping the iteration stops the validations")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	keys = keys[:0]

	for key, report := range plg.ValidateAll(ctx, ctr) {
		keys = append(keys, key)

		require.ErrorIs(t, report.Err, context.Canceled)
		assert.Equal(t, "1", report.Version)
	}

	assert.Equal(t, []string{"a/p"}, keys, "nothing more runs once the context ended")

	kept := inspect(t, ctr, "a/p")
	require.NotNil(t, kept.Validation)
	require.NoError(t, kept.Validation.Err, "a validation that could not run changes nothing")
	assert.Equal(t, plg.StatusServing, kept.Status)
	assert.Equal(t, 4, plg.Instances(ctr), "every validation instance was closed")
}

// TestOnDemandValidationNeedsRoomBeyondTheReservations validates a plugin in a container whose budget holds
// exactly its worst case: on demand there is no room beyond the version's reservation, so Validate and
// ValidateAll fail at once with ErrMemoryBudget, counted as unavailable, and nothing changes.
func TestOnDemandValidationNeedsRoomBeyondTheReservations(t *testing.T) {
	t.Parallel()

	const key = "full/p"

	reader := newMetricsReader(t)
	ctr := newContainer(t, plg.WithMemoryBudget(hostileLimit), plg.WithMeterProvider(reader.provider))
	checks := newLiveChecks(map[string]string{key: "valid"})
	require.NoError(t, registerLive(t, ctr, golang(), "full", checks, []string{"p"},
		plg.WithOnDemandValidationPolicy(plg.Unpublish)))

	checks.set(map[string]string{key: "error"}) // would unpublish it, had it run

	report, err := plg.Validate(t.Context(), ctr, key)
	require.ErrorIs(t, err, plg.ErrMemoryBudget)
	require.ErrorContains(t, err, "beyond the reservations")
	assert.Equal(t, err, report.Err, "the report says why too")
	assert.Equal(t, key, report.Key)
	assert.Equal(t, "1", report.Version)

	var reports []plg.Report

	for _, report := range plg.ValidateAll(t.Context(), ctr) {
		reports = append(reports, report)
	}

	require.Len(t, reports, 1)
	require.ErrorIs(t, reports[0].Err, plg.ErrMemoryBudget)
	assert.Equal(t, key, reports[0].Key)
	assert.Equal(t, validatorOwner(), reports[0].Owner)

	state := inspect(t, ctr, key)
	assert.Nil(t, state.Validation, "no report is kept")
	assert.Equal(t, plg.StatusServing, state.Status)

	count, err := getFooer(t, ctr, key).Count(t.Context())
	require.NoError(t, err, "the version still serves")
	assert.Equal(t, 1, count)
	assert.Equal(t, uint64(hostileLimit), plg.MemoryInUse(ctr))
	assert.Equal(t, uint64(hostileLimit), plg.MemoryReserved(ctr))

	got := reader.collect(t)
	assert.Equal(t, int64(2), got.int64Value(t, "plg.plugin.validations",
		validationPoint(key, "on_demand", "outcome", "unavailable")...))

	_, counted := got["plg.plugin.validation.issues"]
	assert.False(t, counted, "a validation that could not run counts no issue")
}

// TestOnDemandValidationInEveryLanguage validates a Go, a Lua and a TypeScript plugin on demand with identical
// outcomes: a passing validation, error issues degrading the version while it serves on, a failing Validate,
// then warnings clearing it.
func TestOnDemandValidationInEveryLanguage(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)

	eachValidatingLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		key := lang.dir + "/p"
		checks := newLiveChecks(map[string]string{key: "valid"})
		require.NoError(t, registerLive(t, ctr, lang, lang.dir, checks, []string{"p"}))

		for _, step := range []struct {
			err    error
			mode   string
			issues int
		}{
			{mode: "valid"},
			{mode: "error", err: plg.ErrBlockingIssue, issues: 2},
			{mode: "fail", err: plg.ErrValidation},
			{mode: "warn", issues: 2},
		} {
			checks.set(map[string]string{key: step.mode})

			report, err := plg.Validate(t.Context(), ctr, key)
			require.NoError(t, err, step.mode)
			require.ErrorIs(t, report.Err, step.err, step.mode)
			assert.Len(t, report.Issues, step.issues, step.mode)

			state := inspect(t, ctr, key)
			require.NotNil(t, state.Validation, step.mode)
			assert.Equal(t, report.Err, state.Validation.Err, step.mode)

			status := plg.StatusServing
			if step.err != nil {
				status = plg.StatusDegraded
			}

			assert.Equal(t, status, state.Status, "%s: degraded while it failed", step.mode)

			_, err = getFooer(t, ctr, key).Count(t.Context())
			require.NoError(t, err, "%s: the version serves on", step.mode)
		}
	})
}
