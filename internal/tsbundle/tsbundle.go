// Package tsbundle bundles a TypeScript (or JavaScript) plugin into the one script the TypeScript guest runs.
// esbuild runs on the host and reads only the plugin's fs.FS: imports resolve to files of the plugin, relative to
// the importing file, never to node_modules, the host's file system or the network.
package tsbundle

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// GlobalName is the global variable the bundle assigns the entrypoint module's exports to; the guest takes
// Create from it.
const GlobalName = "__plg_plugin"

// namespace is the esbuild namespace of the plugin's files, so that esbuild never looks at the host's files.
const namespace = "plg"

var (
	// ErrBundle reports a plugin that could not be bundled: a syntax error, an unresolvable import, a missing
	// entrypoint. Its message lists esbuild's errors with their file:line:column.
	ErrBundle = errors.New("bundling the TypeScript plugin failed")

	errBareImport = errors.New("only relative imports of the plugin's own files are supported " +
		"(no packages, node_modules or URLs)")
	errOutside   = errors.New("the import leaves the plugin's directory")
	errNotFound  = errors.New("no such file in the plugin")
	errExtension = errors.New("files of this type cannot be imported")
)

// Bundle bundles the plugin whose entrypoint file is entrypoint inside files into one script, an immediately
// invoked function assigning the entrypoint's exports to [GlobalName]. Types are stripped, not checked.
func Bundle(files fs.FS, entrypoint string) (string, error) {
	result := api.Build(api.BuildOptions{
		EntryPoints: []string{entrypoint},
		Bundle:      true,
		Write:       false,
		Format:      api.FormatIIFE,
		GlobalName:  GlobalName,
		Platform:    api.PlatformNeutral,
		Target:      api.ES2023,
		Charset:     api.CharsetUTF8,
		LogLevel:    api.LogLevelSilent,
		TsconfigRaw: "{}",
		Plugins:     []api.Plugin{plugin(files)},
	})

	if len(result.Errors) > 0 {
		return "", fmt.Errorf("%w: %s", ErrBundle, describe(result.Errors))
	}

	return string(result.OutputFiles[0].Contents), nil // one entrypoint, written to memory: one output file
}

// describe lists esbuild's errors as file:line:column: text.
func describe(msgs []api.Message) string {
	parts := make([]string, 0, len(msgs))

	for _, msg := range msgs {
		text := msg.Text
		if msg.Location != nil {
			loc := msg.Location
			text = fmt.Sprintf("%s:%d:%d: %s", strings.TrimPrefix(loc.File, namespace+":"), loc.Line, loc.Column,
				text)
		}

		parts = append(parts, text)
	}

	return strings.Join(parts, "; ")
}

// plugin is the esbuild plugin serving every path from files.
func plugin(files fs.FS) api.Plugin {
	return api.Plugin{
		Name: "plg",
		Setup: func(build api.PluginBuild) {
			build.OnResolve(
				api.OnResolveOptions{Filter: ".*"},
				func(args api.OnResolveArgs) (api.OnResolveResult, error) {
					resolved, err := resolve(files, &args)
					if err != nil {
						return api.OnResolveResult{}, err
					}

					return api.OnResolveResult{Path: resolved, Namespace: namespace}, nil
				},
			)
			build.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: namespace},
				func(args api.OnLoadArgs) (api.OnLoadResult, error) {
					return load(files, args.Path)
				})
		},
	}
}

// resolve returns the file an import (or the entrypoint) names, a valid path inside files. Like TypeScript, it
// tries the path as is, then with each script extension, then as a directory with an index file.
func resolve(files fs.FS, args *api.OnResolveArgs) (string, error) {
	name := path.Clean(args.Path)

	if args.Kind != api.ResolveEntryPoint {
		if !strings.HasPrefix(args.Path, "./") && !strings.HasPrefix(args.Path, "../") {
			return "", fmt.Errorf("%q: %w", args.Path, errBareImport)
		}

		name = path.Join(path.Dir(args.Importer), args.Path)
	}

	if !fs.ValidPath(name) {
		return "", fmt.Errorf("%q: %w", args.Path, errOutside)
	}

	// TypeScript sources import each other with .js extensions (moduleResolution nodenext): try the .ts file too.
	tries := []string{name}
	if base, ok := strings.CutSuffix(name, ".js"); ok {
		tries = append(tries, base+".ts", base+".tsx")
	}

	suffixes := [...]string{
		"", ".ts", ".tsx", ".mts", ".js", ".jsx", ".mjs", ".cjs", ".json",
		"/index.ts", "/index.tsx", "/index.js", "/index.jsx", "/index.mjs",
	}

	for _, try := range tries {
		for _, suffix := range suffixes {
			info, err := fs.Stat(files, try+suffix)
			if err == nil && info.Mode().IsRegular() {
				return try + suffix, nil
			}
		}
	}

	return "", fmt.Errorf("%q: %w", args.Path, errNotFound)
}

// load reads a resolved file with the loader of its extension; other files cannot be imported.
func load(files fs.FS, name string) (api.OnLoadResult, error) {
	loader, ok := loaderOf(path.Ext(name))
	if !ok {
		return api.OnLoadResult{}, fmt.Errorf("%q: %w", name, errExtension)
	}

	src, err := fs.ReadFile(files, name)
	if err != nil {
		return api.OnLoadResult{}, fmt.Errorf("reading %q: %w", name, err)
	}

	contents := string(src)

	return api.OnLoadResult{Contents: &contents, Loader: loader}, nil
}

// loaderOf returns the esbuild loader of a file extension.
func loaderOf(ext string) (api.Loader, bool) {
	loaders := [...]struct {
		ext    string
		loader api.Loader
	}{
		{".ts", api.LoaderTS},
		{".mts", api.LoaderTS},
		{".cts", api.LoaderTS},
		{".tsx", api.LoaderTSX},
		{".js", api.LoaderJS},
		{".mjs", api.LoaderJS},
		{".cjs", api.LoaderJS},
		{".jsx", api.LoaderJSX},
		{".json", api.LoaderJSON},
		{".txt", api.LoaderText},
	}

	for _, entry := range loaders {
		if entry.ext == ext {
			return entry.loader, true
		}
	}

	return api.LoaderNone, false
}
