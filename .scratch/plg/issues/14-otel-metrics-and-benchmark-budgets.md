# 14: OTel metrics and benchmark budgets

**What to build:** Operators can see what plugins are doing, and performance regressions are caught. `WithMeterProvider` (OTel metric API only, no SDK; global provider by default) emits, labelled by plugin key: calls, latency, errors, panics/traps, pool in-use, waits and refills, instance memory, loads and reload failures, HTTP requests, latency and bytes. CI enforces benchmark budgets: Get and All make 0 allocations and Get takes ≤ 50 ns. Plugin call costs per guest are recorded with benchstat but don't fail the build.

**Blocked by:** 07 (Stability), 09 (Host-mediated HTTP)

**Status:** ready-for-agent

- [ ] A test with an in-memory metric reader observes every listed instrument with correct labels
- [ ] CI fails if Get or All allocate or Get exceeds 50 ns/op
- [ ] Call benchmarks per guest are published as CI artifacts in benchstat format
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
