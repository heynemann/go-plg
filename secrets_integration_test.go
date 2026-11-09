//go:build integration

package plg_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// vault is a WithSecrets resolver over fixed values: a name it has no value for fails with errSealed; it counts
// the reads it served by name and keeps the PluginInfo of each.
type vault struct {
	values map[string]string
	reads  *xsync.Map[string, int]
	infos  *xsync.Map[string, plg.PluginInfo]
}

// errSealed is the vault's failure, whose text no plugin may see.
var errSealed = errors.New("vault sealed at /v1/kv/plg (token s.4f8e)")

func newVault(values map[string]string) *vault {
	return &vault{values: values, reads: xsync.NewMap[string, int](), infos: xsync.NewMap[string, plg.PluginInfo]()}
}

//nolint:gocritic // hugeParam: WithSecrets fixes the signature
func (v *vault) resolve(_ context.Context, info plg.PluginInfo, name string) (string, error) {
	v.reads.Compute(name, func(old int, _ bool) (int, xsync.ComputeOp) { return old + 1, xsync.UpdateOp })
	v.infos.Store(name, info)

	value, ok := v.values[name]
	if !ok {
		return "", errSealed
	}

	return value, nil
}

func (v *vault) readsOf(name string) int {
	n, _ := v.reads.Load(name)

	return n
}

// secretsPlugin registers lang's secrets plugin as name+"/p" in ctr with opts, and returns it.
//
//nolint:ireturn // the plugin is served as the contract interface under test
func secretsPlugin(tb testing.TB, lang contractLanguage, ctr *plg.Container, name string, opts ...plg.RegisterOption,
) testcontract.Fooer {
	tb.Helper()

	opts = append([]plg.RegisterOption{
		plg.WithPool(1, 1), plg.WithStorage(lang.storage(tb, name, map[string]string{"p": "secrets"})),
	}, opts...)
	require.NoError(tb, plg.Register[testcontract.Fooer](tb.Context(), ctr, name, opts...))

	return getFooer(tb, ctr, name+"/p")
}

// loggingContainer returns a container logging JSON records to logs, closed when the test ends.
func loggingContainer(t *testing.T, logs *bytes.Buffer) *plg.Container {
	t.Helper()

	return httpContainer(t, logs)
}

// TestPluginsReadTheSecretsTheyDeclare: every language reads a declared secret, in its calls and in Create, from
// the Register's resolver, which learns which plugin version asks; undeclared names never reach the resolver; a
// failing or panicking resolver, or none, fails the read with a message that keeps the resolver's reason (which
// only the service's log has) from the plugin.
func TestPluginsReadTheSecretsTheyDeclare(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggingContainer(t, &logs)

	for _, lang := range contractLanguages() {
		t.Run(string(lang.lang), func(t *testing.T) {
			t.Parallel()

			keys := newVault(map[string]string{"api_key": "k-" + lang.dir, "create": "c-" + lang.dir})
			plug := secretsPlugin(t, lang, ctr, lang.dir, plg.WithSecrets(keys.resolve))

			got, err := plug.Foo(t.Context(), "api_key")
			require.NoError(t, err)
			assert.Equal(t, "k-"+lang.dir, got)

			got, err = plug.Foo(t.Context(), "@create")
			require.NoError(t, err)
			assert.Equal(t, "c-"+lang.dir, got, "Create read its secret")

			info, _ := keys.infos.Load("create")
			assert.Equal(t, plg.PluginInfo{Key: lang.dir + "/p", Version: "v1", Language: lang.lang, Storage: lang.dir},
				info)

			_, err = plug.Foo(t.Context(), "other")
			require.ErrorAs(t, err, new(*plg.PluginError))
			assert.Contains(t, err.Error(), `plg: secret "other" is not declared in the plugin's plugin.yaml `+
				`(it declares api_key, create, broken)`)
			assert.Zero(t, keys.readsOf("other"), "an undeclared name never reaches the resolver")

			_, err = plug.Foo(t.Context(), "broken")
			require.ErrorAs(t, err, new(*plg.PluginError))
			assert.Contains(t, err.Error(), `plg: secret "broken" could not be resolved (the service's log says why)`)
			assert.NotContains(t, err.Error(), "vault sealed")

			panicking := secretsPlugin(t, lang, ctr, lang.dir+"-panics", plg.WithSecrets(
				func(context.Context, plg.PluginInfo, string) (string, error) { panic("the vault client broke") }))

			got, err = panicking.Foo(t.Context(), "@create")
			require.NoError(t, err)
			assert.Equal(t, `error: plg: secret "create" could not be resolved (the service's log says why)`, got)

			unresolved := secretsPlugin(t, lang, ctr, lang.dir+"-none")

			_, err = unresolved.Foo(t.Context(), "api_key")
			require.ErrorContains(t, err, `plg: secret "api_key" cannot be resolved: the service registered no `+
				`secret resolver (plg.WithSecrets)`)
		})
	}

	t.Cleanup(func() { // after every subtest, before the container closes
		for _, lang := range contractLanguages() {
			assert.Contains(t, logs.String(), `"msg":"plg: resolving a secret failed","key":"`+lang.dir+
				`/p","version":"v1","secret":"broken","error":"vault sealed at /v1/kv/plg (token s.4f8e)"`)
		}

		panics := 0

		for _, failure := range ctr.RecentPanics() {
			if failure.Phase == plg.PhaseCallback && failure.Method == "WithSecrets" {
				assert.Equal(t, "the vault client broke", failure.Value)
				assert.True(t, strings.HasSuffix(failure.Key, "-panics/p"), failure.Key)

				panics++
			}
		}

		assert.Equal(t, len(contractLanguages()), panics, "each panicking resolver is reported")
	})
}

