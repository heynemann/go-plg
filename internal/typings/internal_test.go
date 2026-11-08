package typings

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/heynemann/go-plg/internal/tscheck"
)

// TestCreatePosition finds the export of Create in the forms TypeScript and JavaScript write it, in UTF-16 columns
// as the compiler counts them, and falls back to the file's start.
func TestCreatePosition(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		source       string
		line, column int
	}{
		{"export function Create() {}", 1, 17},
		{"// é\n\nexport async function Create() {}", 3, 23},
		{"export function* Create() {}", 1, 18},
		{"const x = '😀'; export const Create = () => 1;", 1, 30},
		{"export let Create = 1", 1, 12},
		{"function Create() {}\nexport { other, Create };", 2, 17},
		{"function Create() {}", 1, 1},
		{"export function CreateMore() {}", 1, 1},
		{"", 1, 1},
	} {
		diag := tscheck.Diagnostic{Line: 9, Column: 9}
		placeAtCreate(&diag, []byte(tc.source))
		assert.Equal(t, []int{tc.line, tc.column}, []int{diag.Line, diag.Column}, tc.source)
	}
}

// TestRelative maps the compiler's file names, relative to the plugin's directory, to the plugin's files, and
// tells the type-check's own files apart.
func TestRelative(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, want string
		plugin     bool
	}{
		{"plugin.ts", "plugin.ts", true},
		{"lib/a.ts", "lib/a.ts", true},
		{"./lib/../b.ts", "b.ts", true},
		{"../plg/check.ts", "/plg/check.ts", false},
		{"", "", true},
	} {
		got, plugin := relative(tc.name)
		assert.Equal(t, tc.want, got, tc.name)
		assert.Equal(t, tc.plugin, plugin, tc.name)
	}
}
