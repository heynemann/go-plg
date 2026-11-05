# 06: The pool never starts an instance after it closed

**Priority:** Medium · **Difficulty:** Easy · **Category:** Concurrency, shutdown leak

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 7.

Inferred from reading the code: reproduce before fixing.

**What to build:** A caller cannot start a new instance after `pool.close` has finished. Today `await` checks `closed` before reserving; a caller paused between the two can reserve and start an instance after a forced close drained the last one and returned with nothing abandoned. `open` then returns `ErrClosed` with the slots left idle and no closer, and with multiplexing the new owner goroutine waits forever. Recheck closure after taking the reservation (before starting), retiring the reservation on rejection, and close anything started regardless.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A synctest test interleaving a caller's reservation with a forced close (via a test hook between check and reserve) reproduces the bug (fails on the current code) and passes with the fix
No instance or owner goroutine survives the close (goleak); budget and pool counters return to zero
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
