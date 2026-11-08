# 12: Events ring and plg status / plg events

**What to build:** Authors check their plugins in an environment from a terminal. The container keeps the last 100 events per plugin key (bounded, lock-free reads), served read-only by the admin handler at `GET /plugins/{key}/events`. The CLI gains `plg status [key]` and `plg events <key>`, reading an environment's admin handler through profiles in `~/.config/plg/profiles.yaml` (name, admin URL, token from an env var or a command; `--profile`), with table output by default or `--format json`, and `--team` to filter by owner team.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] Tests: the ring keeps the newest 100 per key under concurrency (-race), the endpoint's JSON and 404/405 behaviour, the CLI against an httptest admin handler (profiles, token sources, table and JSON output, team filter, auth errors)
- [ ] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
