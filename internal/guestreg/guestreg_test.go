package guestreg_test

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/guestreg"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// language returns a language name no other test run has registered: the registry is process-global and
// cannot be reset, and CI runs every test several times in one process (-count=10).
func language() string {
	return "guestreg-test-" + rand.Text()
}

func TestRegisterAndLookup(t *testing.T) {
	t.Parallel()

	lang := language()

	_, ok := guestreg.Lookup(lang)
	assert.False(t, ok)

	artifact := []byte("\x00asm")

	guestreg.Register(guestreg.Guest{
		Language: lang,
		Wasm:     func() ([]byte, error) { return artifact, nil },
	})

	g, ok := guestreg.Lookup(lang)
	require.True(t, ok)
	assert.Equal(t, lang, g.Language)

	wasm, err := g.Wasm()
	require.NoError(t, err)
	assert.Equal(t, artifact, wasm)
}

func TestRegisterRejectsProgrammingErrors(t *testing.T) {
	t.Parallel()

	wasm := func() ([]byte, error) { return nil, nil }
	dup, unregistered := language(), language()

	guestreg.Register(guestreg.Guest{Language: dup, Wasm: wasm})
	assert.PanicsWithValue(t, `guestreg: guest for language "`+dup+`" registered twice`, func() {
		guestreg.Register(guestreg.Guest{Language: dup, Wasm: wasm})
	})

	assert.Panics(t, func() { guestreg.Register(guestreg.Guest{Wasm: wasm}) }, "no language")
	assert.Panics(t, func() { guestreg.Register(guestreg.Guest{Language: unregistered}) }, "no artifact")

	_, ok := guestreg.Lookup(unregistered)
	assert.False(t, ok, "a rejected guest is not registered")
}

func TestAllIsSortedByLanguage(t *testing.T) {
	t.Parallel()

	wasm := func() ([]byte, error) { return nil, nil }
	first, second := language(), language()

	if first > second {
		first, second = second, first
	}

	// Registered out of order; other tests register guests concurrently, so only these two are checked.
	guestreg.Register(guestreg.Guest{Language: second, Wasm: wasm})
	guestreg.Register(guestreg.Guest{Language: first, Wasm: wasm})

	var mine []string

	all := guestreg.All()
	for i, g := range all {
		if i > 0 {
			assert.Less(t, all[i-1].Language, g.Language, "sorted, each language once")
		}

		if g.Language == first || g.Language == second {
			mine = append(mine, g.Language)
		}
	}

	assert.Equal(t, []string{first, second}, mine)
}

func TestInflateRejectsCorruptArtifacts(t *testing.T) {
	t.Parallel()

	var compressed bytes.Buffer

	zw := gzip.NewWriter(&compressed)
	_, err := zw.Write(bytes.Repeat([]byte("wasm"), 1000))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	raw, err := guestreg.Inflate(compressed.Bytes())
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte("wasm"), 1000), raw)

	_, err = guestreg.Inflate([]byte("not gzip"))
	require.Error(t, err, "bad header")

	_, err = guestreg.Inflate(compressed.Bytes()[:compressed.Len()-10])
	require.Error(t, err, "truncated stream")
}
