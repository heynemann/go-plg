# 03: Ownership checks and load reports

**What to build:** Every load produces a report, and missing ownership is flagged. Introduce `plg.Issue{Severity (error | warning | info), Code, Message, Field}` and `plg.Report{Key, Version, Owner, Issues, Err, At, Duration}`. Each load attempt builds a report kept with the version it loaded (or the rejection).

A plugin without `team.name` and at least one of `contact.oncall`, `contact.channel`, `contact.email` gets a warning issue (`plg.ownership.missing`, naming the missing fields) on every load, whether or not validation is configured; warnings are logged with the owner. `plg.WithRequiredOwnership()` (Register option) turns that issue into an error: the load is blocked like any load failure (Register fails, a reload keeps the last good version) and the error names the plugin and missing fields.

**Blocked by:** 02 (Plugin ownership in plugin.yaml, shown in errors, logs and metrics)

**Status:** done

- [x] Tests: warning issue without the option, blocked Register and kept-last-good reload with the option, complete ownership passes cleanly
- [x] The load report is retrievable for tests (export_test hook) until the inspection API exists, and its issues match the log records
- [x] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket — CI run 42167947554 for `331f5e6`: success
