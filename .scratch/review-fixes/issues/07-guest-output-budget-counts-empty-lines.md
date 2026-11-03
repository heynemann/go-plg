# 07: Empty lines count against the guest output log budget

**Severity:** medium (from the code review of `a1988b6`)

**What to build:** Every guest output line, including empty ones, counts against the per-step log budget, so a plugin cannot flood the host's logs. Today the budget adds only the line's length without its newline, so empty lines cost nothing: `console.log("")` in a loop or `io.write(string.rep("\n", 1e7))` produces tens of millions of log records per step. Count the newline (and/or count records) so the budget bounds both bytes and records.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A regression test emitting millions of empty lines reproduces the bug (fails on the current code) and passes with the fix
- [x] The number of log records per step is bounded by the documented budget, and the truncation is reported once like other budget overruns
- [x] Normal output logging unchanged (existing guestout tests pass)
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
