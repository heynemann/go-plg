# 15: The coverage gate always checks the profile from the current test run

**Priority:** Low · **Difficulty:** Easy · **Category:** Unfulfilled promise

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** `make check`'s coverage gate cannot pass on a stale or empty profile. Today `coverage` is only a sibling prerequisite of `check`, so under `make -j check` it can check an old passing `coverage.out` before the new test run finishes; the gate script also reports success for an empty or header-only profile. Make `coverage` depend on the test target that produces the profile (same for budgets/allocs outputs), remove stale profiles before tests, and make the gate script fail closed on unusable profiles.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A script test (or Makefile dry-run check) showing an empty/header-only profile reproduces the bug (fails on the current code) and passes with the fix
Gate fails on an empty/header-only profile; `make -n -j check` shows coverage ordered after test
`make check` still passes normally
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
