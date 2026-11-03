package tsbundle_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/tsbundle"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func file(src string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(src)}
}

func TestBundleResolvesTheEntrypointsImportsInsideThePlugin(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"src/index.ts": file(
			`import { a } from "./a"; import { b } from "./lib/b.js"; import c from "../data.json"; import { d } from "./dir"; import t from "./notes.txt"; export function Create(): string { return a + b + c.c + d + t; }`,
		),
		"src/a.ts":           file(`export const a: string = "A";`),
		"src/lib/b.ts":       file(`import { a } from "../a.ts"; export const b = a.toLowerCase();`),
		"src/dir/index.js":   file(`export const d = "D";`),
		"src/notes.txt":      file(`T`),
		"data.json":          file(`{"c": "C"}`),
		"src/unused.ts":      file(`this is not even TypeScript`),
		"node_modules/x.js":  file(`export const x = 1;`),
		"src/lib/../junk.md": file(``),
	}

	bundle, err := tsbundle.Bundle(files, "src/index.ts")
	require.NoError(t, err)
	assert.Contains(t, bundle, "var "+tsbundle.GlobalName+" = ", "the exports land in the global the guest reads")
	assert.Contains(t, bundle, `"A"`, "an extensionless import resolves to the .ts file")
	assert.Contains(t, bundle, `a.toLowerCase()`, "a .js import resolves to the .ts file, imports nest")
	assert.Contains(t, bundle, `"C"`, "JSON imports, from the parent directory")
	assert.Contains(t, bundle, `"D"`, "a directory import resolves to its index")
	assert.Contains(t, bundle, `"T"`, "text imports")
	assert.NotContains(t, bundle, "this is not even", "unimported files are not read")
	assert.NotContains(t, bundle, ": string", "types are stripped")

	again, err := tsbundle.Bundle(files, "./src/index.ts")
	require.NoError(t, err, "the entrypoint is cleaned")
	assert.Equal(t, bundle, again, "bundling is deterministic")
}

func TestBundleRefusesWhatItCannotBundle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files fs.FS
		entry string
		want  []string
	}{
		{
			name:  "syntax error",
			files: fstest.MapFS{"p.ts": file("export function Create() {\n  return {;\n}")},
			entry: "p.ts",
			want:  []string{"p.ts:2:"},
		},
		{
			name:  "a package import",
			files: fstest.MapFS{"p.ts": file(`import "lodash";`), "node_modules/lodash/index.js": file(``)},
			entry: "p.ts",
			want:  []string{"p.ts:1:", `"lodash": only relative imports of the plugin's own files are supported`},
		},
		{
			name:  "a URL import",
			files: fstest.MapFS{"p.ts": file(`import "https://example.com/x.js";`)},
			entry: "p.ts",
			want:  []string{`"https://example.com/x.js": only relative imports`},
		},
		{
			name:  "an absolute import",
			files: fstest.MapFS{"p.ts": file(`import "/etc/passwd";`)},
			entry: "p.ts",
			want:  []string{`"/etc/passwd": only relative imports`},
		},
		{
			name:  "an import leaving the plugin",
			files: fstest.MapFS{"p.ts": file(`import "../../etc/passwd";`)},
			entry: "p.ts",
			want:  []string{`"../../etc/passwd": the import leaves the plugin's directory`},
		},
		{
			name:  "a missing file",
			files: fstest.MapFS{"p.ts": file(`import "./gone";`)},
			entry: "p.ts",
			want:  []string{`"./gone": no such file in the plugin`},
		},
		{
			name:  "a file type esbuild cannot load",
			files: fstest.MapFS{"p.ts": file(`import "./logo.png";`), "logo.png": file("\x89PNG")},
			entry: "p.ts",
			want:  []string{`"logo.png": files of this type cannot be imported`},
		},
		{
			name:  "a missing entrypoint",
			files: fstest.MapFS{"p.ts": file(``)},
			entry: "main.ts",
			want:  []string{`"main.ts": no such file in the plugin`},
		},
		{
			name:  "an entrypoint outside the plugin",
			files: fstest.MapFS{"p.ts": file(``)},
			entry: "../p.ts",
			want:  []string{`"../p.ts": the import leaves the plugin's directory`},
		},
		{
			name:  "a directory without an index",
			files: fstest.MapFS{"p.ts": file(`import "./dir";`), "dir/a.ts": file(``)},
			entry: "p.ts",
			want:  []string{`"./dir": no such file in the plugin`},
		},
		{
			name:  "unreadable file",
			files: unreadable{fstest.MapFS{"p.ts": file(``)}},
			entry: "p.ts",
			want:  []string{`reading "p.ts": `, errUnreadable.Error()},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := tsbundle.Bundle(tc.files, tc.entry)
			require.ErrorIs(t, err, tsbundle.ErrBundle)

			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

var errUnreadable = errors.New("unreadable")

// unreadable is a file tree whose files can be listed and stat'ed but not read.
type unreadable struct {
	fstest.MapFS
}

func (unreadable) ReadFile(string) ([]byte, error) {
	return nil, errUnreadable
}

func TestBundleListsEveryError(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{"p.ts": file(`import "./a"; import "./b";`)}

	_, err := tsbundle.Bundle(files, "p.ts")
	require.ErrorIs(t, err, tsbundle.ErrBundle)
	assert.Equal(t, 2, strings.Count(err.Error(), "no such file in the plugin"), err.Error())
	assert.Contains(t, err.Error(), "; ", "separated")
}
