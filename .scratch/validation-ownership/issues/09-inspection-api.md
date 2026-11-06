# 09: Inspection API: plg.Info and plg.Plugins

**What to build:** Admins can see the whole dynamic environment. `plg.Info(c, key)` and `plg.Plugins(c)` describe every plugin a registration's storages hold, including those that never loaded: key, registration, storage, language, owner, the serving version and any rejected newer version, load time, status (`serving`, `degraded`, `serving_last_good` with the rejected version's report, `rejected` with the reason and the owner if the manifest parsed, `retired`), the last load and validation reports, panic counts with the last panic, and pool stats. Data comes from immutable snapshots (no locks on the call path; Get/All unchanged).

**Blocked by:** 03 (Ownership checks and load reports), 06 (On-demand validation)

**Status:** ready-for-agent

- [ ] Tests drive each status through real loads, rejections, on-demand validation and reloads, and check every field
- [ ] Get/All stay at 0 allocations; Plugins is safe under concurrent reloads (-race)
- [ ] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
