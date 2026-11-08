package plg_test

import (
	"context"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
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
		Manifest: []byte("implements: github.com/heynemann/go-plg_test.offlineIface@v1.0.0\n" +
			"language: lua\nentrypoint: plugin.lua\n"),
	}
	opts := &offline.Options{}

	_, err := offline.Check(t.Context(), spec, &offline.Plugin{Key: "p", Manifest: []byte("language: lua\n")}, opts)
	require.ErrorIs(t, err, plg.ErrInvalidManifest)

	empty := *spec
	empty.Methods = nil
	_, err = offline.Check(t.Context(), &empty, plugin, opts)
	require.ErrorIs(t, err, plg.ErrInvalidInterface)
	require.ErrorContains(t, err, "plg_test.offlineIface: declares no methods")

	sourceless := *spec
	sourceless.Source = nil
	_, err = offline.Check(t.Context(), &sourceless, plugin, opts)
	require.ErrorIs(t, err, plg.ErrInvalidContract)
	require.ErrorContains(t, err, "nil Source")

	other := *spec.Interface.Type
	other.Path = "example.com/other"
	hosted := *spec
	hosted.Host = &offline.Host{
		Interface: &goabi.Interface{Type: &other, Methods: spec.Interface.Methods}, Methods: spec.Methods,
	}
	_, err = offline.Check(t.Context(), &hosted, plugin, opts)
	require.ErrorIs(t, err, plg.ErrInvalidHost)
	require.ErrorContains(t, err, "must be declared in the contract package")
}

// TestCheckOfflineRunsUnderTheLimitsAsRegisterOptions: the limits and guest plg validate passes are Register's
// options, refused as Register refuses them, before any guest runs.
func TestCheckOfflineRunsUnderTheLimitsAsRegisterOptions(t *testing.T) {
	t.Parallel()

	spec := offlineSpec(t, reflect.TypeFor[offlineIface](), fstest.MapFS{"c.go": {Data: []byte("package c\n")}})
	plugin := &offline.Plugin{
		Key: "p", Files: fstest.MapFS{},
		Manifest: []byte("implements: github.com/heynemann/go-plg_test.offlineIface@v1.0.0\n" +
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
		require.ErrorIs(t, err, plg.ErrInvalidOption, tc.msg)
		require.ErrorContains(t, err, tc.msg)
	}
}
