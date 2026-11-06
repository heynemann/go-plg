//go:build integration

package plg_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/tsbundle"
)

// exportStarLayers is a TypeScript plugin of n layers of two modules, each re-exporting both modules of the layer
// below with `export *`. esbuild walks every path through the layers for each module: about 2^n steps. Natively,
// 18 layers take 0.6 s and 22 about 11 s; in the bundler, far longer.
func exportStarLayers(n int) fstest.MapFS {
	files := fstest.MapFS{
		"L0a.ts": {Data: []byte("export const a0 = 1;")},
		"L0b.ts": {Data: []byte("export const b0 = 1;")},
	}

	for i := 1; i <= n; i++ {
		for _, side := range []string{"a", "b"} {
			files[fmt.Sprintf("L%d%s.ts", i, side)] = &fstest.MapFile{Data: fmt.Appendf(nil,
				`export * from "./L%da"; export * from "./L%db"; export const %s%d = 1;`, i-1, i-1, side, i)}
		}
	}

	files["plugin.ts"] = &fstest.MapFile{Data: fmt.Appendf(nil,
		"export * from \"./L%da\"; export * from \"./L%db\";\nexport function Create() { return {}; }", n, n)}

	return files
}

// nestedArrows is a one-file TypeScript plugin nesting n conditionals whose branches look like arrow functions
// with a return type: esbuild's TypeScript parser backtracks at every level, about 2^n steps. Natively, 22
// levels take 20 s.
func nestedArrows(n int) fstest.MapFS {
	return fstest.MapFS{"plugin.ts": {Data: []byte("export const x = " + strings.Repeat("a ? (b): c => ", n) +
		"c;\nexport function Create() { return {}; }")}}
}

func TestTypeScriptBundlingStopsAtItsTimeLimit(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)

	// The plugins meant to load do so under the default limits: a small plugin bundles in about 40 ms, but in more
	// than 2 s on a busy CI runner under the race detector. Only the plugins that never bundle in time run under
	// the short one.
	first := tsFooer(t, ctr, `Foo: (s: string) => s`)
	store := tsSource(t, tsPlugin(fooerMembers(`Foo: (s: string) => "v1 " + s`)))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "reloaded", plg.WithStorage(store)))

	plg.SetBundleLimits(ctr, 2*time.Second, plg.BundleMemoryLimit)

	for _, tc := range []struct {
		files fstest.MapFS
		name  string
	}{
		{name: "layers", files: exportStarLayers(24)},
		{name: "arrows", files: nestedArrows(26)},
	} {
		err := plg.Register[testcontract.Fooer](t.Context(), ctr, tc.name, plg.WithStorage(tsSource(t, tc.files)))
		require.ErrorIs(t, err, plg.ErrLoad, tc.name)
		require.ErrorContains(t, err, tc.name+"/p (storage mem): bundling the TypeScript plugin failed: "+
			"bundling took longer than its time limit, 2.0", tc.name)
	}

	// A reload of such a plugin fails the same way, and the version before it serves on.
	require.NoError(t, store.Put(&plg.Descriptor{Key: "p", Language: plg.LanguageTypeScript, Entrypoint: "plugin.ts"},
		exportStarLayers(24)))

	err := plg.ReloadInterface(t.Context(), ctr, "reloaded")
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, "reloading reloaded/p: plg: plugin failed to load: storage mem: "+
		"bundling the TypeScript plugin failed: bundling took longer than its time limit, 2.0")

	out, err := getFooer(t, ctr, "reloaded/p").Foo(t.Context(), "x")
	require.NoError(t, err)
	assert.Equal(t, "v1 x", out)

	out, err = first.Foo(t.Context(), "y")
	require.NoError(t, err)
	assert.Equal(t, "y", out)
	assert.Equal(
		t,
		inspect(t, ctr, "ts/p").Pool.Instances+inspect(t, ctr, "reloaded/p").Pool.Instances,
		plg.Instances(ctr),
		"no bundler instance is left open",
	)
}

