// Package harness is what the rewards platform's end-to-end tests share: the teams' author repositories run
// through their CI (the plg command built from this checkout, the contracts served by an offline GOPROXY), their
// artifacts published to MinIO, the platform started on them with an in-process OpenTelemetry SDK, a fake wallet
// API and a fake vendor API, a user directory, and the oracle computing in Go what a campaign must do.
//
// [NewRig] sets all of it up; the pieces are exported for tests that need another arrangement.
package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/examples/authorrepo"
	"github.com/heynemann/go-plg/internal/contractsrc"
)

// ContractModule is the contracts' module path; v2 and later majors add their major (ContractModuleV2).
const (
	ContractModule   = "example.com/rewards"
	ContractModuleV2 = ContractModule + "/v2"
)

// The contract versions the GOPROXY of [Environ] serves: e2e/contracts is v1.0.0, and the overlays of
// testdata/contracts/<version> make the others (v1.1.0 adds a field; v2.0.0 changes the condition's method, as
// only a new major may).
const (
	ContractV1      = "v1.0.0"
	ContractV1Minor = "v1.1.0"
	ContractV2      = "v2.0.0"
)

// Modes of the files the harness writes.
const (
	dirMode  = 0o750
	fileMode = 0o600
)

// Root returns the directory of the e2e module.
func Root(tb testing.TB) string {
	tb.Helper()

	_, file, _, ok := runtime.Caller(0)
	require.True(tb, ok)

	return filepath.Join(filepath.Dir(file), "..")
}

// Environ returns the environment the teams' commands run in: bin (holding the plg command built from this
// checkout, authorrepo.Command's) first on the PATH, and the go command resolving the contract module at every
// version [Modules] holds through an offline GOPROXY, into a module cache of tb.
func Environ(tb testing.TB, bin string) []string {
	tb.Helper()

	return authorrepo.ProxyEnviron(tb, bin, Modules(tb))
}

// Modules lays out, in a directory of tb, the contract module's versions as a module proxy serves them, each a
// directory <module path>@<version>: example.com/rewards@v1.0.0 (e2e/contracts), @v1.1.0 (v1.0.0 with
// testdata/contracts/v1.1.0 over it) and example.com/rewards/v2@v2.0.0 (with testdata/contracts/v2.0.0 over it,
// its go.mod naming the /v2 module). Each holds the contract's source as its authors would release it, without
// what plg gen generates for the platform (its go.mod requires nothing).
func Modules(tb testing.TB) string {
	tb.Helper()

	modules := tb.TempDir()
	contracts := filepath.Join(Root(tb), "contracts")
	overlays := filepath.Join(Root(tb), "harness", "testdata", "contracts")

	for _, version := range []struct{ module, version, overlay string }{
		{module: ContractModule, version: ContractV1},
		{module: ContractModule, version: ContractV1Minor, overlay: ContractV1Minor},
		{module: ContractModuleV2, version: ContractV2, overlay: ContractV2},
	} {
		dir := filepath.Join(modules, filepath.FromSlash(version.module)+"@"+version.version)
		copySource(tb, contracts, dir)
		require.NoError(tb, os.WriteFile(filepath.Join(dir, "go.mod"),
			[]byte("module "+version.module+"\n\ngo 1.27\n"), fileMode))

		if version.overlay != "" {
			copySource(tb, filepath.Join(overlays, version.overlay), dir)
		}
	}

	return modules
}

// copySource copies the contract's Go source under src into dst, replacing what is there: every .go file but
// tests and plg gen's output, as plg's Go guest reads a contract.
func copySource(tb testing.TB, src, dst string) {
	tb.Helper()

	copyTree(tb, src, dst, func(name string, data []byte) bool {
		return filepath.Ext(name) == ".go" && contractsrc.Include(name, data)
	})
}
