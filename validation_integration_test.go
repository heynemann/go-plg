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

// The validation tests belong to the cross-language contract suite (RULES.md §2): each runs against the
// equivalent validator plugins of every language, testdata/plugins/<dir>/validator, whose Validate does what the
// input's Mode says, with the same outcomes.

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

// putValidator puts lang's validator plugin of the given version into store under key.
func putValidator(t *testing.T, store *plg.MemoryStorage, lang contractLanguage, key, version string) {
	t.Helper()

	owner := validatorOwner()
	require.NoError(t, store.Put(&plg.Descriptor{
		Key: key, Language: lang.lang, Entrypoint: lang.entry, Version: version, Owner: &owner,
	}, lang.files("validator")))
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

// eachValidatingLanguage runs test as a parallel subtest per contract language; validation does not depend on
// the binder.
func eachValidatingLanguage(t *testing.T, test func(t *testing.T, lang contractLanguage)) {
	t.Helper()

	for _, lang := range contractLanguages() {
		t.Run(lang.dir, func(t *testing.T) {
			t.Parallel()

			test(t, lang)
		})
	}
}

// validatePanic is what the PanicError of the validator plugin's panic mode holds in lang: Go plugins panic;
// script plugins, which fail any other way with an error, run out of memory.
func validatePanic(lang plg.Language) string {
	switch lang {
	case plg.LanguageGo:
		return "Validate panicked on purpose"
	case plg.LanguageLua:
		return "not enough memory"
	case plg.LanguageTypeScript:
		return "out of memory"
	default:
		panic("no validator plugin in " + string(lang))
	}
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

// registerValidator registers lang's validator plugin as name/key with opts, host and WithPool(1, 1) under
// hostileLimit, validated with check; Register's context carries "register" for testHost.Value.
func registerValidator(t *testing.T, ctr *plg.Container, lang contractLanguage, name, key string,
	host testcontract.Host, check testcontract.Check, opts ...plg.RegisterOption,
) error {
	t.Helper()

	store := plg.NewMemoryStorage(name)
	putValidator(t, store, lang, key, "1")

	opts = append([]plg.RegisterOption{
		plg.WithStorage(store), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit),
		plg.WithHost[testcontract.Host](host),
		plg.WithValidation(checkByKey(map[string]testcontract.Check{key: check})),
	}, opts...)

	return plg.Register[testcontract.Fooer](withHostValue(t.Context(), "register"), ctr, name, opts...)
}

// blockedValidation registers lang's validator plugin as name/key validated with check, which must fail its load,
// and checks nothing of it is left: no instance, charge or reservation, no catalogue entry.
func blockedValidation(t *testing.T, ctr *plg.Container, lang contractLanguage, name, key string,
	check testcontract.Check, opts ...plg.RegisterOption,
) error {
	t.Helper()

	instances, used, reserved := plg.Instances(ctr), plg.MemoryInUse(ctr), plg.MemoryReserved(ctr)

	err := registerValidator(t, ctr, lang, name, key, newTestHost(), check, opts...)
	require.ErrorIs(t, err, plg.ErrLoad, name)
	assert.Contains(t, err.Error(), name+"/"+key)
	assert.Equal(t, instances, plg.Instances(ctr), "%s: no instance is left", name)
	assert.Equal(t, used, plg.MemoryInUse(ctr), "%s: the instances' charges were given back", name)
	assert.Equal(t, reserved, plg.MemoryReserved(ctr), "%s: the reservation was given back", name)

	_, getErr := plg.Get[testcontract.Fooer](t.Context(), ctr, name+"/"+key)
	require.ErrorIs(t, getErr, plg.ErrNotFound, name)

	return err
}

// TestValidationAtRegister registers the validator plugin with each of its Validate behaviours: a valid plugin
// and one with only warnings go live with their issues in the load report and the log, in a pool whose
// instance never ran Validate; error issues, an issue of an unknown severity, a failing Validate, a panic, a
// Validate looping past the validation timeout and a failing WithValidation function each fail Register, with no
// instance left and the memory given back.
func TestValidationAtRegister(t *testing.T) {
	t.Parallel()

	eachValidatingLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		var logs bytes.Buffer

		// slog's JSON handler serializes its writes; the test reads once the container is closed.
		ctr := newContainer(t, plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))

		// The steps share the container and run in turn: each compares the instances and memory left with what
		// the previous ones left.
		validationGoesLive(t, ctr, lang)
		validationBlocks(t, ctr, lang)

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
		assert.Equal(t, []string{"ERROR validator.fatal"}, codes("unknown/p"), "an unknown severity is an error")
		assert.Equal(t, []string{"ERROR " + plg.CodeValidationInput}, codes("input/input"))
	})
}

