# 09: plg init and plg dev

**What to build:** Authors start and iterate quickly. `plg init --implements <interface@version> --lang <go|lua|ts> [dir]` creates a plugin directory (plugin.yaml with implements and an owner block to fill in, typings, a skeleton that compiles and implements every method with a TODO-free minimal body, example tests that pass, a Validate when the contract has validation) and adds it to the repo's plg.yaml. `plg dev [plugin]` watches the plugin, shared directories and tests, re-runs validate incrementally with warm guests on each save, and offers an interactive prompt to call one method with an input file.

**Blocked by:** 04 (Test cases), 05 (Author repos: plg.yaml), 06 (Typings and type-checking)

**Status:** done

- [x] Tests: init for each language produces a plugin that passes validate unchanged; dev re-runs on a file change (deterministic, driven by an injected watcher) and the interactive call prints the result
- [x] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket (CI: verified on the PR)
