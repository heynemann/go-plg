# 04: `plg gen` adapter and contract source generation

**What to build:** Service developers add one `go:generate` line next to their interface (`go run github.com/heynemann/go-plg/cmd/plg gen Fooer`). It generates:
- the adapter type, with an `init()` that registers it with plg;
- a compile-time assertion that the adapter implements the interface, so a changed interface breaks the build until it's regenerated;
- an embed of the contract package's non-test Go source (excluding the generated file) for the guest to interpret.

It refuses contract packages that import anything outside the guest allowlist. The tracer tests switch from hand-written to generated code.

**Blocked by:** 02 (Tracer bullet)

**Status:** done

- [x] Running go generate in a sample contract package produces code that compiles and passes the tracer tests unchanged
- [x] Changing the interface without regenerating fails `go build`
- [x] A contract package importing a disallowed package makes `plg gen` fail with a clear message
- [x] Generated output is deterministic (running twice gives no diff)
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
