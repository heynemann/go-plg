# 06: Abandoned multiplexed calls have a total grace limit

**Severity:** medium (from the code review of `a1988b6`)

**What to build:** An abandoned (deadline-passed) multiplexed call gets one total grace deadline, not a fresh 250 ms per step. Today each resume of an answered call gets a new 250 ms timeout, so a call doing `while(true){ try { await fetch(u) } catch {} }` (each refused request returns at once) lives forever in the instance's flight table, burning the owner goroutine's CPU and logging a warning and metric every loop. Store one grace deadline per call when it is first abandoned/answered, reuse it, and fail (discard) the instance once it is exceeded. Document the total limit in the README (currently says only "250 ms per step").

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A regression test (the review's fetch-in-a-loop plugin abandoned at its deadline) reproduces the bug (fails on the current code) and passes with the fix
- [x] The abandoned call is gone and the instance discarded within the documented total grace; other calls on the instance fail with `ErrInstanceLost`
- [x] Warnings/metrics for the zombie stop after discard
- [x] README limitations updated with the total grace rule
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
