//go:build integration

package plg_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// validatorOwner owns the validating test plugins, so their reports hold only what validation found.
func validatorOwner() plg.Owner {
	return plg.Owner{Team: plg.Team{Name: "payments"}, Contact: plg.Contact{Email: "payments@example.com"}}
}

// putGo puts a Go plugin of the given version into store under key, from the files of a testdata plugin.
func putGo(t *testing.T, store *plg.MemoryStorage, key, version string, files fstest.MapFS) {
	t.Helper()

	owner := validatorOwner()
	require.NoError(t, store.Put(&plg.Descriptor{
		Key: key, Language: plg.LanguageGo, Entrypoint: ".", Version: version, Owner: &owner,
	}, files))
}

// goSource returns the plugin.go of a Go test plugin.
func goSource(t *testing.T, plugin string) string {
	t.Helper()

	src, err := fs.ReadFile(pluginFiles(plugin), "plugin.go")
	require.NoError(t, err)

	return string(src)
}

// sourceFS is a plugin directory holding src as its plugin.go.
func sourceFS(src string) fstest.MapFS {
	return fstest.MapFS{"plugin.go": {Data: []byte(src)}}
}

// checkByKey is a WithValidation function giving each plugin the check of its key, after the Register name.
func checkByKey(
	checks map[string]testcontract.Check,
) func(context.Context, plg.PluginInfo) (testcontract.Check, error) {
	return func(_ context.Context, info plg.PluginInfo) (testcontract.Check, error) {
		_, key, _ := strings.Cut(info.Key, "/")
		switch key {
		case "input":
			return testcontract.Check{}, errInputFailed
		case "inputpanic":
			panic("input panicked on purpose")
		default:
			return checks[key], nil
		}
	}
}

var errInputFailed = errors.New("no limits for this plugin")

// registerValidator registers the validator test plugin as name/key with opts, host and WithPool(1, 1) under
// hostileLimit, validated with check; Register's context carries "register" for testHost.Value.
func registerValidator(t *testing.T, ctr *plg.Container, name, key string, host testcontract.Host,
	check testcontract.Check, opts ...plg.RegisterOption,
) error {
	t.Helper()

	store := plg.NewMemoryStorage(name)
	putGo(t, store, key, "1", sourceFS(goSource(t, "validator")))

	opts = append([]plg.RegisterOption{
		plg.WithStorage(store), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit),
		plg.WithHost[testcontract.Host](host),
		plg.WithValidation(checkByKey(map[string]testcontract.Check{key: check})),
	}, opts...)

	return plg.Register[testcontract.Fooer](withHostValue(t.Context(), "register"), ctr, name, opts...)
}

