package goallow_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/goallow"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestGuestSymbolsMatchTheAllowlist checks the committed guest symbol tables against the list: the guest links
// exactly the allowlisted packages, so a list edited without `make symbols guests` fails here.
func TestGuestSymbolsMatchTheAllowlist(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join("..", "..", "guestsrc", "golang", "symbols", "*.go"))
	require.NoError(t, err)

	var extracted []string

	for _, file := range files {
		name := filepath.Base(file)
		if name == "symbols.go" { // the hand-written registry
			continue
		}

		extracted = append(extracted, strings.ReplaceAll(strings.TrimSuffix(name, ".go"), "-", "/"))
	}

	want := slices.DeleteFunc(goallow.Packages(), func(pkg string) bool { return pkg == "cmp" })
	slices.Sort(want)
	slices.Sort(extracted)
	assert.Equal(t, want, extracted)
}

func TestPackages(t *testing.T) {
	t.Parallel()

	pkgs := goallow.Packages()
	assert.Equal(t, "bufio", pkgs[0], "the header comment is not a package")
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(pkgs))), len(pkgs), "no duplicates")

	for _, pkg := range pkgs {
		assert.Equal(t, strings.TrimSpace(pkg), pkg)
		assert.NotContains(t, pkg, "#")
	}
}

func TestAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want bool
	}{
		{path: "fmt", want: true},
		{path: "context", want: true},
		{path: "math/rand/v2", want: true},
		{path: "cmp", want: true},
		{path: "unicode/utf8", want: true},
		{path: "", want: false},
		{path: "os", want: false},
		{path: "unsafe", want: false},
		{path: "reflect", want: false},
		{path: "math/rand/v3", want: false},
		{path: "fmt/", want: false},
		{path: "github.com/heynemann/go-plg", want: false},
		{
			path: "# cmp has no symbol table: yaegi compiles it from source, for its generic maps and slices.",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, goallow.Allowed(tc.path))
		})
	}
}
