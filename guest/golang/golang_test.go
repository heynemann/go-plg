package golang_test

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/guest/golang"
	"github.com/heynemann/go-plg/internal/guestreg"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestImportRegistersTheGoGuest(t *testing.T) {
	t.Parallel()

	guest, ok := guestreg.Lookup(golang.Language)
	require.True(t, ok)

	wasm, err := guest.Wasm()
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(wasm, []byte("\x00asm\x01\x00\x00\x00")), "a wasm 1.0 module")

	exports := exportNames(t, wasm)
	for _, name := range []string{"memory", "_initialize", "plg_alloc", "plg_init", "plg_call", "plg_close"} {
		assert.Contains(t, exports, name, "the artifact is a reactor exporting the plg guest ABI")
	}
}

// TestOnlyTheImportLinksTheGuest checks the link graph: the core does not depend on any guest, and even the
// guest package links no interpreter (yaegi lives inside the wasm artifact).
func TestOnlyTheImportLinksTheGuest(t *testing.T) {
	t.Parallel()

	deps := func(pkg string) []string {
		//nolint:gosec // G204: go list on fixed package paths
		out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", pkg).Output()
		require.NoError(t, err)

		return strings.Fields(string(out))
	}

	for _, dep := range deps("github.com/heynemann/go-plg/internal/engine") {
		assert.NotContains(t, dep, "github.com/heynemann/go-plg/guest", "the core links no guest")
		assert.NotContains(t, dep, "goguest")
		assert.NotContains(t, dep, "yaegi")
	}

	for _, dep := range deps("github.com/heynemann/go-plg/guest/golang") {
		assert.NotContains(t, dep, "goguest")
		assert.NotContains(t, dep, "yaegi")
	}
}

var errMalformed = errors.New("malformed wasm")

// exportNames lists a module's export names from its export section (id 7).
func exportNames(t *testing.T, wasm []byte) []string {
	t.Helper()

	reader := bytes.NewReader(wasm[8:])
	for reader.Len() > 0 {
		id, err := reader.ReadByte()
		require.NoError(t, err)

		section := readBytes(t, reader)
		if id == 7 {
			return parseExports(t, bytes.NewReader(section))
		}
	}

	require.FailNow(t, errMalformed.Error(), "no export section")

	return nil
}

func parseExports(t *testing.T, reader *bytes.Reader) []string {
	t.Helper()

	count, err := binary.ReadUvarint(reader)
	require.NoError(t, err)

	names := make([]string, 0, count)

	for range count {
		names = append(names, string(readBytes(t, reader)))

		_, err = reader.ReadByte() // kind
		require.NoError(t, err)

		_, err = binary.ReadUvarint(reader) // index
		require.NoError(t, err)
	}

	return names
}

// readBytes reads a LEB128 length and that many bytes.
func readBytes(t *testing.T, reader *bytes.Reader) []byte {
	t.Helper()

	size, err := binary.ReadUvarint(reader)
	require.NoError(t, err)

	remaining := uint64(reader.Len()) //nolint:gosec // G115: Len is never negative
	require.LessOrEqual(t, size, remaining, errMalformed.Error())

	out := make([]byte, size)
	_, err = reader.Read(out)
	require.NoError(t, err)

	return out
}

func TestInflateRejectsCorruptArtifacts(t *testing.T) {
	t.Parallel()

	var compressed bytes.Buffer

	zw := gzip.NewWriter(&compressed)
	_, err := zw.Write(bytes.Repeat([]byte("wasm"), 1000))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	raw, err := golang.InflateArtifact(compressed.Bytes())
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte("wasm"), 1000), raw)

	_, err = golang.InflateArtifact([]byte("not gzip"))
	require.Error(t, err, "bad header")

	_, err = golang.InflateArtifact(compressed.Bytes()[:compressed.Len()-10])
	require.Error(t, err, "truncated stream")
}