// TestValidationAtRegister registers the validator plugin with each of its Validate behaviours: a valid plugin
// and one with only warnings go live with their issues in the load report and the log, in a pool whose
// instance never ran Validate; error issues, an error, a panic, a Validate looping past the validation timeout
// and a failing WithValidation function each fail Register, with no instance left and the memory given back.
func TestValidationAtRegister(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	// slog's JSON handler serializes its writes; the test reads once the container is closed.
	ctr := newContainer(t, plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))

	// The subtests share the container and run in turn: each compares the instances and memory left with what
	// the previous ones left.

	// A valid plugin goes live, in a pool whose instance never ran Validate.
	require.NoError(t, registerValidator(t, ctr, "valid", "p", newTestHost(), testcontract.Check{Mode: "valid"}))

	report, ok := plg.ServingReport(ctr, "valid/p")
	require.True(t, ok)
	assert.Empty(t, report.Issues)

	fooer := getFooer(t, ctr, "valid/p")
	validated, err := fooer.Foo(t.Context(), "validated")
	require.NoError(t, err)
	assert.Equal(t, "false", validated, "Validate ran in an instance of its own")

	count, err := fooer.Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count, "the serving instance's state is its own")
	assert.Equal(t, 1, plg.Instances(ctr), "the validation instance was closed")
	assert.Equal(t, uint64(hostileLimit), plg.MemoryInUse(ctr), "only the serving instance is charged")

	// Warnings and info go live, recorded in the report.
	require.NoError(t, registerValidator(t, ctr, "warn", "p", newTestHost(), testcontract.Check{Mode: "warn"}))

	report, ok = plg.ServingReport(ctr, "warn/p")
	require.True(t, ok)
	assert.Equal(t, []plg.Issue{
		{Severity: plg.SeverityWarning, Code: "validator.slow", Message: "slow", Field: "speed"},
		{Severity: plg.SeverityInfo, Code: "validator.note", Message: "noted"},
	}, report.Issues)

	// blocked registers a plugin that must fail validation, and checks nothing of it is left.
	blocked := func(name, key string, check testcontract.Check, opts ...plg.RegisterOption) error {
		t.Helper()

		instances, used, reserved := plg.Instances(ctr), plg.MemoryInUse(ctr), plg.MemoryReserved(ctr)

		loadErr := registerValidator(t, ctr, name, key, newTestHost(), check, opts...)
		require.ErrorIs(t, loadErr, plg.ErrLoad, name)
		assert.Contains(t, loadErr.Error(), name+"/"+key)
		assert.Equal(t, instances, plg.Instances(ctr), "%s: no instance is left", name)
		assert.Equal(t, used, plg.MemoryInUse(ctr), "%s: the instances' charges were given back", name)
		assert.Equal(t, reserved, plg.MemoryReserved(ctr), "%s: the reservation was given back", name)

		_, getErr := plg.Get[testcontract.Fooer](t.Context(), ctr, name+"/"+key)
		require.ErrorIs(t, getErr, plg.ErrNotFound, name)

		return loadErr
	}

	err = blocked("error", "p", testcontract.Check{Mode: "error"})
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Contains(t, err.Error(), "validator.broken: broken")
	assert.NotContains(t, err.Error(), "validator.slow", "a warning does not block")

	err = blocked("fail", "p", testcontract.Check{Mode: "fail"})
	require.ErrorIs(t, err, plg.ErrValidation)

	var pluginErr *plg.PluginError

	require.ErrorAs(t, err, &pluginErr)
	assert.Equal(t, "Validate", pluginErr.Method)
	assert.Equal(t, "validation broke", pluginErr.Message)

	err = blocked("panic", "p", testcontract.Check{Mode: "panic"})
	require.ErrorIs(t, err, plg.ErrValidation)

	var panicErr *plg.PanicError

	require.ErrorAs(t, err, &panicErr)
	assert.Equal(t, plg.PhaseCall, panicErr.Phase)
	assert.Equal(t, "Validate", panicErr.Method)
	assert.Contains(t, err.Error(), "Validate panicked on purpose")

	err = blocked("loop", "p", testcontract.Check{Mode: "loop"}, plg.WithValidationTimeout(100*time.Millisecond))
	require.ErrorIs(t, err, plg.ErrValidation)
	assert.Contains(t, err.Error(), "Validate did not return within the validation timeout (100ms, "+
		"WithValidationTimeout)")
	require.ErrorAs(t, err, &panicErr, "the stopped Validate is reported")
	assert.Equal(t, "Validate", panicErr.Method)

	err = blocked("input", "input", testcontract.Check{})
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Contains(t, err.Error(), plg.CodeValidationInput+": the WithValidation function failed: "+
		errInputFailed.Error())

	err = blocked("inputpanic", "inputpanic", testcontract.Check{})
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Contains(t, err.Error(), "the WithValidation function failed: panic: input panicked on purpose")

	require.NoError(t, ctr.Close(t.Context())) // every record is written

	codes := func(key string) []string {
		records := issueRecords(t, logs.Bytes(), key)
		got := make([]string, 0, len(records))

		for _, rec := range records {
			got = append(got, rec.Level+" "+rec.Code)
		}

		return got
	}

	assert.Empty(t, codes("valid/p"))
	assert.Equal(t, []string{"WARN validator.slow", "INFO validator.note"}, codes("warn/p"))
	assert.Equal(t, []string{"ERROR validator.broken", "WARN validator.slow"}, codes("error/p"))
	assert.Equal(t, []string{"ERROR " + plg.CodeValidationInput}, codes("input/input"))
}

