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

	tests := []struct {
		lang  plg.Language
		entry string
		guest string // the import naming the guest
	}{
		{lang: plg.LanguageGo, entry: "plugin.go", guest: "github.com/heynemann/go-plg/guest/golang"},
		{lang: plg.LanguageTypeScript, entry: "plugin.ts", guest: "github.com/heynemann/go-plg/guest/typescript"},
	}

	ctr := plg.New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	for _, tc := range tests {
		t.Run(string(tc.lang), func(t *testing.T) {
			t.Parallel()

			_, linked := guestreg.Lookup(string(tc.lang))
			require.False(t, linked, "this test binary must not link the %s guest", tc.lang)

			store := plg.NewMemoryStorage("mem")
			files := fstest.MapFS{tc.entry: {Data: []byte("package p\n")}}
			require.NoError(t, store.Put(&plg.Descriptor{
				Key:        "k",
				Implements: "github.com/heynemann/go-plg/internal/testcontract.Fooer@v1.0.0",
				Language:   tc.lang,
				Entrypoint: tc.entry,
			}, files))

			err := plg.Register[testcontract.Fooer](t.Context(), ctr, string(tc.lang), plg.WithStorage(store))
			require.ErrorIs(t, err, plg.ErrGuestNotLinked)
			require.ErrorContains(t, err, `import _ "`+tc.guest+`"`)
			require.ErrorContains(t, err, "plugin k (storage mem)")
		})
	}
}

func TestWarmNeedsALinkedGuest(t *testing.T) {
	t.Parallel()

	err := plg.Warm(t.Context(), plg.WithCompilationCacheDir(t.TempDir()))
	require.ErrorIs(t, err, plg.ErrGuestNotLinked)
}
