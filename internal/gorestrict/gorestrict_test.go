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
// read; without allowed, imports are not checked.
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
