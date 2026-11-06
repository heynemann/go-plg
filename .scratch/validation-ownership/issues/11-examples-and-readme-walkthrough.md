# 11: Examples and README walkthrough for validation and ownership

**What to build:** The examples and README tell the end-to-end story for plugin authors, users and administrators: the pricing service's plugins declare ownership and implement `Validate` in Go, Lua and TS; the service registers with validation and `WithRequiredOwnership`, routes events to a log sink, exposes the admin handler, and uses `plg.Check` for an upload-style endpoint. The README gains a section on validating plugins, declaring ownership, and finding who to talk to.

**Blocked by:** 05 (Validate in Lua and TypeScript plugins), 07 (plg.Check dry run), 08 (Lifecycle events: WithOnEvent), 10 (plg.AdminHandler read-only JSON)

**Status:** done

- [x] The examples module's tests exercise validation, a rejected plugin, an event, the admin handler and Check end to end
- [x] The README quick start still passes its extraction test
- [x] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket — CI run 42183634972 for `1e7146d`: success