// validationGoesLive registers a valid validator plugin and one with warnings: both go live, in a pool whose
// instance never ran Validate, the warnings recorded in the report.
func validationGoesLive(t *testing.T, ctr *plg.Container, lang contractLanguage) {
	t.Helper()

	require.NoError(t, registerValidator(t, ctr, lang, "valid", "p", newTestHost(), testcontract.Check{Mode: "valid"}))

	assert.Empty(t, inspect(t, ctr, "valid/p").Load.Issues)

	fooer := getFooer(t, ctr, "valid/p")
	validated, err := fooer.Foo(t.Context(), "validated")
	require.NoError(t, err)
	assert.Equal(t, "false", validated, "Validate ran in an instance of its own")

	count, err := fooer.Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count, "the serving instance's state is its own")
	assert.Equal(t, 1, plg.Instances(ctr), "the validation instance was closed")
	assert.Equal(t, uint64(hostileLimit), plg.MemoryInUse(ctr), "only the serving instance is charged")

	require.NoError(t, registerValidator(t, ctr, lang, "warn", "p", newTestHost(), testcontract.Check{Mode: "warn"}))

	assert.Equal(t, []plg.Issue{
		{Severity: plg.SeverityWarning, Code: "validator.slow", Message: "slow", Field: "speed"},
		{Severity: plg.SeverityInfo, Code: "validator.note", Message: "noted"},
	}, inspect(t, ctr, "warn/p").Load.Issues)
}

// validationBlocks registers the validator plugin with each behaviour that must fail its load.
func validationBlocks(t *testing.T, ctr *plg.Container, lang contractLanguage) {
	t.Helper()

	err := blockedValidation(t, ctr, lang, "error", "p", testcontract.Check{Mode: "error"})
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Contains(t, err.Error(), "validator.broken: broken")
	assert.NotContains(t, err.Error(), "validator.slow", "a warning does not block")

	err = blockedValidation(t, ctr, lang, "unknown", "p", testcontract.Check{Mode: "unknown"})
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Contains(t, err.Error(), "validator.fatal: fatal")

	err = blockedValidation(t, ctr, lang, "fail", "p", testcontract.Check{Mode: "fail"})
	require.ErrorIs(t, err, plg.ErrValidation)

	var pluginErr *plg.PluginError

	require.ErrorAs(t, err, &pluginErr)
	assert.Equal(t, "Validate", pluginErr.Method)
	assert.Contains(t, pluginErr.Message, "validation broke")

	err = blockedValidation(t, ctr, lang, "panic", "p", testcontract.Check{Mode: "panic"})
	require.ErrorIs(t, err, plg.ErrValidation)

	var panicErr *plg.PanicError

	require.ErrorAs(t, err, &panicErr)
	assert.Equal(t, plg.PhaseCall, panicErr.Phase)
	assert.Equal(t, "Validate", panicErr.Method)
	assert.Equal(t, lang.lang, panicErr.Language)
	assert.Contains(t, err.Error(), validatePanic(lang.lang))

	err = blockedValidation(t, ctr, lang, "loop", "p", testcontract.Check{Mode: "loop"},
		plg.WithValidationTimeout(100*time.Millisecond))
	require.ErrorIs(t, err, plg.ErrValidation)
	assert.Contains(t, err.Error(), "Validate did not return within the validation timeout (100ms, "+
		"WithValidationTimeout)")
	require.ErrorAs(t, err, &panicErr, "the stopped Validate is reported")
	assert.Equal(t, "Validate", panicErr.Method)

	err = blockedValidation(t, ctr, lang, "input", "input", testcontract.Check{})
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Contains(t, err.Error(), plg.CodeValidationInput+": the WithValidation function failed: "+
		errInputFailed.Error())

	err = blockedValidation(t, ctr, lang, "inputpanic", "inputpanic", testcontract.Check{})
	require.ErrorIs(t, err, plg.ErrBlockingIssue)
	assert.Contains(t, err.Error(), "the WithValidation function failed: panic: input panicked on purpose")
}

