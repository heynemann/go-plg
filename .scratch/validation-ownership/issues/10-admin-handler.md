# 10: plg.AdminHandler read-only JSON

**What to build:** Dashboards can read the environment over HTTP. `plg.AdminHandler(c) http.Handler` serves the inspection data as read-only JSON (sonic): `GET /plugins` and `GET /plugins/{key}` with the latest reports. It accepts no other method and triggers no plugin code. The docs state it exposes owner contact data and must be mounted behind the service's own auth.

**Blocked by:** 09 (Inspection API: plg.Info and plg.Plugins)

**Status:** ready-for-agent

- [ ] httptest-based tests for both endpoints, unknown keys (404), non-GET methods (405), and that a request runs no plugin code
- [ ] The JSON shape is documented and pinned by tests on fields that matter to consumers (not formatting)
- [ ] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
