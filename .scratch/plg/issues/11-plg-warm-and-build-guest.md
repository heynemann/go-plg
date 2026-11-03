# 11: `plg warm` and `plg build-guest`

**What to build:** Two operator commands in the plg CLI:
- `plg warm` compiles every linked guest into the compilation cache. Run it in a Docker build so containers start warm.
- `plg build-guest -pkg <import path> ... -o guest.wasm` builds a custom Go guest with extra packages (stdlib or pure-Go third-party). The service passes it in with `WithGoGuest(embedded)`, and plugins can import those packages.

**Blocked by:** 04 (`plg gen`)

**Status:** ready-for-agent

- [ ] After `plg warm` into a directory, a fresh process with that cache dir loads the Go guest without a cold compile
- [ ] A custom guest including encoding/xml lets a Go plugin import it, while the default guest rejects that import
- [ ] WithGoGuest guests share the same validation, limits and stability behavior as the default guest
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
