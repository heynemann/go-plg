package tscheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParse reads the compiler's --pretty false output: located and global diagnostics, the indented lines
// explaining one joined to it, and nothing from other lines.
func TestParse(t *testing.T) {
	t.Parallel()

	out := "Found 3 errors.\n" +
		"  stray explanation before any diagnostic\n" +
		"plugin.ts(2,12): error TS2322: Type '(u: string) => Promise<string>' is not assignable.\n" +
		"  Type 'Promise<string>' is not assignable to type 'Promise<boolean>'.\n" +
		"    Type 'string' is not assignable to type 'boolean'.\n" +
		"   \n" +
		"lib/a b.ts(10,3): error TS2551: Property 'Tierr' does not exist. Did you mean 'Tier'?\n" +
		"error TS5058: The specified path does not exist: 'x'.\n" +
		"plugin.ts(1,1): warning TS1: not an error\n"

	assert.Equal(t, []Diagnostic{
		{
			File: "plugin.ts", Line: 2, Column: 12, Code: "TS2322",
			Message: "Type '(u: string) => Promise<string>' is not assignable. Type 'Promise<string>' is not " +
				"assignable to type 'Promise<boolean>'. Type 'string' is not assignable to type 'boolean'.",
		},
		{File: "lib/a b.ts", Line: 10, Column: 3, Code: "TS2551", Message: "Property 'Tierr' does not exist. Did " +
			"you mean 'Tier'?"},
		{Code: "TS5058", Message: "The specified path does not exist: 'x'."},
	}, parse([]byte(out)))
	assert.Empty(t, parse(nil))
}

func TestDiagnosticString(t *testing.T) {
	t.Parallel()

	located := Diagnostic{File: "plugin.ts", Line: 4, Column: 51, Code: "TS2339", Message: "No Rank."}
	assert.Equal(t, "plugin.ts:4:51: No Rank. (TS2339)", located.String())

	global := Diagnostic{Code: "TS5058", Message: "No such path."}
	assert.Equal(t, "No such path. (TS5058)", global.String())
}
