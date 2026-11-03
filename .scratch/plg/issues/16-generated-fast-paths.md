# 16: Generated fast paths

**What to build:** `plg gen` optionally emits encoders and direct call paths specialized to each interface, removing reflection and `MakeFunc` from the hot path. They're kept only where benchmarks show a meaningful gain over the generic binder.

**Blocked by:** 04 (`plg gen`), 14 (OTel metrics and benchmark budgets)

**Status:** done

- [x] Benchmarks compare generic vs generated per guest; the gains are recorded in PLAN.md
- [x] Generated paths pass the full shared contract test suite
- [x] If the gain is insignificant, the ticket closes with the data and no generated paths are shipped
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
