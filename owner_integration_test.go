//go:build integration

package plg_test

import (
	"bufio"
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// ownedPlugin writes the language's hostile-corpus plugin into a new plugin directory, key "hostile", of an FS
// storage, with a plugin.yaml holding manifest's keys after the language and entrypoint.
func ownedPlugin(t *testing.T, lang contractLanguage, manifest string) *plg.FSStorage {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "hostile")
	require.NoError(t, os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "hostile", lang.dir, "hostile"))))

	entry := lang.entry
	if lang.lang == plg.LanguageGo {
		entry = "."
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "plugin.yaml"),
		[]byte("language: "+string(lang.lang)+"\nentrypoint: "+entry+"\n"+manifest), 0o600))

	return plg.NewFSStorage(root)
}

// TestOwnerShowsWhereverAPluginFails registers each language's hostile plugin with the owner block of a
// plugin.yaml and makes it fail: the owner is on its PluginError and PanicError, in every log record about it
// (its output's included) as the owner group, and on every one of its metrics as the team, org and company
// attributes only.
func TestOwnerShowsWhereverAPluginFails(t *testing.T) {
	t.Parallel()

	panicking := map[plg.Language]string{plg.LanguageGo: "panic", plg.LanguageLua: "oom", plg.LanguageTypeScript: "oom"}

	for _, lang := range contractLanguages() {
		t.Run(string(lang.lang), func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer

			reader := newMetricsReader(t)
			// slog's JSON handler serializes its writes; the test reads once the container is closed.
			ctr := newContainer(t, plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
				plg.WithMeterProvider(reader.provider))
			require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "owned",
				plg.WithStorage(ownedPlugin(t, lang, fullOwner)), plg.WithPool(1, 1),
				plg.WithMemoryLimit(hostileLimit)))

			owned := getFooer(t, ctr, "owned/hostile")
			want := wantFullOwner()

			pluginErr, ok := errors.AsType[*plg.PluginError](owned.Fail(t.Context(), "owned failure"))
			require.True(t, ok, "a PluginError")
			assert.Equal(t, want, pluginErr.Owner)
			assert.Equal(t, "owned failure", pluginErr.Error(), "the message is unchanged")

			require.Error(t, owned.Fail(t.Context(), "print"), "the plugin writes to stdout, then fails")

			panicErr, ok := errors.AsType[*plg.PanicError](owned.Fail(t.Context(), panicking[lang.lang]))
			require.True(t, ok, "a PanicError")
			assert.Equal(t, want, panicErr.Owner)
			assert.Equal(t, plg.PhaseCall, panicErr.Phase)

			assertOwnedMetrics(t, reader, "owned/hostile", lang.lang) // before Close unregisters the gauges

			require.NoError(t, ctr.Close(t.Context())) // every record is written
			assertOwnedRecords(t, logs.Bytes(), "owned/hostile", "plg: plugin loaded", "plg: guest output",
				"plg: plugin failure")
		})
	}
}

// assertOwnedRecords checks that logs (JSON lines) hold a record with each of the messages about key, and that
// every record about key carries fullOwner's log group.
func assertOwnedRecords(t *testing.T, logs []byte, key string, messages ...string) {
	t.Helper()

	want := map[string]any{
		"team": "payments", "department": "finance", "org": "commerce", "company": "Acme Inc.",
		"maintainer": "Ana Lima", "maintainer_email": "ana@example.com", "maintainer_handle": "@ana",
		"oncall": "payments-oncall", "channel": "#payments", "email": "payments@example.com",
		"repository": "https://git.example.com/payments/rules", "docs": "https://docs.example.com/rules",
		"runbook": "https://runbooks.example.com/rules", "issues": "https://git.example.com/payments/rules/issues",
		"labels": map[string]any{"tier": "1", "cost-center": "42"},
	}
	seen := map[string]bool{}

	lines := bufio.NewScanner(bytes.NewReader(logs))
	lines.Buffer(nil, 1<<20)

	for lines.Scan() {
		var rec map[string]any

		require.NoError(t, sonic.Unmarshal(lines.Bytes(), &rec))

		if rec["key"] != key {
			continue
		}

		msg, ok := rec["msg"].(string)
		require.True(t, ok, "a record's msg is a string")

		seen[msg] = true

		assert.Equal(t, want, rec["owner"], "record %q", msg)
	}

	require.NoError(t, lines.Err())

	for _, msg := range messages {
		assert.True(t, seen[msg], "a %q record about %s", msg, key)
	}
}

