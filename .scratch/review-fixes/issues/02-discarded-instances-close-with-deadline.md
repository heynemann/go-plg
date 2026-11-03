# 02: Discarded instances never run plugin Close without a deadline

**Severity:** high (from the code review of `a1988b6`)

**What to build:** Whenever an instance is thrown away, the plugin's own `Close()` either runs under a bounded deadline or is skipped, never under a context that cannot end. Today several discard paths close with a context that never ends: a multiplexed call failing alone (out of memory reported as that call's panic while the step itself succeeded) marks only the multiplexer broken, not the engine instance, so `plg_close` still runs; warm-up failures close already-started instances the same way; and the binder's discard after a host-side decode failure or recovered panic does too. Reproduced by the review: a TS plugin with `WithPool(1,1)`, `WithConcurrentCalls(2)` and `Close(){for(;;){}}` — after `Fail("oom")`, the next call with a 2 s deadline was still blocked after 15 s, losing the caller goroutine, a CPU core, the pool slot and its memory-budget charge.

Rule to implement everywhere: an instance discarded because it is broken skips the plugin's Close (like `closeAbandoned`); a healthy instance being retired runs Close under the existing bounded close timeout.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A regression test (the review's TS `Close(){for(;;){}}` + OOM-in-multiplexed-call scenario) reproduces the bug (fails on the current code) and passes with the fix
- [x] Every discard/close path (multiplexed lane failure, warm-up failure, binder discard, pool discard, retire) is covered by a test showing Close is skipped or bounded
- [x] The pool slot and memory-budget reservation are returned after such a discard (asserted via the test hooks)
- [x] Warm-up failure with a hanging Close cannot make `Register` hang
- [x] PLAN.md decision log updated
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
