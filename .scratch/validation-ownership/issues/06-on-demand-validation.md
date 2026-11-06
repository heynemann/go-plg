# 06: On-demand validation

**What to build:** Admins and health checks can validate published plugins at any time. `plg.Validate(ctx, c, key) (plg.Report, error)` validates the currently published version in a fresh throwaway instance; `plg.ValidateAll(ctx, c) iter.Seq2[string, plg.Report]` (or equivalent) validates every plugin of every registration with validation configured. The last report is stored with the version.

`WithOnDemandValidationPolicy(plg.Report | plg.Unpublish)` per Register decides what an error-severity result does: `Report` (default) marks the version `degraded` until a later validation passes; `Unpublish` removes it from the catalogue (Get returns `ErrNotFound`; in-flight calls drain as on a reload removal). Metrics count validation runs by outcome and issues by severity, labelled with key and owner labels.

**Blocked by:** 04 (Validate at load, for Go plugins)

**Status:** done

- [x] Tests for both policies: degraded then recovered; unpublished then re-published on a later passing reload
- [x] ValidateAll covers several registrations and skips those without validation
- [x] Metrics for runs and issue counts observed via the SDK ManualReader
- [x] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