// TestScriptValidationFollowsTheLanguage registers the Lua and TypeScript validator plugins with the ways only
// their language has to fail or answer: Lua's error(...), TypeScript's rejected Promise, a Promise nothing is
// left to settle and one that never settles while its jobs run on (stopped by the validation timeout), and
// issues that do not fit plg.Issue each fail Register; TypeScript's async Validate goes live with its issues.
func TestScriptValidationFollowsTheLanguage(t *testing.T) {
	t.Parallel()

	type failure struct {
		mode, message string
		timeout       bool // stopped by the validation timeout, a PanicError
		plugin        bool // a PluginError whose message holds message
	}

	failures := map[plg.Language][]failure{
		plg.LanguageLua: {
			{mode: "raise", message: "validation broke", plugin: true},
			{mode: "malformed", message: "Validate returned issues that cannot cross the plugin boundary"},
		},
		plg.LanguageTypeScript: {
			{mode: "reject", message: "validation broke", plugin: true},
			{mode: "stuck", message: "the returned promise never settles", plugin: true},
			{
				mode: "pending", timeout: true,
				message: "Validate did not return within the validation timeout (100ms, WithValidationTimeout)",
			},
			{mode: "malformed", message: "Validate returned issues that cannot cross the plugin boundary"},
		},
	}

	for _, lang := range []contractLanguage{luaLanguage(), typescript()} {
		t.Run(lang.dir, func(t *testing.T) {
			t.Parallel()

			ctr := newContainer(t)

			for _, tc := range failures[lang.lang] {
				err := blockedValidation(t, ctr, lang, tc.mode, "p", testcontract.Check{Mode: tc.mode},
					plg.WithValidationTimeout(100*time.Millisecond))
				require.ErrorIs(t, err, plg.ErrValidation, tc.mode)
				assert.Contains(t, err.Error(), tc.message, tc.mode)

				var (
					pluginErr *plg.PluginError
					panicErr  *plg.PanicError
				)

				if tc.plugin {
					require.ErrorAs(t, err, &pluginErr, tc.mode)
					assert.Equal(t, "Validate", pluginErr.Method, tc.mode)
					assert.Contains(t, pluginErr.Message, tc.message, tc.mode)
				} else {
					assert.NotErrorAs(t, err, &pluginErr, tc.mode)
				}

				assert.Equal(t, tc.timeout, errors.As(err, &panicErr), "%s: reported as a panic", tc.mode)
			}

			if lang.lang != plg.LanguageTypeScript {
				return
			}

			require.NoError(t, registerValidator(t, ctr, lang, "async", "p", newTestHost(),
				testcontract.Check{Mode: "async"}))

			assert.Equal(t, []plg.Issue{
				{Severity: plg.SeverityWarning, Code: "validator.slow", Message: "slow", Field: "speed"},
				{Severity: plg.SeverityInfo, Code: "validator.note", Message: "noted"},
			}, inspect(t, ctr, "async/p").Load.Issues, "the issues the async Validate's Promise settled with")
		})
	}
}

// TestValidationNeedsAValidate registers each language's reference fooer, whose instance has no Validate: its
// load fails with ErrValidation, saying how the language's Validate looks, and nothing runs in its place.
func TestValidationNeedsAValidate(t *testing.T) {
	t.Parallel()

	missing := map[plg.Language]string{
		plg.LanguageGo: "the plugin instance has no method Validate(ctx context.Context, in testcontract.Check) " +
			"([]plugin.Issue, error), and the plugin declares no method Validate",
		plg.LanguageLua: "the table Create returned has no method Validate: inst:Validate(v) returns a list of issue " +
			"tables ({severity, code, message, field}), or nil and a message",
		plg.LanguageTypeScript: "the object Create returned has no method Validate: inst.Validate(v) returns an array " +
			"of issues ({severity, code, message, field}), or a Promise of one",
	}

	eachValidatingLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		ctr := newContainer(t)

		err := plg.Register[testcontract.Fooer](t.Context(), ctr, "missing",
			plg.WithStorage(lang.storage(t, "missing", map[string]string{"p": "fooer"})), plg.WithPool(1, 1),
			plg.WithMemoryLimit(hostileLimit), plg.WithValidation(checkByKey(nil)))
		require.ErrorIs(t, err, plg.ErrLoad)
		require.ErrorIs(t, err, plg.ErrValidation)
		assert.Contains(t, err.Error(), missing[lang.lang])

		var failure *plg.PanicError

		assert.NotErrorAs(t, err, &failure, "no Validate ran")
		assert.Zero(t, plg.Instances(ctr))
	})
}

