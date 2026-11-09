# 05: Author repos: plg.yaml

**What to build:** Teams keep several plugins in one repo. A `plg.yaml` at the repo root lists the plugins (directories), shared directories (copied into each plugin's file tree for validate and pack, so the runtime's one-directory rule stays), limit overrides (on top of the contract's directives), `requireOwnership` (missing ownership is an error instead of a warning), and `replace` entries pointing contract modules at local checkouts. `plg validate <repo>` validates every listed plugin and summarises. For Go plugins, validate fails when the repo's `go.mod` (after replaces) requires a different contract version from `plugin.yaml`'s pin, naming which to change.

**Blocked by:** 02 (plg validate: resolve the contract and load the plugin offline)

**Status:** done

- [x] Tests: repo-mode validation of several plugins in three languages, shared directories importable from TS/Lua/Go plugins, limit overrides applied, `requireOwnership`, a `replace` to a local contract checkout used by validate, and go.mod/plugin.yaml pin mismatches
- [x] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket (CI: run 42460920550 for `863182c`: success)
