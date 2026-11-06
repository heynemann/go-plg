# 07: plg.Check dry run

**What to build:** A candidate plugin can be checked before it's enabled. `plg.Check(ctx, c, name, plg.Descriptor, fs.FS) (plg.Report, error)` loads the candidate against the existing registration `name` (its contract, Host, HTTP policy, limits, ownership rules and validation), runs Create and Validate in a throwaway instance charged to the budget, and returns the full report; nothing is published, no catalogue entry or event is created, and the candidate's files go through the same snapshot limits as a storage's.

**Blocked by:** 04 (Validate at load, for Go plugins)

**Status:** done

- [x] Tests: a good candidate passes, a broken one (load error, validation error, missing ownership with `WithRequiredOwnership`) reports why, the catalogue and events are untouched, an unknown registration name is an error
- [x] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket — CI run 42172681687 for `4b3d18e`: success