// TestSecretsRotateOnceTheirCacheExpires: a value serves a plugin version's reads for 30 seconds, then the next
// read resolves the secret again, so a rotated secret reaches the plugin without a reload.
func TestSecretsRotateOnceTheirCacheExpires(t *testing.T) {
	t.Parallel()

	for _, lang := range contractLanguages() {
		t.Run(string(lang.lang), func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				var rotations atomic.Int64

				ctr := newContainer(t)
				plug := secretsPlugin(t, lang, ctr, "rotating", plg.WithSecrets(
					func(_ context.Context, _ plg.PluginInfo, name string) (string, error) {
						return fmt.Sprintf("%s-%d", name, rotations.Add(1)), nil
					}))

				read := func() string {
					t.Helper()

					got, err := plug.Foo(t.Context(), "api_key")
					require.NoError(t, err)

					return got
				}

				// Create read "create" once: 1. The first read of api_key resolves it: 2.
				assert.Equal(t, "api_key-2", read())
				time.Sleep(29 * time.Second)
				assert.Equal(t, "api_key-2", read(), "cached while the value serves reads")
				assert.Equal(t, int64(2), rotations.Load())

				time.Sleep(time.Second)
				assert.Equal(t, "api_key-3", read(), "resolved again once 30 seconds passed")
				assert.Equal(t, "api_key-3", read())
				assert.Equal(t, int64(3), rotations.Load())

				require.NoError(t, ctr.Close(t.Context()))
			})
		})
	}
}