func TestTypeScriptBundlingStopsWithItsContext(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	plg.SetBundleLimits(ctr, time.Hour, plg.BundleMemoryLimit)

	tsFooer(t, ctr, `Foo: (s: string) => s`) // the bundler is compiled: what follows runs it

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	err := plg.Register[testcontract.Fooer](ctx, ctr, "layers", plg.WithStorage(tsSource(t, exportStarLayers(40))))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "layers/p (storage mem): bundling the TypeScript plugin: context deadline exceeded")
	assert.Equal(t, inspect(t, ctr, "ts/p").Pool.Instances, plg.Instances(ctr), "the bundler instance is closed")

	// A reload bundling such a plugin stops when the container closes.
	store := tsSource(t, tsPlugin(fooerMembers(`Foo: (s: string) => s`)))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "reloaded", plg.WithStorage(store)))
	require.NoError(t, store.Put(&plg.Descriptor{Key: "p", Language: plg.LanguageTypeScript, Entrypoint: "plugin.ts"},
		nestedArrows(40)))

	waiting, stop := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer stop()

	require.ErrorIs(t, plg.ReloadInterface(waiting, ctr, "reloaded"), context.DeadlineExceeded,
		"the reload is still bundling")
	require.NoError(t, ctr.Close(t.Context()), "closing waits for the reload, whose bundling stops")
	assert.Zero(t, plg.Instances(ctr))
}

func TestTypeScriptBundlingHasAMemoryLimit(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	plg.SetBundleLimits(ctr, time.Minute, 32<<20)

	tsFooer(t, ctr, `Foo: (s: string) => s`) // a small plugin bundles within 32 MiB

	big := fstest.MapFS{
		"plugin.ts": {
			Data: []byte(`import text from "./big.txt"; export function Create() { return { Foo: () => text }; }`),
		},
		"big.txt": {Data: []byte(strings.Repeat("x", 8<<20))},
	}

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "big", plg.WithStorage(tsSource(t, big)))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, "big/p (storage mem): bundling the TypeScript plugin failed: the bundler stopped: "+
		"runtime: out of memory")
	assert.NotContains(t, err.Error(), "wasm stack trace", "the report is the failure, not its trace")
	assert.Equal(t, inspect(t, ctr, "ts/p").Pool.Instances, plg.Instances(ctr), "the bundler instance is closed")
}

func TestTypeScriptBundlingTimeGrowsWithThePlugin(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	plg.SetBundleLimits(ctr, time.Nanosecond, plg.BundleMemoryLimit)

	files := tsPlugin(fooerMembers(`Foo: (s: string) => s`))

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "bare", plg.WithStorage(tsSource(t, files)))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, "bare/p (storage mem): bundling the TypeScript plugin failed: "+
		"bundling took longer than its time limit, 1ms", "a nanosecond and 5 s per MiB of a small plugin")

	files["padding.bin"] = &fstest.MapFile{Data: make([]byte, 1<<20)} // 5 s more, imported or not
	require.NoError(
		t,
		plg.Register[testcontract.Fooer](t.Context(), ctr, "padded", plg.WithStorage(tsSource(t, files))),
	)

	out, err := getFooer(t, ctr, "padded/p").Foo(t.Context(), "x")
	require.NoError(t, err)
	assert.Equal(t, "x", out)
}

// TestTypeScriptBundlesAsEsbuildDoes holds the bundler guest to esbuild run natively: the same bundle of a plugin
// importing TypeScript, JavaScript, JSON and text files from several directories, and the same errors.
func TestTypeScriptBundlesAsEsbuildDoes(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)

	for _, tc := range []struct {
		files fstest.MapFS
		name  string
	}{
		{
			name: "multi-file",
			files: fstest.MapFS{
				"src/index.ts": {Data: []byte(`import { a } from "./a"; import { b } from "./lib/b.js"; ` +
					`import c from "../data.json"; import { d } from "./dir"; import t from "./notes.txt"; ` +
					`export function Create(): string { return a + b + c.c + d + t; }`)},
				"src/a.ts":         {Data: []byte(`export const a: string = "A";`)},
				"src/lib/b.ts":     {Data: []byte(`import { a } from "../a.ts"; export const b = a.toLowerCase();`)},
				"src/dir/index.js": {Data: []byte(`export const d = "D";`)},
				"src/notes.txt":    {Data: []byte(`T`)},
				"data.json":        {Data: []byte(`{"c": "C"}`)},
			},
		},
		{
			name:  "errors",
			files: fstest.MapFS{"src/index.ts": {Data: []byte("import \"./gone\";\nimport \"left-pad\";\nexport {")}},
		},
	} {
		want, wantErr := tsbundle.Bundle(tc.files, "src/index.ts")

		got, err := plg.Bundle(t.Context(), ctr, tc.files, "src/index.ts")
		assert.Equal(t, want, got, tc.name)

		if wantErr == nil {
			require.NoError(t, err, tc.name)
			assert.Contains(t, got, `"T"`, "the text file is in")
		} else {
			require.EqualError(t, err, wantErr.Error(), tc.name)
		}
	}
}
