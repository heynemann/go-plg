# 04: Test cases: tests/*.yaml

**What to build:** Authors write language-neutral test cases next to their plugin, and `plg validate` runs them inside the real guest. Each `tests/*.yaml` case has `name`, `call` (a contract method or `Validate`), `input` by parameter name (values may be `{$ref: fixtures/x.yaml}`), `host:` stubs (method plus argument matcher → result or error; optional expected call counts; unmatched Host calls fail the case), `http:` stubs (method plus URL → status, headers, body; unmatched requests fail the case; the HTTP policy is enforced), and `expect:` (result, error message substring, or validation issues). Inputs and results are checked against the contract's types (a typo in a field name is a case error, not a silent zero). Each case runs in a fresh instance with the contract's limits.

**Blocked by:** 02 (plg validate: resolve the contract and load the plugin offline)

**Status:** done

- [x] Tests of the runner itself for Go, Lua and TS plugins: passing and failing results, errors, issues, Host stubs (matching, counts, unmatched), HTTP stubs (matching, unmatched, policy), fixtures with $ref, type errors in inputs and expectations, and a case that panics or times out
- [x] Failure output shows the case, the expected and actual values (diff for structs)
- [x] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket (CI: verified on the PR)