// TestSecretsAreScrubbedFromWhatPlgReports: a value the plugin read is replaced by "[secret <name>]" wherever plg
// reports the plugin: its output, the errors it returns, its panics (value and stderr) and its HTTP requests' log
// records; what the plugin itself gets is not scrubbed.
func TestSecretsAreScrubbedFromWhatPlgReports(t *testing.T) {
	t.Parallel()

	api := newAPIServer(t)

	var logs bytes.Buffer

	ctr := loggingContainer(t, &logs)
	values := map[string]string{}

	for _, lang := range contractLanguages() {
		value := "tok-" + lang.dir + "-7f3a9e"
		values[lang.dir] = value

		t.Run(string(lang.lang), func(t *testing.T) {
			t.Parallel()

			keys := newVault(map[string]string{"api_key": value, "create": "created-value"})
			plug := secretsPlugin(t, lang, ctr, lang.dir, plg.WithSecrets(keys.resolve),
				plg.WithHTTP(openPolicy("127.0.0.1")))

			require.NoError(t, plug.Fail(t.Context(), "print api_key"))

			err := plug.Fail(t.Context(), "error api_key")
			require.ErrorAs(t, err, new(*plg.PluginError))
			assert.Contains(t, err.Error(), "rejected [secret api_key]")

			err = plug.Fail(t.Context(), "panic api_key")
			require.Error(t, err)

			assertPanicScrubbed(t, lang.lang, err)

			require.NoError(t, plug.Fail(t.Context(), "fetch api_key "+api.URL))

			got, err := plug.Foo(t.Context(), "api_key")
			require.NoError(t, err)
			assert.Equal(t, value, got, "the plugin's own results are not scrubbed")
		})
	}

	t.Cleanup(func() { // after every subtest, before the container closes
		for dir, value := range values {
			assert.NotContains(t, logs.String(), value)

			for _, failure := range ctr.RecentPanics() {
				assert.NotContains(t, fmt.Sprint(failure.Value), value)
				assert.NotContains(t, failure.Guest, value)
			}

			key := `"key":"` + dir + `/p"`
			assert.Contains(t, logs.String(), key+`,"stream":"stdout","line":"token [secret api_key]"`)
			assert.Contains(t, logs.String(), `"msg":"plg: http request",`+key)
			assert.Contains(t, logs.String(), `"url":"`+api.URL+`/[secret api_key]"`)

			if dir == "typescript" { // Lua has no stderr; Go's holds tracebacks
				assert.Contains(t, logs.String(), key+`,"stream":"stderr","line":"stderr token [secret api_key]"`)
			}
		}

		assert.Equal(t, int64(len(values)), api.hits.Load(), "the requests carried the real value")
	})
}

// assertPanicScrubbed checks that err, a secrets plugin's failure on "panic api_key", tells of the leaked value by
// its mask: Lua's panic, error(), is the plugin's error; Go's is in the panic's value (its stderr is the
// traceback); TypeScript runs out of memory, so its stderr is what tells.
func assertPanicScrubbed(t *testing.T, lang plg.Language, err error) {
	t.Helper()

	if lang == plg.LanguageLua {
		require.ErrorAs(t, err, new(*plg.PluginError))
		assert.Contains(t, err.Error(), "leaked [secret api_key]")

		return
	}

	var failure *plg.PanicError

	require.ErrorAs(t, err, &failure)

	if lang == plg.LanguageGo {
		assert.Contains(t, failure.Value, "leaked [secret api_key]")
	} else {
		assert.Contains(t, failure.Guest, "dying with [secret api_key]")
	}
}

// TestSecretsAreScrubbedFromValidationAndCreate: a value the plugin read is replaced by "[secret <name>]" in the
// code, message and field of the issues its Validate returns, and in the error of a Create that fails with it.
func TestSecretsAreScrubbedFromValidationAndCreate(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)

	for _, lang := range contractLanguages() {
		t.Run(string(lang.lang), func(t *testing.T) {
			t.Parallel()

			value := "tok-" + lang.dir + "-5c1d2b"
			keys := newVault(map[string]string{"api_key": value, "create": "created-value"})
			secretsPlugin(t, lang, ctr, lang.dir, plg.WithSecrets(keys.resolve),
				plg.WithValidation(func(context.Context, plg.PluginInfo) (testcontract.Check, error) {
					return testcontract.Check{Mode: "api_key"}, nil
				}))

			report, err := plg.Validate(t.Context(), ctr, lang.dir+"/p")
			require.NoError(t, err)
			assert.Equal(t, []plg.Issue{{
				Severity: plg.SeverityInfo, Code: "secrets.[secret api_key]", Message: "rejected [secret api_key]",
				Field: "[secret api_key]",
			}}, report.Issues)

			refusing := newVault(map[string]string{"create": "fail-" + value})
			err = plg.Register[testcontract.Fooer](t.Context(), ctr, lang.dir+"-refuses", plg.WithPool(1, 1),
				plg.WithStorage(lang.storage(t, lang.dir+"-refuses", map[string]string{"p": "secrets"})),
				plg.WithSecrets(refusing.resolve))
			require.ErrorIs(t, err, plg.ErrLoad)
			assert.Contains(t, err.Error(), "refused [secret create]")
			assert.NotContains(t, err.Error(), value)
		})
	}
}

