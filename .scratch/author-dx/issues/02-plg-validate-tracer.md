# 02: plg validate: resolve the contract and load the plugin offline

**What to build:** An author can run `plg validate <plugin dir>` on their machine before registering anything. The CLI reads `plugin.yaml`, resolves the declared contract module at its pinned version through the local `go` command (`go mod download -json`, honouring GOPROXY/GOPRIVATE/credentials; a clear message when `go` is missing), type-checks the contract with go/types (as `plg gen` does), and renders the contract's adapter/schema in memory. It then checks the manifest and ownership, loads the plugin and runs `Create` in the real guest (all three guests are linked into the CLI) with a stub Host that fails any call clearly, and reports each step in terminal output, exiting non-zero on any error.

The contract-to-runtime machinery is factored so the CLI uses the same validation, schema and load code as a service (no second implementation).

**Blocked by:** 01 (Plugins declare the interface they implement)

**Status:** ready-for-agent

- [ ] Integration tests (a contract module in testdata served through a local GOPROXY/GOFLAGS=-mod=mod or a file-based proxy): a good Go, Lua and TS plugin pass; a bad manifest, an unknown or unfetchable contract, a missing interface, a failing Create and a wrong signature each fail with a precise message and non-zero exit
- [ ] Works offline once the module is in the Go module cache
- [ ] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
