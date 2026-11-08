package goguest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNameParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in, want string
	}{
		{
			name: "unnamed method parameters",
			in:   "package p\nfunc (t *T) M(context.Context, int) (int, error) { return 0, nil }\n",
			want: "package p\nfunc (t *T) M(_ context.Context, _ int) (int, error) { return 0, nil }\n",
		},
		{
			name: "function literal and variadic",
			in:   "package p\nvar f = func(string, ...int) {}\n",
			want: "package p\nvar f = func(_ string, _ ...int) {}\n",
		},
		{
			name: "named and empty parameter lists are untouched",
			in:   "package p\nfunc A(x int) {}\nfunc B() {}\nfunc (T) C(_ int) {}\n",
			want: "package p\nfunc A(x int) {}\nfunc B() {}\nfunc (T) C(_ int) {}\n",
		},
		{
			name: "results and interface method specs are untouched",
			in:   "package p\ntype I interface{ M(int) error }\nfunc F(x int) (int, error) { return x, nil }\n",
			want: "package p\ntype I interface{ M(int) error }\nfunc F(x int) (int, error) { return x, nil }\n",
		},
		{
			name: "source that does not parse is left for the interpreter to report",
			in:   "package p\nfunc (int {\n",
			want: "package p\nfunc (int {\n",
		},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.want, string(NameParams([]byte(tc.in))), tc.name)
	}
}

func TestSourceFSRewritesOnlyGoFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	unnamed := "package p\nfunc F(int) {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p.go"), []byte(unnamed), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(unnamed), 0o600))

	got, err := sourceFS{}.ReadFile(filepath.Join(dir, "p.go"))
	require.NoError(t, err)
	assert.Equal(t, "package p\nfunc F(_ int) {}\n", string(got))

	got, err = sourceFS{}.ReadFile(filepath.Join(dir, "notes.txt"))
	require.NoError(t, err)
	assert.Equal(t, unnamed, string(got))

	_, err = sourceFS{}.ReadFile(filepath.Join(dir, "missing.go"))
	require.ErrorIs(t, err, os.ErrNotExist)

	f, err := sourceFS{}.Open(dir)
	require.NoError(t, err)
	assert.NoError(t, f.Close())
}
