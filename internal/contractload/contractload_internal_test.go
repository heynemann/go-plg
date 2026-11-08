package contractload

import (
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/goabi"
)

// TestReportsUnreadableContractSource reads contract sources that are gone: the package's files and the
// interpreter's GOPATH built from them fail with the read error, and an interpreter that could not start refuses
// every method with it.
//
//nolint:paralleltest // yaegi patches recursive StructOf types in place, process-wide (see PLAN.md, internal/goguest)
func TestReportsUnreadableContractSource(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")

	_, err := rootSource(missing)
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.ErrorContains(t, err, "reading the contract package: ")

	interpreter := newInterpreter(os.DirFS(missing), "example.com/c")
	require.ErrorIs(t, interpreter.err, ErrInterpret)
	require.ErrorIs(t, interpreter.err, fs.ErrNotExist)

	_, err = interpreter.methods(nil)
	require.ErrorIs(t, err, ErrInterpret)
}

// TestInterpreterRefusesPackagesTheGuestLacks gives the interpreter a method type naming a package the Go guest's
// symbol tables do not have: its import fails, as the Go guest would fail to load such a contract.
//
//nolint:paralleltest // as TestReportsUnreadableContractSource
func TestInterpreterRefusesPackagesTheGuestLacks(t *testing.T) {
	interpreter := newInterpreter(fstest.MapFS{}, "example.com/c")
	require.NoError(t, interpreter.err)

	other := types.NewPackage("example.com/none", "none")
	named := types.NewNamed(types.NewTypeName(token.NoPos, other, "T", nil), types.Typ[types.Int], nil)
	params := types.NewTuple(types.NewVar(token.NoPos, nil, "t", named))

	_, err := interpreter.typeOf("I_M", types.NewSignatureType(nil, nil, nil, params, nil, false))
	require.ErrorContains(t, err, "example.com/none")
	require.Empty(t, interpreter.aliasOf("example.com/unknown"), "a package never named has no alias")
}

// TestDescribeRejectsUnwritableTypes describes the types no contract method may use, with the messages
// reflection gives them: unsafe.Pointer, and an unnamed type of a kind the dispatcher has no name for.
func TestDescribeRejectsUnwritableTypes(t *testing.T) {
	t.Parallel()

	_, err := describe(types.Typ[types.UnsafePointer])
	require.ErrorIs(t, err, goabi.ErrRender)
	require.ErrorContains(t, err, "unsafe.Pointer")

	_, err = describe(types.NewTuple())
	require.ErrorIs(t, err, goabi.ErrRender)
	require.ErrorContains(t, err, "unnamed unsupported type")
}
