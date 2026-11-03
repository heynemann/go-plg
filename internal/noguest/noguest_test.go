// Package noguest_test runs plg in a program that does not import the Go guest, which no other test binary
// can do: they all link it.
package noguest_test

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestRegisterNamesTheMissingGuestImport(t *testing.T) {
	t.Parallel()

	_, linked := guestreg.Lookup(string(plg.LanguageGo))
	require.False(t, linked, "this test binary must not link the Go guest")

	store := plg.NewMemoryStorage("mem")
	files := fstest.MapFS{"plugin.go": {Data: []byte("package p\n")}}
	require.NoError(t, store.Put(plg.Descriptor{Key: "k", Language: plg.LanguageGo, Entrypoint: "plugin.go"}, files))

	ctr := plg.New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer", plg.WithStorage(store))
	require.ErrorIs(t, err, plg.ErrGuestNotLinked)
	require.ErrorContains(t, err, `import _ "github.com/heynemann/go-plg/guest/golang"`)
	require.ErrorContains(t, err, "plugin k (storage mem)")
}
