package gorestrict_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/gorestrict"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func src(body string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("package p\n\n" + body)}
}

// TestCheckFindsGoStatementsAndImportsTheGuestCannotProvide checks plugin sources: every go statement and every
// import allowed refuses is named by position, both kinds at once; tests, links and other directories are not
// read, the files the Go guest does not load (see TestGuestLoadsWhatYaegiLoads) are not checked, and those it loads
// although go build would not are; without allowed, imports are not checked.
func TestCheckFindsGoStatementsAndImportsTheGuestCannotProvide(t *testing.T) {
	t.Parallel()

	allowed := func(imp string) bool { return imp == "context" || imp == "example.com/vendored" }
	clean := src("import (\n\t\"context\"\n\t_ \"example.com/vendored\"\n)\n\nfunc f(context.Context) { defer g() }\n")
	both := src("import (\n\t\"os\"\n\tx `example.com/other`\n)\n\nfunc f() {\n\tgo g(os.Args, x.Y)\n}\n")

	for _, tc := range []struct {
		files   fs.FS
		allowed func(string) bool
		name    string
		dir     string
		msg     string
		want    []error
	}{
		{name: "clean", files: fstest.MapFS{"p/a.go": clean, "p/vendor/v/b.go": clean}, dir: "p", allowed: allowed},
		{
			name: "go statements and imports, in every file below dir", dir: "p", allowed: allowed,
			files: fstest.MapFS{"p/a.go": both, "p/vendor/v/b.go": src("import \"net\"\n")},
			want:  []error{gorestrict.ErrGoStatement, gorestrict.ErrImport},
			msg: "plg: go statements are not allowed in plugins: p/a.go:9:2\n" +
				"plg: the plugin imports packages the Go guest cannot provide: p/a.go:4:2: \"os\", " +
				"p/a.go:5:4: `example.com/other`, p/vendor/v/b.go:3:8: \"net\"",
		},
		{
			name: "imports unchecked without allowed", dir: "p", files: fstest.MapFS{"p/a.go": both},
			want: []error{gorestrict.ErrGoStatement}, msg: "plg: go statements are not allowed in plugins: p/a.go:9:2",
		},
		{
			name: "tests, links and other files are not read", dir: ".", allowed: allowed,
			files: fstest.MapFS{
				"a_test.go": both, "link.go": {Mode: fs.ModeSymlink, Data: []byte("x")}, "n.txt": {Data: []byte("go")},
			},
		},
		{name: "only below dir", dir: "p", allowed: allowed, files: fstest.MapFS{"p/a.go": clean, "q/b.go": both}},
		{
			name: "files the guest skips are not checked", dir: "p", allowed: allowed,
			files: fstest.MapFS{
				"p/a.go": clean, "p/_old.go": both, "p/.x.go": both, "p/x_windows.go": both, "p/x_linux_amd64.go": both,
				"p/_broken.go": {Data: []byte("package p\nfunc {")},
				"p/gen.go": {Data: []byte("//go:build ignore\n// +build ignore\n\npackage main\n\nimport \"os\"\n\n" +
					"func main() {\n\tgo os.Exit(0)\n}\n")},
				"p/vendor/v/v_windows.go": both,
			},
		},
		{
			name: "files the guest loads although go build would not", dir: "p", allowed: allowed,
			files: fstest.MapFS{"p/x_riscv64.go": both, "p/x_hurd.go": src("import \"os\"\n")},
			want:  []error{gorestrict.ErrGoStatement, gorestrict.ErrImport},
			msg: "plg: go statements are not allowed in plugins: p/x_riscv64.go:9:2\n" +
				"plg: the plugin imports packages the Go guest cannot provide: p/x_hurd.go:3:8: \"os\", " +
				"p/x_riscv64.go:4:2: \"os\", p/x_riscv64.go:5:4: `example.com/other`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := gorestrict.Check(tc.files, tc.dir, tc.allowed)
			if tc.want == nil {
				require.NoError(t, err)

				return
			}

			for _, want := range tc.want {
				require.ErrorIs(t, err, want)
			}

			assert.EqualError(t, err, tc.msg)
		})
	}
}

