package plg_test

import (
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The S3 and Postgres storages and their container-based tests live in examples/, a module of its own, so that a
// service importing plg never inherits their dependencies.
func TestCoreModuleStaysFreeOfExampleDependencies(t *testing.T) {
	t.Parallel()

	examplesOnly := []string{
		"github.com/aws/",            // aws-sdk-go-v2 and smithy-go, the S3 storage
		"github.com/jackc/",          // pgx, the Postgres storage
		"github.com/testcontainers/", // MinIO and Postgres in the examples' tests
	}

	root := os.DirFS(".")

	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := fs.ReadFile(root, name)
		require.NoError(t, err)

		for _, module := range examplesOnly {
			assert.NotContains(t, string(data), module, "%s must not depend on %s", name, module)
		}
	}

	examples, err := fs.ReadFile(root, "examples/go.mod")
	require.NoError(t, err, "examples/ is a module of its own")
	assert.Contains(t, string(examples), "module github.com/heynemann/go-plg/examples\n",
		"a nested module, which the core module's ./... does not build")
	assert.Contains(t, string(examples), "replace github.com/heynemann/go-plg => ../\n",
		"the examples build against this checkout")
}