// TestValidationNeedsAValidateOfTheExpectedSignature registers plugins whose instance has no Validate, or one of
// another signature: each fails Register with ErrValidation, the message giving the method expected and the ones
// the plugin declares. A Validate taking an extra leading parameter, which yaegi would bind, never runs.
func TestValidationNeedsAValidateOfTheExpectedSignature(t *testing.T) {
	t.Parallel()

	const want = "the plugin instance has no method Validate(ctx context.Context, in testcontract.Check) " +
		"([]plugin.Issue, error)"

	fooer := strings.Replace(goSource(t, "fooer"), `"github.com/heynemann/go-plg/internal/testcontract"`,
		`"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/plugin"`, 1)
	withValidate := func(signature string) string {
		return fooer + "\nfunc (*fooer) Validate" + signature + " {\n\tpanic(\"this Validate must never run\")\n}\n" +
			"\nvar _ plugin.Issue\n"
	}

	tests := []struct {
		name, src, declares string
	}{
		{name: "missing", src: goSource(t, "fooer"), declares: ", and the plugin declares no method Validate"},
		{
			name: "input of another type",
			src:  withValidate("(ctx context.Context, in string) ([]plugin.Issue, error)"),
			declares: "; the plugin declares func (*fooer) Validate(ctx context.Context, in string) " +
				"([]plugin.Issue, error) (",
		},
		{
			name: "no error result", src: withValidate("(ctx context.Context, in testcontract.Check) []plugin.Issue"),
			declares: "; the plugin declares func (*fooer) Validate(ctx context.Context, in testcontract.Check) " +
				"[]plugin.Issue (",
		},
		{
			name: "no context", src: withValidate("(in testcontract.Check) ([]plugin.Issue, error)"),
			declares: "; the plugin declares func (*fooer) Validate(in testcontract.Check) ([]plugin.Issue, error) (",
		},
		{
			name: "extra leading parameter",
			src:  withValidate("(n int, ctx context.Context, in testcontract.Check) ([]plugin.Issue, error)"),
			declares: "; the plugin declares func (*fooer) Validate(n int, ctx context.Context, in testcontract.Check) " +
				"([]plugin.Issue, error) (",
		},
	}

	ctr := newContainer(t)

	for _, tc := range tests {
		store := plg.NewMemoryStorage("sig")
		putGo(t, store, "p", "1", sourceFS(tc.src))

		err := plg.Register[testcontract.Fooer](t.Context(), ctr, "sig", plg.WithStorage(store), plg.WithPool(1, 1),
			plg.WithMemoryLimit(hostileLimit), plg.WithValidation(checkByKey(nil)))
		require.ErrorIs(t, err, plg.ErrLoad, tc.name)
		require.ErrorIs(t, err, plg.ErrValidation, tc.name)
		assert.Contains(t, err.Error(), want+tc.declares, tc.name)

		var failure *plg.PanicError

		assert.NotErrorAs(t, err, &failure, "%s: no Validate ran", tc.name)
		assert.Zero(t, plg.Instances(ctr), tc.name)
	}
}

// validationHost is the testHost recording, while a validation runs, the memory the container charges.
type validationHost struct {
	*testHost

	ctr     atomic.Pointer[plg.Container]
	charged atomic.Uint64
}

func (h *validationHost) Value(ctx context.Context) (string, error) {
	if plg.IsValidation(ctx) {
		h.charged.Store(max(h.charged.Load(), plg.MemoryInUse(h.ctr.Load())))
	}

	return h.testHost.Value(ctx)
}

// TestValidationWorksWithinTheVersionsReservation validates a plugin in a container whose budget holds exactly
// its worst case: the validation instance, charged while it runs, fits in the version's reservation, and its
// Create and Validate reach the Host, which sees IsValidation, while the serving instance's Create does not. A
// further plugin finds no room and fails at once with ErrMemoryBudget.
func TestValidationWorksWithinTheVersionsReservation(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t, plg.WithMemoryBudget(hostileLimit))
	host := &validationHost{testHost: newTestHost()}
	host.ctr.Store(ctr)

	require.NoError(t, registerValidator(t, ctr, "exact", "p", host, testcontract.Check{Mode: "host"}))

	assert.Equal(t, uint64(hostileLimit), host.charged.Load(), "the validation instance was charged")
	assert.Equal(t, uint64(hostileLimit), plg.MemoryInUse(ctr), "and released before the pool's instance started")

	report, ok := plg.ServingReport(ctr, "exact/p")
	require.True(t, ok)
	assert.Equal(t, []plg.Issue{
		{
			Severity: plg.SeverityInfo,
			Code:     "validator.host",
			Message:  "create=validating register validate=validating register",
		},
	}, report.Issues, "Create and Validate of the validation instance saw IsValidation")

	origin, err := getFooer(t, ctr, "exact/p").Foo(t.Context(), "origin")
	require.NoError(t, err)
	assert.Equal(t, "register", origin, "the serving instance's Create is no validation")

	err = registerValidator(t, ctr, "over", "p", newTestHost(), testcontract.Check{Mode: "valid"})
	require.ErrorIs(t, err, plg.ErrMemoryBudget)
	assert.Equal(t, 1, plg.Instances(ctr))
}