// TestValidationNeedsAValidateOfTheExpectedSignature registers Go plugins whose instance has a Validate of
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

	eachValidatingLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		ctr := newContainer(t, plg.WithMemoryBudget(hostileLimit))
		host := &validationHost{testHost: newTestHost()}
		host.ctr.Store(ctr)

		require.NoError(t, registerValidator(t, ctr, lang, "exact", "p", host, testcontract.Check{Mode: "host"}))

		assert.Equal(t, uint64(hostileLimit), host.charged.Load(), "the validation instance was charged")
		assert.Equal(t, uint64(hostileLimit), plg.MemoryInUse(ctr), "and released before the pool's instance started")

		assert.Equal(t, []plg.Issue{
			{
				Severity: plg.SeverityInfo,
				Code:     "validator.host",
				Message:  "create=validating register validate=validating register",
			},
		}, inspect(t, ctr, "exact/p").Load.Issues, "Create and Validate of the validation instance saw IsValidation")

		origin, err := getFooer(t, ctr, "exact/p").Foo(t.Context(), "origin")
		require.NoError(t, err)
		assert.Equal(t, "register", origin, "the serving instance's Create is no validation")

		err = registerValidator(t, ctr, lang, "over", "p", newTestHost(), testcontract.Check{Mode: "valid"})
		require.ErrorIs(t, err, plg.ErrMemoryBudget)
		assert.Equal(t, 1, plg.Instances(ctr))
	})
}

// TestValidationRequestsAreLoggedAsSuch validates a plugin whose Validate makes an HTTP request under the
// Register's policy: the request is served, and its log record carries validation=true.
func TestValidationRequestsAreLoggedAsSuch(t *testing.T) {
	t.Parallel()

	eachValidatingLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		var logs bytes.Buffer

		api := newAPIServer(t)
		ctr := newContainer(t, plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))

		require.NoError(t, registerValidator(t, ctr, lang, "web", "p", newTestHost(),
			testcontract.Check{Mode: "http", URL: api.URL + "/v1/check"},
			plg.WithHTTP(openPolicy(hostOf(t, api.URL)))))

		report := inspect(t, ctr, "web/p").Load
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
	})
}

// TestValidationKeepsTheLastGoodVersion reloads updates of a validated plugin: one whose Validate reports an
// error issue, then one that fails to validate, are rejected while the last good version serves on, its
// instance untouched; a valid update replaces it.
func TestValidationKeepsTheLastGoodVersion(t *testing.T) {
	t.Parallel()

	eachValidatingLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		modes := map[string]string{"1": "valid", "2": "error", "3": "panic", "4": "warn"}
		store := plg.NewMemoryStorage("kept")
		putValidator(t, store, lang, "p", "1")

		ctr := newContainer(t)
		require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "kept", plg.WithStorage(store),
			plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit), plg.WithHost[testcontract.Host](newTestHost()),
			plg.WithValidation(func(_ context.Context, info plg.PluginInfo) (testcontract.Check, error) {
				assert.Equal(t, plg.PluginInfo{
					Owner:    validatorOwner(),
					Key:      "kept/p",
					Version:  info.Version,
					Language: lang.lang,
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

		putValidator(t, store, lang, "p", "2")

		err := plg.ReloadInterface(t.Context(), ctr, "kept")
		require.ErrorIs(t, err, plg.ErrBlockingIssue)

		kept := inspect(t, ctr, "kept/p")
		assert.Equal(t, "1", kept.Version)
		assert.Equal(t, plg.StatusServingLastGood, kept.Status)
		assert.Equal(t, 2, count(), "the last good version's instance serves on")

		rejected := kept.Rejection
		require.NotNil(t, rejected)
		assert.Equal(t, "2", rejected.Version)
		require.ErrorIs(t, rejected.Err, plg.ErrBlockingIssue)
		assert.Equal(t, []plg.Issue{
			{Severity: plg.SeverityError, Code: "validator.broken", Message: "broken", Field: "max"},
			{Severity: plg.SeverityWarning, Code: "validator.slow", Message: "slow"},
		}, rejected.Issues)

		putValidator(t, store, lang, "p", "3")
		require.ErrorIs(t, plg.ReloadInterface(t.Context(), ctr, "kept"), plg.ErrValidation)
		assert.Equal(t, "1", inspect(t, ctr, "kept/p").Version)
		assert.Equal(t, 3, count())

		putValidator(t, store, lang, "p", "4")
		require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "kept"))

		served := inspect(t, ctr, "kept/p")
		assert.Equal(t, "4", served.Version)
		assert.Equal(t, 1, count(), "version 4 serves from a fresh instance")
		assert.Len(t, served.Load.Issues, 2)
		assert.Nil(t, served.Rejection)
		assert.Equal(t, plg.StatusServing, served.Status)
	})
}
