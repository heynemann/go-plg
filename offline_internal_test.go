package plg

import (
	"context"
	"io/fs"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/directive"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/offline"
)

// offlineIface is a contract interface of this package's tests.
type offlineIface interface {
	Do(ctx context.Context, s string) (string, error)
}

// offlineSpec describes iface from reflection as plg validate describes a contract from its source, with
// source as the package's files.
func offlineSpec(t *testing.T, iface reflect.Type, source fs.FS) *offline.Contract {
	t.Helper()

	described, err := goabi.InterfaceOf(iface)
	require.NoError(t, err)

	spec := &offline.Contract{Interface: described, Source: source}
	for method := range iface.Methods() {
		spec.Methods = append(spec.Methods, method)
	}

	return spec
}

// TestCheckOfflineRefusesWhatRegisterRefuses checks plugins against contracts plg validate could describe but plg
// cannot serve, before any guest runs: an invalid manifest, an interface without methods, a contract without
// source and a Host of another package fail as Register and RegisterContract fail.
func TestCheckOfflineRefusesWhatRegisterRefuses(t *testing.T) {
	t.Parallel()

	source := fstest.MapFS{"c.go": {Data: []byte("package c\n")}}
	spec := offlineSpec(t, reflect.TypeFor[offlineIface](), source)
	plugin := &offline.Plugin{
		Key: "p", Files: fstest.MapFS{},
		Manifest: []byte("implements: github.com/heynemann/go-plg.offlineIface@v1.0.0\n" +
			"language: lua\nentrypoint: plugin.lua\n"),
	}
	opts := &offline.Options{}

	_, err := offline.Check(t.Context(), spec, &offline.Plugin{Key: "p", Manifest: []byte("language: lua\n")}, opts)
	require.ErrorIs(t, err, ErrInvalidManifest)

	empty := *spec
	empty.Methods = nil
	_, err = offline.Check(t.Context(), &empty, plugin, opts)
	require.ErrorIs(t, err, ErrInvalidInterface)
	require.ErrorContains(t, err, "plg.offlineIface: declares no methods")

	sourceless := *spec
	sourceless.Source = nil
	_, err = offline.Check(t.Context(), &sourceless, plugin, opts)
	require.ErrorIs(t, err, ErrInvalidContract)
	require.ErrorContains(t, err, "nil Source")

	other := *spec.Interface.Type
	other.Path = "example.com/other"
	hosted := *spec
	hosted.Host = &offline.Host{
		Interface: &goabi.Interface{Type: &other, Methods: spec.Interface.Methods}, Methods: spec.Methods,
	}
	_, err = offline.Check(t.Context(), &hosted, plugin, opts)
	require.ErrorIs(t, err, ErrInvalidHost)
	require.ErrorContains(t, err, "must be declared in the contract package")
}

// TestCheckOfflineRunsUnderTheLimitsAsRegisterOptions: the limits and guest plg validate passes are Register's
// options, refused as Register refuses them, before any guest runs.
func TestCheckOfflineRunsUnderTheLimitsAsRegisterOptions(t *testing.T) {
	t.Parallel()

	spec := offlineSpec(t, reflect.TypeFor[offlineIface](), fstest.MapFS{"c.go": {Data: []byte("package c\n")}})
	plugin := &offline.Plugin{
		Key: "p", Files: fstest.MapFS{},
		Manifest: []byte("implements: github.com/heynemann/go-plg.offlineIface@v1.0.0\n" +
			"language: lua\nentrypoint: plugin.lua\n"),
	}

	for _, tc := range []struct {
		msg  string
		opts offline.Options
	}{
		{msg: "WithPool(3, 2)", opts: offline.Options{Limits: directive.Limits{PoolMin: 3, PoolMax: 2}}},
		{msg: "WithMemoryLimit", opts: offline.Options{Limits: directive.Limits{Memory: 1}}},
		{msg: "WithGoGuest: the module is empty", opts: offline.Options{GoGuest: []byte{}}},
	} {
		_, err := offline.Check(t.Context(), spec, plugin, &tc.opts)
		require.ErrorIs(t, err, ErrInvalidOption, tc.msg)
		require.ErrorContains(t, err, tc.msg)
	}
}

// TestOfflineDefaultsAreRegistersDefaults: plg validate prints and runs under the defaults a Register without
// WithMemoryLimit and WithPool has, per language.
func TestOfflineDefaultsAreRegistersDefaults(t *testing.T) {
	t.Parallel()

	pool := min(runtime.GOMAXPROCS(0), maxPoolSize)

	for lang, memory := range map[Language]uint64{
		LanguageGo: 128 << 20, LanguageLua: 64 << 20, LanguageTypeScript: 64 << 20, "cobol": 0,
	} {
		assert.Equal(t, directive.Limits{Memory: memory, PoolMin: 1, PoolMax: pool}, offline.Defaults(string(lang)),
			lang)
	}
}

// TestLuaLongStringHoldsAnyText embeds texts holding the closing brackets of the shorter long strings, at their end
// too, and a first newline (which Lua skips after the opening bracket): each is closed only at its end, by the
// first level it never closes.
func TestLuaLongStringHoldsAnyText(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ text, level string }{
		{"", ""},
		{"plain", ""},
		{"]]", "="},
		{"x]", "="},
		{"]=]]]", "=="},
		{"x]=", ""},
		{"x]=]", "=="},
		{"\nfirst", ""},
	} {
		got := luaLongString(tc.text)
		assert.Equal(t, "["+tc.level+"[\n"+tc.text+"]"+tc.level+"]", got, "%q", tc.text)
		assert.Equal(t, len(got)-len(tc.level)-2, strings.Index(got, "]"+tc.level+"]"), "%q: closed at its end only",
			tc.text)
	}
}

// TestOfflineOwnershipIsTheLoadsRule returns the missing-ownership issue a load adds: a warning, an error when
// required, none for a complete owner; an invalid manifest fails.
func TestOfflineOwnershipIsTheLoadsRule(t *testing.T) {
	t.Parallel()

	const head = "implements: example.com/c/p.I@v1.0.0\nlanguage: lua\nentrypoint: p.lua\n"

	issues, err := offlineOwnership([]byte(head+"owner: {team: {name: growth}}\n"), false)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "warning", issues[0].Severity)
	assert.Equal(t, CodeOwnershipMissing, issues[0].Code)
	assert.Equal(t, "owner.contact", issues[0].Field)

	issues, err = offlineOwnership([]byte(head), true)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "error", issues[0].Severity)
	assert.Equal(t, "owner", issues[0].Field)

	issues, err = offlineOwnership([]byte(head+"owner: {team: {name: growth}, contact: {email: g@example.com}}\n"),
		true)
	require.NoError(t, err)
	assert.Empty(t, issues)

	_, err = offlineOwnership([]byte("language: lua\n"), false)
	require.ErrorIs(t, err, ErrInvalidManifest)
}
