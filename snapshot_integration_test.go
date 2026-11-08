//go:build integration

package plg_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// Ending Register's context stops copying a custom storage's plugin files at the next read.
func TestCancellingRegisterStopsTheCopyOfPluginFiles(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	root := sparseRoot(t, map[string]int64{"plugin.go": 0})
	files := &growingFS{
		FS: os.DirFS(
			filepath.Join(root, "p"),
		),
		name:    "plugin.go",
		grownTo: plg.MaxPluginFileSize,
		stop:    cancel,
		stopAt:  3,
	}
	store := listStorage{
		name:  "custom",
		files: files,
		descs: []plg.Descriptor{
			{Key: "p", Implements: implementsOf[testcontract.Fooer](), Language: plg.LanguageGo, Entrypoint: "."},
		},
	}

	err := plg.Register[testcontract.Fooer](ctx, newContainer(t), "fooer", plg.WithStorage(store))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "fooer/p")
	assert.Equal(t, 3, files.reads, "no read after the cancellation")
}
