package guestreg_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/guestreg"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestRegisterAndLookup(t *testing.T) {
	t.Parallel()

	_, ok := guestreg.Lookup("guestreg-test-unregistered")
	assert.False(t, ok)

	artifact := []byte("\x00asm")

	guestreg.Register(guestreg.Guest{
		Language: "guestreg-test-lang",
		Wasm:     func() ([]byte, error) { return artifact, nil },
	})

	g, ok := guestreg.Lookup("guestreg-test-lang")
	require.True(t, ok)
	assert.Equal(t, "guestreg-test-lang", g.Language)

	wasm, err := g.Wasm()
	require.NoError(t, err)
	assert.Equal(t, artifact, wasm)
}

func TestRegisterRejectsProgrammingErrors(t *testing.T) {
	t.Parallel()

	wasm := func() ([]byte, error) { return nil, nil }

	guestreg.Register(guestreg.Guest{Language: "guestreg-test-dup", Wasm: wasm})
	assert.PanicsWithValue(t, `guestreg: guest for language "guestreg-test-dup" registered twice`, func() {
		guestreg.Register(guestreg.Guest{Language: "guestreg-test-dup", Wasm: wasm})
	})

	assert.Panics(t, func() { guestreg.Register(guestreg.Guest{Wasm: wasm}) }, "no language")
	assert.Panics(t, func() { guestreg.Register(guestreg.Guest{Language: "guestreg-test-nil"}) }, "no artifact")

	_, ok := guestreg.Lookup("guestreg-test-nil")
	assert.False(t, ok, "a rejected guest is not registered")
}