// assertOwnedMetrics checks that every data point about key carries fullOwner's team, org and company, and no
// other owner field.
func assertOwnedMetrics(t *testing.T, reader *metricsReader, key string, lang plg.Language) {
	t.Helper()

	owner := [...]attribute.KeyValue{
		attribute.String("team", "payments"), attribute.String("org", "commerce"),
		attribute.String("company", "Acme Inc."),
	}
	points := 0

	for name, m := range reader.collect(t) {
		for _, attrs := range pointAttributes(m) {
			if v, ok := attrs.Value("key"); !ok || v.AsString() != key {
				continue
			}

			points++

			for _, kv := range owner {
				got, ok := attrs.Value(kv.Key)
				assert.True(
					t,
					ok && got == kv.Value,
					"%s %s: %s",
					name,
					attrs.Encoded(attribute.DefaultEncoder()),
					kv.Key,
				)
			}

			for iter := attrs.Iter(); iter.Next(); {
				assert.Contains(t, []attribute.Key{
					"key", "language", "method", "outcome", "phase", "status", "team",
					"org", "company",
				}, iter.Attribute().Key, "%s: no person, address or label", name)
			}
		}
	}

	assert.GreaterOrEqual(t, points, 10, "calls, call errors, durations, panics, loads, gauges")
	assert.Positive(t, reader.collect(t).int64Value(t, "plg.plugin.calls",
		append(owner[:], attribute.String("key", key), attribute.String("language", string(lang)),
			attribute.String("method", "Fail"), attribute.String("outcome", "panic"))...))
}

// pointAttributes returns the attribute sets of m's data points.
func pointAttributes(m metricdata.Metrics) []attribute.Set {
	var sets []attribute.Set

	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range data.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, dp := range data.DataPoints {
			sets = append(sets, dp.Attributes)
		}

	default: // plg records only counters and histograms
	}

	return sets
}

// TestReloadPicksUpAnOwnerChange edits only the owner in a plugin's plugin.yaml, its version unchanged: the reload
// loads the plugin again and its failures carry the new owner. A reload with nothing changed keeps the version
// served, an owner with labels and maintainers included.
func TestReloadPicksUpAnOwnerChange(t *testing.T) {
	t.Parallel()

	store := ownedPlugin(t, luaLanguage(), "version: \"1\"\n"+fullOwner)
	manifest := filepath.Join(store.Name(), "hostile", "plugin.yaml")
	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "owned", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	ownerOf := func() plg.Owner {
		t.Helper()

		pluginErr, ok := errors.AsType[*plg.PluginError](getFooer(t, ctr, "owned/hostile").Fail(t.Context(), "x"))
		require.True(t, ok)

		return pluginErr.Owner
	}

	assert.Equal(t, wantFullOwner(), ownerOf())

	// Count counts the calls of the plugin's one instance: a version loaded again starts from 1.
	count := func() int {
		t.Helper()

		n, err := getFooer(t, ctr, "owned/hostile").Count(t.Context())
		require.NoError(t, err)

		return n
	}

	assert.Equal(t, 1, count())
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "owned"))
	assert.Equal(t, 2, count(), "an unchanged owner is no change")

	require.NoError(t, os.WriteFile(manifest, []byte("language: lua\nentrypoint: plugin.lua\nversion: \"1\"\n"+
		"owner: {team: {name: platform}, org: infra}\n"), 0o600))
	require.NoError(t, plg.ReloadInterface(t.Context(), ctr, "owned"))

	assert.Equal(t, 1, count(), "the plugin was loaded again")
	assert.Equal(t, plg.Owner{Team: plg.Team{Name: "platform"}, Org: "infra"}, ownerOf(), "the new owner")
}
