# 06: Typings and type-checking

**What to build:** TypeScript and Lua authors get editor support from the contract. `plg types` (also run by `plg init`) writes committed TypeScript declarations (the interface, its types with json-tag names, Host, the validation input, `Issue`, the `host`/`fetch`/`console` globals) and Lua Language Server annotations for the same. `plg validate` regenerates them in memory and fails when the committed files are stale (naming the command to run), and type-checks TS plugins against them, reporting errors with file:line:col.

**Blocked by:** 02 (plg validate: resolve the contract and load the plugin offline)

**Status:** done

- [x] Tests: generated declarations for a contract covering every data type (golden files are acceptable here because the output IS the product), staleness detection, a TS type error (wrong result type, unknown Host method) failing validate with position, LuaLS annotations parsing
- [x] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket (CI: run 42472553664 for `00ff89c`: success)
