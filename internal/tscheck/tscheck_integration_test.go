//go:build integration

package tscheck_test

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/tscheck"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// compile compiles the compiler through the default compilation cache, which `make warm` fills.
func compile(t *testing.T) (*engine.Runtime, *tscheck.Checker) {
	t.Helper()

	eng := engine.New(t.Context(), engine.Options{})
	rt := eng.NewRuntime(t.Context(), engine.RuntimeOptions{MemoryLimit: tscheck.MemoryLimit})

	t.Cleanup(func() {
		assert.NoError(t, rt.Close(context.Background()))
		assert.NoError(t, eng.Close(context.Background()))
	})

	checker, err := tscheck.Compile(t.Context(), rt)
	require.NoError(t, err)

	return rt, checker
}

// TestCheck type-checks small projects with the real compiler: one that type-checks has no diagnostics; one that
// does not gets them with their files, positions and codes; a project the compiler cannot even start on (no
// working directory) fails with ErrCompiler and the compiler's output; a run whose ctx has ended or whose runtime
// is closed fails, and a closed runtime cannot compile the compiler.
func TestCheck(t *testing.T) {
	t.Parallel()

	rt, checker := compile(t)

	project := fstest.MapFS{
		"tsconfig.json": {Data: []byte(`{"compilerOptions": {"strict": true, "noEmit": true, "lib": ["es2023"], ` +
			`"types": []}}`)},
		"good.ts": {Data: []byte("export const n: number = 1;\n")},
	}
	mount := engine.Mount{FS: project, GuestPath: "/p"}

	diags, err := checker.Check(t.Context(), "/p", "/p/tsconfig.json", mount)
	require.NoError(t, err)
	assert.Empty(t, diags)

	project["bad.ts"] = &fstest.MapFile{Data: []byte("export const s: string = 1;\nconsole.log(s);\n")}

	diags, err = checker.Check(t.Context(), "/p", "/p/tsconfig.json", mount)
	require.NoError(t, err)
	assert.Equal(t, []tscheck.Diagnostic{
		{File: "bad.ts", Line: 1, Column: 14, Code: "TS2322", Message: "Type 'number' is not assignable to type " +
			"'string'."},
		{File: "bad.ts", Line: 2, Column: 1, Code: "TS2584", Message: "Cannot find name 'console'. Do you need to " +
			"change your target library? Try changing the 'lib' compiler option to include 'dom'."},
	}, diags, "no console without plg's declarations: the compiler has ECMAScript's library only")

	_, err = checker.Check(t.Context(), "/nowhere", "/p/tsconfig.json", mount)
	require.ErrorIs(t, err, tscheck.ErrCompiler)
	assert.Contains(t, err.Error(), "exit code 3: Error getting current directory")

	ended, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = checker.Check(ended, "/p", "/p/tsconfig.json", mount)
	require.ErrorIs(t, err, context.Canceled)

	require.NoError(t, rt.Close(t.Context()))

	_, err = checker.Check(t.Context(), "/p", "/p/tsconfig.json", mount)
	require.ErrorIs(t, err, engine.ErrStopped)

	_, err = tscheck.Compile(t.Context(), rt)
	require.Error(t, err, "a closed runtime compiles nothing")
}
