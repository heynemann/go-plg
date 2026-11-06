# 05: Validate in Lua and TypeScript plugins

**What to build:** Lua and TypeScript plugins validate the same way. Lua: `inst:Validate(v)` returns a list of issue tables (`{severity, code, message, field}`) or `nil, msg`; `error(...)` is a failure. TypeScript: `inst.Validate(v)` returns an array of issues or a Promise of one; a throw is a failure. Both look the method up by name, as `Close` does; `V` crosses through the wire schema like any argument. An unknown severity string counts as error.

**Blocked by:** 04 (Validate at load, for Go plugins)

**Status:** ready-for-agent

- [ ] The shared cross-language contract suite runs the validation cases from ticket 04 against equivalent Go, Lua and TS plugins with identical outcomes
- [ ] TS async `Validate` and a Promise that never settles (stopped by the timeout) are covered
- [ ] Guest artifacts rebuilt with `make guests` and committed; `make verify-guests` clean
- [ ] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