func TestCheckFailsOnSourceItCannotRead(t *testing.T) {
	t.Parallel()

	err := gorestrict.Check(fstest.MapFS{"a.go": {Data: []byte("package p\nfunc {")}}, ".", nil)

	var syntax *gorestrict.SyntaxError

	require.ErrorAs(t, err, &syntax)
	require.ErrorContains(t, err, "a.go:2:6: expected 'IDENT', found '{'")
	assert.Equal(t, syntax.Err, syntax.Unwrap())

	err = gorestrict.Check(fstest.MapFS{"a.go": {Data: []byte("package p\n")}}, "missing", nil)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestRefusedListsImportsAsPlgGenDoes lists a file's refused imports with their positions, each path as written
// (escapes judged by what they spell).
func TestRefusedListsImportsAsPlgGenDoes(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "c.go", "package c\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"\\x6fs\"\n)\n",
		parser.ImportsOnly)
	require.NoError(t, err)

	assert.Equal(t, []string{`c.go:5:2: "os"`, `c.go:6:2: "\x6fs"`},
		gorestrict.Refused(fset, file, func(imp string) bool { return imp == "fmt" }))
	assert.Empty(t, gorestrict.Refused(fset, file, func(string) bool { return true }))
}

// TestGuestLoadsWhatYaegiLoads selects files as yaegi v0.16.1's skipFile and buildOk (interp/build.go) do on
// wasip1/wasm: by the name's prefix and by the operating systems and architectures yaegi knows (not go/build's:
// x_riscv64.go and x_hurd.go load), then by the `// +build` lines before the package clause, never `//go:build`.
func TestGuestLoadsWhatYaegiLoads(t *testing.T) {
	t.Parallel()

	for name, loads := range map[string]bool{
		"a.go": true, "windows.go": true, "a_wasip1.go": true, "a_wasm.go": true, "a_wasip1_wasm.go": true,
		"a_riscv64.go": true, "a_hurd.go": true, "a_zos.go": true, "a_linux_riscv64.go": true, "a_hurd_amd64.go": false,
		"a_wasip1_riscv64.go": true, "a_y_windows.go": true, "a_y_wasm.go": true, "a_linux_y.go": true,
		"p/a_windows.go": false, "a_amd64.go": false, "a_linux_amd64.go": false, "a_wasip1_arm64.go": false,
		"a_y_amd64.go": false, "a_linux_wasm.go": false, "_old.go": false, ".x.go": false, "p/_old.go": false,
		"a_test.go": false, "a_wasip1_test.go": false, "a.txt": false, ".go": false,
	} {
		got, err := gorestrict.GuestLoads(token.NewFileSet(), name, []byte("package p\n"))
		require.NoError(t, err, name)
		assert.Equal(t, loads, got, name)
	}

	for header, loads := range map[string]bool{
		"":                                      true,
		"// +build wasip1,wasm\n":               true,
		"// +build linux wasip1\n":              true,
		"// +build go1.21\n":                    true,
		"// +build !go1.999\n":                  true,
		"// +build !\n":                         true,
		"// +build linux\n":                     false,
		"// +build ignore\n":                    false,
		"// +build !wasip1\n":                   false,
		"// +build wasm,linux\n":                false,
		"// +build go1.999\n":                   false,
		"// +build go1.x go1.\n":                false,
		"// +build wasm,\n":                     false,
		"// +build wasm\n// +build linux\n":     false,
		"// +build ignore\n\n// Package p.\n":   false,
		"/*\n+build ignore\n*/\n":               false,
		"//go:build ignore\n":                   true,
		"// Package p is not +build ignore.\n":  true,
		"//go:build ignore\n// +build ignore\n": false,
	} {
		got, err := gorestrict.GuestLoads(token.NewFileSet(), "a.go", []byte(header+"\npackage p\n\nfunc {"))
		require.NoError(t, err, header)
		assert.Equal(t, loads, got, header)
	}

	_, err := gorestrict.GuestLoads(token.NewFileSet(), "p/a.go", []byte("// +build ignore\n\npackag main\n"))
	require.EqualError(t, err, "p/a.go:3:1: expected 'package', found packag",
		"yaegi fails the load on a package clause that does not parse, whatever the constraints")
}
