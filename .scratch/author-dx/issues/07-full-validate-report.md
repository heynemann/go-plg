# 07: Full validate report

**What to build:** `plg validate` runs the whole pipeline in order and reports it: manifest and ownership; contract and pins; language checks (TS type-check, Go plugin restrictions such as `go` statements and imports outside the guest allowlist, Lua syntax); load and `Create`; `Validate` with each example input (from `examples/validation/*.yaml` or the test cases' fixtures); the test cases; and a resource report (peak memory and time per call for each case, against the limits, flagging cases above a configurable share of a limit). Output: coloured terminal summary, `--format json` (stable, documented schema) and `--junit <file>` for CI; exit code non-zero on any error, zero with warnings.

**Blocked by:** 03 (Contract directives), 04 (Test cases), 06 (Typings and type-checking)

**Status:** done

- [x] Tests: each pipeline step's failure surfaces in all three output formats; the JSON schema is pinned by tests on consumer-relevant fields; the resource report reflects a plugin that allocates near its limit
- [x] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket (CI: verified on the PR)
