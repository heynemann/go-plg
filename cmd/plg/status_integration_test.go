//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// TestStatusAndEventsFromTheCommand runs the plg command against a service's admin handler, behind a bearer token,
// that a profile under $XDG_CONFIG_HOME names, its token in an environment variable: status prints the service's
// plugin as JSON on stdout, and events its events as a table.
func TestStatusAndEventsFromTheCommand(t *testing.T) {
	t.Parallel()

	ctr := plg.New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	store := plg.NewMemoryStorage("store")
	require.NoError(t, store.Put(&plg.Descriptor{
		Key: "ts", Implements: "github.com/heynemann/go-plg/internal/testcontract.Fooer@v1.0.0",
		Language: plg.LanguageTypeScript, Entrypoint: "plugin.ts", Version: "4",
		Owner: &plg.Owner{Team: plg.Team{Name: "search"}},
	}, fstest.MapFS{"plugin.ts": {}}))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "svc", plg.WithStorage(store),
		plg.WithLuaPlugins()), "the plugin is skipped: no guest runs")

	handler := plg.AdminHandler(ctr)
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)

			return
		}

		handler.ServeHTTP(writer, req)
	}))
	t.Cleanup(srv.Close)

	config := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(config, "plg"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(config, "plg", "profiles.yaml"), fmt.Appendf(nil,
		"profiles:\n  - {name: test, url: %q, token: {env: PLG_TEST_TOKEN}}\n", srv.URL), 0o600))

	bin := buildCommand(t)

	environ := append(os.Environ(), "XDG_CONFIG_HOME="+config, "PLG_TEST_TOKEN=s3cret")
	plgCmd := func(args ...string) string {
		t.Helper()

		stdout, stderr, code := runCommand(t, bin, environ, args...)
		require.Equal(t, 0, code, stderr)
		assert.Empty(t, stderr)

		return stdout
	}

	var list adminjson.Plugins

	require.NoError(t, sonic.ConfigDefault.UnmarshalFromString(plgCmd("status", "--format", "json"), &list))
	require.Len(t, list.Plugins, 1)
	assert.Equal(t, "svc/ts", list.Plugins[0].Key)
	assert.Equal(t, string(plg.StatusSkipped), list.Plugins[0].Status)
	assert.Equal(t, "search", list.Plugins[0].Owner.Team.Name)

	lines := strings.Split(strings.TrimSpace(plgCmd("events", "svc/ts")), "\n")
	require.Len(t, lines, 2, "the header and the event")
	assert.Equal(t, []string{"AT", "KIND", "VERSION", "DETAIL"}, strings.Fields(lines[0]))
	assert.Equal(t, []string{"skipped", "4"}, strings.Fields(lines[1])[1:3])
}
