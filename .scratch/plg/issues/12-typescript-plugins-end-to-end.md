# 12: TypeScript plugins end to end

**What to build:** TS plugins work like Go plugins:
- The QuickJS-ng guest is built with pinned wasi-sdk, committed and embedded in the TS guest package, with a larger wasm stack so deep recursion becomes an ordinary error.
- esbuild bundles the plugin from its fs.FS on the host.
- `export function Create()` returns an object whose methods are called with `this` bound. Async methods are resolved by running pending jobs until they settle or ctx ends.
- C glue builds JS objects directly from the encoded data.
- Globals: `console` (to slog), `host` (the typed Host interface), `fetch` (through the HTTP policy).
- Only ECMAScript built-ins otherwise; no std/os modules.

**Blocked by:** 03 (Full data-type encoding), 05 (Filesystem and fs.FS storages), 08 (Typed Host interface), 09 (Host-mediated HTTP)

**Status:** ready-for-agent

- [ ] The same contract tests that pass for Go plugins pass for an equivalent TS plugin, including all data types
- [ ] Multi-file TS plugins with relative imports bundle and run
- [ ] Async methods and `await host.X()` / `await fetch()` work; fetch obeys the HTTP policy
- [ ] TS plugins that throw, run out of memory, recurse or loop return errors and the host survives
- [ ] The guest artifact is rebuilt and checked for reproducibility in CI
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
