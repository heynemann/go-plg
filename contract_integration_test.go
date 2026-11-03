//go:build integration

package plg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	_ "github.com/heynemann/go-plg/guest/lua"
	_ "github.com/heynemann/go-plg/guest/typescript"
)

// contractLanguage is a guest language the contract suite holds to the same behaviour (RULES.md §2): the suite's
// tests run once per language against equivalent plugins, testdata/plugins/<dir>/<case>/<entry>.
type contractLanguage struct {
	// field names a struct field as the language's conversion errors do: the Go name, or the json name the
	// script guests know.
	field func(goName, jsonName string) string
	lang  plg.Language
	dir   string
	entry string
	// exactInts reports whether the language's integer arithmetic is exact beyond 2^53 (Go's and Lua's 64-bit
	// integers), so a plugin computing such an integer fails to return it instead of rounding it.
	exactInts bool
}

func goName(name, _ string) string { return name }

func jsonName(_, name string) string { return name }

// golang is the contract language of Go plugins.
func golang() contractLanguage {
	return contractLanguage{lang: plg.LanguageGo, dir: "go", entry: "plugin.go", field: goName, exactInts: true}
}

// luaLanguage is the contract language of Lua plugins.
func luaLanguage() contractLanguage {
	return contractLanguage{lang: plg.LanguageLua, dir: "lua", entry: "plugin.lua", field: jsonName, exactInts: true}
}

// typescript is the contract language of TypeScript plugins: its numbers are float64, exact to 2^53 only.
func typescript() contractLanguage {
	return contractLanguage{
		lang: plg.LanguageTypeScript, dir: "typescript", entry: "plugin.ts", field: jsonName, exactInts: false,
	}
}

// contractLanguages returns every language the contract suite runs against.
func contractLanguages() []contractLanguage {
	return []contractLanguage{golang(), luaLanguage(), typescript()}
}

// eachLanguage runs test as a parallel subtest per contract language.
func eachLanguage(t *testing.T, test func(t *testing.T, lang contractLanguage)) {
	t.Helper()

	for _, lang := range contractLanguages() {
		t.Run(string(lang.lang), func(t *testing.T) {
			t.Parallel()

			test(t, lang)
		})
	}
}

// files returns the source directory of the language's test plugin name.
func (l contractLanguage) files(name string) fs.FS {
	return os.DirFS(filepath.Join("testdata", "plugins", l.dir, name))
}

// storage returns a memory storage named name holding the language's test plugins (key -> plugin case).
func (l contractLanguage) storage(tb testing.TB, name string, cases map[string]string) *plg.MemoryStorage {
	tb.Helper()

	store := plg.NewMemoryStorage(name)

	for key, dir := range cases {
		desc := plg.Descriptor{Key: key, Language: l.lang, Entrypoint: l.entry, Version: "v1"}
		require.NoError(tb, store.Put(desc, l.files(dir)))
	}

	return store
}
