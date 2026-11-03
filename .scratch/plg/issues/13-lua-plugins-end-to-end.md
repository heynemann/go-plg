# 13: Lua plugins end to end

**What to build:** Lua plugins work like Go plugins:
- C Lua 5.4 is built with pinned wasi-sdk, using wasm exception handling for Lua's error handling (wazero's experimental feature, enabled), committed and embedded in the Lua guest package.
- `function Create()` returns a table whose methods are called as `inst:Method(...)`.
- Errors come from `error(...)` or `return nil, msg`.
- C glue builds Lua tables directly from the encoded data.
- `require` resolves only inside the plugin's mounted directory.
- Libraries: base (without file loading), string, table, math, utf8, coroutine, plus a bundled in-guest `json` module (`json.encode`/`json.decode`) compiled into the Lua artifact (RULES.md §5).
- Globals: `host` and `http.request{...}` (through the HTTP policy).
- Deep recursion and out-of-memory become ordinary Lua errors.

**Blocked by:** 03 (Full data-type encoding), 05 (Filesystem and fs.FS storages), 08 (Typed Host interface), 09 (Host-mediated HTTP)

**Status:** ready-for-agent

- [ ] The shared contract tests pass for an equivalent Lua plugin, including all data types
- [ ] A multi-file Lua plugin using `require` works; requiring outside the plugin directory fails
- [ ] `host` and `http.request` work and obey their policies
- [ ] Lua plugins that error, run out of memory, recurse or loop return errors and the host survives
- [ ] The guest artifact is rebuilt and checked for reproducibility in CI
- [ ] `json.encode`/`json.decode` round-trip nested tables, arrays vs objects, unicode and numbers; malformed JSON returns a Lua error
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