// TestValidationRequestsAreLoggedAsSuch validates a plugin whose Validate makes an HTTP request under the
// Register's policy: the request is served, and its log record carries validation=true.
func TestValidationRequestsAreLoggedAsSuch(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	api := newAPIServer(t)
	ctr := newContainer(t, plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))

	require.NoError(t, registerValidator(t, ctr, "web", "p", newTestHost(),
		testcontract.Check{Mode: "http", URL: api.URL + "/v1/check"}, plg.WithHTTP(openPolicy(hostOf(t, api.URL)))))

	report, ok := plg.ServingReport(ctr, "web/p")
	require.True(t, ok)
	require.Len(t, report.Issues, 1)
	assert.Equal(t, "200 GET /v1/check  "+hostOf(t, api.URL)+" 0", report.Issues[0].Message)
	assert.Equal(t, int64(1), api.hits.Load())

	require.NoError(t, ctr.Close(t.Context())) // every record is written

	var requests []map[string]any

	lines := bufio.NewScanner(bytes.NewReader(logs.Bytes()))
	for lines.Scan() {
		var rec map[string]any

		require.NoError(t, sonic.Unmarshal(lines.Bytes(), &rec))

		if rec["msg"] == "plg: http request" {
			requests = append(requests, rec)
		}
	}

	require.NoError(t, lines.Err())
	require.Len(t, requests, 1)
	assert.Equal(t, "web/p", requests[0]["key"])
	assert.Equal(t, true, requests[0]["validation"])
}

// TestValidationKeepsTheLastGoodVersion reloads updates of a validated plugin: one whose Validate reports an
// error issue, then one that fails to validate, are rejected while the last good version serves on, its
// instance untouched; a valid update replaces it.
func TestValidationKeepsTheLastGoodVersion(t *testing.T) {
	t.Parallel()

	modes := map[string]string{"1": "valid", "2": "error", "3": "panic", "4": "warn"}
	store := plg.NewMemoryStorage("kept")
	src := sourceFS(goSource(t, "validator"))
	putGo(t, store, "p", "1", src)

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "kept", plg.WithStorage(store),
		plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit), plg.WithHost[testcontract.Host](newTestHost()),
		plg.WithValidation(func(_ context.Context, info plg.PluginInfo) (testcontract.Check, error) {
			assert.Equal(t, plg.PluginInfo{
				Owner:    validatorOwner(),
				Key:      "kept/p",
				Version:  info.Version,
				Language: plg.LanguageGo,
				Storage:  "kept",
			}, info)

			return testcontract.Check{Mode: modes[info.Version]}, nil
		})))

	count := func() int {
		t.Helper()

		n, err := getFooer(t, ctr, "kept/p").Count(t.Context())
		require.NoError(t, err)

		return n
	}

	assert.Equal(t, 1, count())

	putGo(t, store, "p", "2", src)

	err := plg.ReloadInterface(t.Context(), ctr, "kept")
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Equal(t, "1", plg.Version(ctr, "kept/p"))
	assert.Equal(t, 2, count(), "the last good version's instance serves on")

	rejected, ok := plg.RejectedReport(ctr, "kept/p")
	require.True(t, ok)
	assert.Equal(t, "2", rejected.Version)
	require.ErrorIs(t, rejected.Err, plg.ErrBlockingIssue)
	assert.Equal(t, []plg.Issue{
		{Severity: plg.SeverityError, Code: "validator.broken", Message: "broken", Field: "max"},
		{Severity: plg.SeverityWarning, Code: "validator.slow", Message: "slow"},
	}, rejected.Issues)

	putGo(t, store, "p", "3", src)
	require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "kept"), plg.ErrValidation)
	assert.Equal(t, "1", plg.Version(ctr, "kept/p"))
	assert.Equal(t, 3, count())

	putGo(t, store, "p", "4", src)
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "kept"))
	assert.Equal(t, "4", plg.Version(ctr, "kept/p"))
	assert.Equal(t, 1, count(), "version 4 serves from a fresh instance")

	served, ok := plg.ServingReport(ctr, "kept/p")
	require.True(t, ok)
	assert.Len(t, served.Issues, 2)

	_, ok = plg.RejectedReport(ctr, "kept/p")
	assert.False(t, ok)
}

// TestScriptPluginsCannotValidateYet registers a Lua plugin with WithValidation: its load fails with
// ErrValidation before any instance starts.
func TestScriptPluginsCannotValidateYet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeLuaPlugin(t, root, "p", minimalOwner)

	ctr := newContainer(t)
	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "lua", plg.WithStorage(plg.NewFSStorage(root)),
		plg.WithPool(1, 1), plg.WithValidation(checkByKey(nil)))
	require.ErrorIs(t, err, plg.ErrValidation)
	require.ErrorContains(t, err, "lua plugins cannot validate yet")
	assert.Zero(t, plg.Instances(ctr))
}
