# 13: Author guide and example plugin repo

**What to build:** A team that has never used plg can build, test, pack and observe a plugin from the docs alone. The README gains an author guide (init, dev, tests, typings, validate in CI, pack, status/events, secrets, tracing) and platform guidance (publishing artifacts to storages, contract design: bounded predicates and pages for large data, idempotency keys and at-least-once delivery for side effects). The examples module gains an author repo for the pricing contract with plg.yaml, three plugins (Go, Lua, TS), tests, committed typings and a CI script running validate and pack; the example service loads the packed artifacts.

**Blocked by:** 08 (plg pack and .plg artifacts), 09 (plg init and plg dev), 10 (Secrets), 11 (Tracing), 12 (Events ring and plg status / plg events)

**Status:** ready-for-agent

- [ ] The examples module's tests run validate and pack on the example repo and load the artifacts in the service
- [ ] The README walkthrough commands are exercised by a test (like the quick start)
- [ ] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