// TestChangingTheDeclaredSecretsReloadsThePlugin: plugin.yaml's secrets are part of the descriptor, so a reload
// loads a plugin whose secrets changed although its version did not.
func TestChangingTheDeclaredSecretsReloadsThePlugin(t *testing.T) {
	t.Parallel()

	lang := luaLanguage()
	store := plg.NewMemoryStorage("s")
	desc := lang.descriptor(t, "p", "secrets")
	desc.Secrets = []string{"api_key"}
	require.NoError(t, store.Put(&desc, lang.files("secrets")))

	ctr := newContainer(t)
	keys := newVault(map[string]string{"api_key": "a", "create": "created-value"})
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "s", plg.WithStorage(store),
		plg.WithSecrets(keys.resolve), plg.WithPool(1, 1)))

	_, err := getFooer(t, ctr, "s/p").Foo(t.Context(), "create")
	require.ErrorContains(t, err, `secret "create" is not declared`)

	desc.Secrets = []string{"api_key", "create"}
	require.NoError(t, store.Put(&desc, lang.files("secrets")))
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "s"))

	got, err := getFooer(t, ctr, "s/p").Foo(t.Context(), "create")
	require.NoError(t, err)
	assert.Equal(t, "created-value", got)
}

// BenchmarkSecretGo is one Go plugin call reading a cached secret (plugin.Secret).
func BenchmarkSecretGo(b *testing.B) {
	benchSecret(b, golang())
}

// BenchmarkSecretLua is one Lua plugin call reading a cached secret (secrets.get).
func BenchmarkSecretLua(b *testing.B) {
	benchSecret(b, luaLanguage())
}

// BenchmarkSecretTypeScript is one TypeScript plugin call reading a cached secret (secrets.get).
func BenchmarkSecretTypeScript(b *testing.B) {
	benchSecret(b, typescript())
}

// benchSecret measures a call whose secret read the version's cache answers: the resolver runs once, before the
// loop.
func benchSecret(b *testing.B, lang contractLanguage) {
	b.Helper()

	keys := newVault(map[string]string{"api_key": "bench-value", "create": "created-value"})
	plug := secretsPlugin(b, lang, newContainer(b), "bench", plg.WithSecrets(keys.resolve))
	ctx := b.Context()

	_, err := plug.Foo(ctx, "api_key")
	require.NoError(b, err)

	b.ReportAllocs()

	for b.Loop() {
		_, err := plug.Foo(ctx, "api_key")
		if err != nil {
			b.Fatal(err)
		}
	}

	b.StopTimer()

	if keys.readsOf("api_key") != 1 {
		b.Fatalf("the resolver ran %d times; the cache should answer every read in the loop", keys.readsOf("api_key"))
	}
}

// BenchmarkSecretGoParallel is BenchmarkSecretGo from GOMAXPROCS goroutines, each with an instance of its own
// (compare BenchmarkCallGoParallel/pool=N): every read is answered by the version's cache, which they share.
func BenchmarkSecretGoParallel(b *testing.B) {
	size := runtime.GOMAXPROCS(0)
	keys := newVault(map[string]string{"api_key": "bench-value", "create": "created-value"})
	plug := secretsPlugin(b, golang(), newContainer(b), "bench", plg.WithSecrets(keys.resolve),
		plg.WithPool(size, size))
	ctx := b.Context()

	_, err := plug.Foo(ctx, "api_key")
	require.NoError(b, err)

	b.ReportAllocs()
	b.ResetTimer() // after starting the instances and resolving the secret
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := plug.Foo(ctx, "api_key")
			if err != nil {
				b.Error(err)

				return
			}
		}
	})
	b.StopTimer()

	if keys.readsOf("api_key") != 1 {
		b.Fatalf("the resolver ran %d times; the cache should answer every read in the loop", keys.readsOf("api_key"))
	}
}
