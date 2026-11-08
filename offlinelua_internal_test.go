//go:build integration

package plg

import (
	"log/slog"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/offline"
)

// TestCheckLuaSyntaxCompilesEveryLuaFile compiles a plugin's .lua files in the real Lua guest: each that does not
// compile is named with its line, in name order, whatever brackets the files hold; none runs (a file raising an
// error when run is fine), files of other kinds are not compiled, and a plugin without .lua files has nothing to
// check.
func TestCheckLuaSyntaxCompilesEveryLuaFile(t *testing.T) {
	t.Parallel()

	opts := &offline.Options{Logger: slog.New(slog.DiscardHandler)}

	errs, err := checkLuaSyntax(t.Context(), fstest.MapFS{
		"plugin.lua":     {Data: []byte("error('runs never')\nlocal s = [==[ ]] ]=] ]==]\nfunction Create() end\n")},
		"b/broken.lua":   {Data: []byte("local t = {\n  1,\n  2 ]]\n")},
		"a.lua":          {Data: []byte("\nlocal = 1\n")},
		"ends.lua":       {Data: []byte("return ']]' -- ]=]")},
		"notes.txt":      {Data: []byte("local = not lua")},
		"empty/init.lua": {Data: []byte("")},
	}, opts)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"a.lua:2: <name> expected near '='",
		"b/broken.lua:3: '}' expected (to close '{' at line 1) near ']'",
	}, errs)

	errs, err = checkLuaSyntax(t.Context(), fstest.MapFS{"plugin.lua": {Data: []byte("return {}\n")}}, opts)
	require.NoError(t, err)
	assert.Empty(t, errs)

	errs, err = checkLuaSyntax(t.Context(), fstest.MapFS{"plugin.ts": {Data: []byte("export {}")}}, opts)
	require.NoError(t, err)
	assert.Empty(t, errs, "no .lua file")
}
