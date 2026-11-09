// Package brokenguest_test links a broken Go guest in place of the real one, which no other test binary can
// do: guests register globally, once per program.
package brokenguest_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

var errArtifact = errors.New("artifact unavailable")

// artifactCalls counts the broken guest's artifact requests: odd ones fail, even ones return bytes that are
// not wasm.
var artifactCalls atomic.Int64 //nolint:gochecknoglobals // shared with the guest registered once per binary

func TestMain(m *testing.M) {
	guestreg.Register(guestreg.Guest{Language: string(plg.LanguageGo), Wasm: func() ([]byte, error) {
		if artifactCalls.Add(1)%2 == 1 {
			return nil, errArtifact
		}

		return []byte("not wasm"), nil
	}})

	goleak.VerifyTestMain(m)
}

//nolint:paralleltest // it counts the artifact requests of the one guest, so no other test may run with it
func TestGuestFailuresFailRegisterAndAreRetried(t *testing.T) {
	store := plg.NewMemoryStorage("mem")
	files := fstest.MapFS{"plugin.go": {Data: []byte("package p\n")}}
	require.NoError(t, store.Put(&plg.Descriptor{
		Key: "k", Implements: "github.com/heynemann/go-plg/internal/testcontract.Fooer@v1.0.0",
		Language: plg.LanguageGo, Entrypoint: "plugin.go",
	}, files))

	ctr := plg.New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	before := artifactCalls.Load()

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer", plg.WithStorage(store))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorIs(t, err, errArtifact)

	err = plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer", plg.WithStorage(store))
	require.ErrorIs(t, err, plg.ErrLoad, "the failed load was forgotten: the guest is fetched and compiled again")
	require.ErrorContains(t, err, "compiling guest go")
	assert.Equal(t, before+2, artifactCalls.Load())
}

//nolint:paralleltest // it counts the artifact requests of the one guest, so no other test may run with it
func TestWarmFailsWithTheGuest(t *testing.T) {
	// Two warms meet both failures, in an order that depends on the artifact requests made before.
	odd := artifactCalls.Load()%2 == 0

	for range 2 {
		err := plg.Warm(t.Context(), plg.WithCompilationCacheDir(t.TempDir()))
		require.ErrorContains(t, err, "plg: warm: loading the go guest")

		if odd {
			require.ErrorIs(t, err, errArtifact)
		} else {
			require.ErrorContains(t, err, "compiling guest go")
		}

		odd = !odd
	}
}
