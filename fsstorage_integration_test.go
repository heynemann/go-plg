//go:build integration

package plg_test

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// loggedContainer returns a container on the default compilation cache whose logs go to logs, as JSON lines.
func loggedContainer(t *testing.T, logs *bytes.Buffer) *plg.Container {
	t.Helper()

	ctr := plg.New(plg.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	return ctr
}

func TestFSStoragesLoadThroughRegister(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)

	// A real directory (with a multi-file plugin and a Lua plugin) and an embed.FS, in one Register.
	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer",
		plg.WithStorage(plg.NewFSStorage(diskDir)), plg.WithStorage(embeddedStorage(t, "embedded", "embedded")),
		plg.WithGolangPlugins())
	require.NoError(t, err)

	tests := []struct {
		key  string
		want string
	}{
		{key: "fooer/multifile", want: "hello, x (multifile)"}, // Create and Foo use code from other files
		{key: "fooer/other", want: "hello, x (embedded)"},
		{key: "fooer/single", want: "hello, x (single)"},
	}

	var keys []string

	for key := range plg.All[testcontract.Fooer](t.Context(), ctr) {
		keys = append(keys, key)
	}

	require.Equal(t, []string{tests[0].key, tests[1].key, tests[2].key}, keys)

	for _, tc := range tests {
		got, callErr := getFooer(t, ctr, tc.key).Foo(t.Context(), "x")
		require.NoError(t, callErr)
		assert.Equal(t, tc.want, got)
	}

	_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, "fooer/scripted")
	require.ErrorIs(t, err, plg.ErrNotFound, "the Lua plugin is outside the allowlist")
	assert.Equal(t, []skipRecord{{
		Level: "INFO", Message: "plg: plugin skipped: its language is not enabled", Key: "fooer/scripted",
		Storage: diskDir, Language: "lua", Implements: implementsOf[testcontract.Fooer](),
	}}, decodeRecords(t, &logs), "and its skip is logged")
}

func TestEnabledLuaPluginsLoadFromAStorage(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)

	for name, opts := range map[string][]plg.RegisterOption{
		"every language": nil,
		"golang and lua": {plg.WithGolangPlugins(), plg.WithLuaPlugins()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := plg.Register[testcontract.Fooer](t.Context(), ctr, name,
				append(opts, plg.WithStorage(plg.NewFSStorage(diskDir)))...)
			require.NoError(t, err)

			got, err := getFooer(t, ctr, name+"/scripted").Foo(t.Context(), "disk")
			require.NoError(t, err)
			assert.Equal(t, "hello, disk", got, "the Lua plugin next to the Go ones")

			got, err = getFooer(t, ctr, name+"/single").Foo(t.Context(), "disk")
			require.NoError(t, err)
			assert.Equal(t, "hello, disk (single)", got)
		})
	}
}

// loadRecord is the part of a log record a loaded plugin is reported with.
type loadRecord struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

func TestRegisterHashesVersionlessPlugins(t *testing.T) {
	t.Parallel()

	disk := plg.NewFSStorage(diskDir)

	listed, err := disk.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, "multifile", listed[0].Key)
	require.Empty(t, listed[0].Version, "List leaves it to Register")

	// The same files in a memory storage, without a version: Register hashes them as it hashes the FS
	// storage's.
	store := plg.NewMemoryStorage("mem")
	require.NoError(t, store.Put(&plg.Descriptor{
		Key: "multifile", Implements: implementsOf[testcontract.Fooer](), Language: plg.LanguageGo, Entrypoint: ".",
	}, os.DirFS(filepath.Join(diskDir, "multifile"))))

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "mem", plg.WithStorage(store)))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "disk", plg.WithStorage(disk),
		plg.WithGolangPlugins()))

	versions := map[string]string{}

	lines := bufio.NewScanner(&logs)
	for lines.Scan() {
		var rec loadRecord

		require.NoError(t, sonic.Unmarshal(lines.Bytes(), &rec))

		if rec.Key == "mem/multifile" || rec.Key == "disk/multifile" {
			versions[rec.Key] = rec.Version
		}
	}

	require.NoError(t, lines.Err())
	require.Regexp(t, isContentHash, versions["mem/multifile"])
	assert.Equal(t, versions["mem/multifile"], versions["disk/multifile"])
	assert.Equal(t, versions["disk/multifile"], inspect(t, ctr, "disk/multifile").Version)
}
