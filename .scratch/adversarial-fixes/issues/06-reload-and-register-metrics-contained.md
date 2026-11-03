# 06: Reload and Register metrics are contained like other callbacks

**Priority:** High · **Difficulty:** Moderate · **Category:** Runtime failure, Unfulfilled promise

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** Completing review fix 11: a panicking meter provider cannot crash the process during reload or registration. Today reload metrics are recorded outside the callback guard: if a load counter panics in `reloadOne`, `reloadRound` recovers it and then calls the unguarded `countReloadFailure`, whose second panic escapes the reloader goroutine and kills the process. Load metrics are also recorded before the new version is added to the reload's changes (and analogously at Register), so a panic there leaks an already-loaded version (instances, pool, budget). Route all lifecycle metrics through the panic reporter's callback guard and record them only after cleanup ownership is established. Audit every remaining `c.meters`/instrument call for the same gap.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] Tests with a meter provider that starts panicking after registration, for a reload round and for Register reproduces the bug (fails on the current code) and passes with the fix
The host survives, the reloader keeps running, PhaseCallback entries are recorded, and no version/instance/budget leaks (asserted via test hooks)
An audit list of all instrument call sites is recorded in PLAN.md with each one guarded
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
