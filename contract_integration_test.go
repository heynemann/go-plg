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
	"github.com/heynemann/go-plg/internal/testcontract"
)

// contractLanguage is a guest language the contract suite holds to the same behaviour (RULES.md §2): the suite's
// tests run once per language against equivalent plugins, testdata/plugins/<dir>/<case>/<entry>, and once per
// binder: the fast paths plg gen generated for internal/testcontract, and plg's generic binder.
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
	// generic serves the contracts with the generic binder instead of the generated fast paths.
	generic bool
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

// eachLanguage runs test as a parallel subtest per contract language and binder.
func eachLanguage(t *testing.T, test func(t *testing.T, lang contractLanguage)) {
	t.Helper()

	for _, lang := range contractLanguages() {
		for _, generic := range []bool{false, true} {
			lang.generic = generic

			t.Run(lang.name(), func(t *testing.T) {
				t.Parallel()

				test(t, lang)
			})
		}
	}
}

// name names the language and its binder.
func (l contractLanguage) name() string {
	if l.generic {
		return string(l.lang) + "/generic"
	}

	return string(l.lang) + "/generated"
}

// binder makes ctr serve the contracts with the language's binder; call it before registering.
func (l contractLanguage) binder(ctr *plg.Container) *plg.Container {
	if l.generic {
		plg.UseGenericBinder(ctr)
	}

	return ctr
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
		require.NoError(tb, store.Put(&desc, l.files(dir)))
	}

	return store
}

// eachBinder runs bench once per binder, as the sub-benchmarks binder=generated and binder=generic, so that
// `benchstat -col /binder` compares the fast paths plg gen generated with the generic binder (ticket 16).
func eachBinder(b *testing.B, lang contractLanguage, bench func(b *testing.B, lang contractLanguage)) {
	b.Helper()

	for _, generic := range []bool{false, true} {
		lang.generic = generic

		name := "binder=generated"
		if generic {
			name = "binder=generic"
		}

		b.Run(name, func(b *testing.B) { bench(b, lang) })
	}
}

// benchCall is one Foo call into lang's fooer plugin, through each binder, with every protection on.
func benchCall(b *testing.B, lang contractLanguage) {
	b.Helper()

	eachBinder(b, lang, func(b *testing.B, lang contractLanguage) {
		b.Helper()

		ref := getFooer(b, fooerContainerIn(b, lang, map[string]string{"ref": "fooer"}), "fooer/ref")
		ctx := b.Context()

		b.ReportAllocs()

		for b.Loop() {
			_, err := ref.Foo(ctx, "bench")
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// benchCallStruct is one Echo call into lang's shaper plugin with SampleOrder as argument and result, through
// each binder: nested slices, maps, pointers, bytes and a time cross both ways.
func benchCallStruct(b *testing.B, lang contractLanguage) {
	b.Helper()

	eachBinder(b, lang, func(b *testing.B, lang contractLanguage) {
		b.Helper()

		shaper, _ := getShaperIn(b, lang)
		ctx := b.Context()
		order := testcontract.SampleOrder()

		b.ReportAllocs()

		for b.Loop() {
			_, err := shaper.Echo(ctx, order)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
