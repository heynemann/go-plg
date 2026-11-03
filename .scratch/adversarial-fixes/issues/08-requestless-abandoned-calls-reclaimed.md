# 08: Abandoned multiplexed calls without requests are reclaimed

**Priority:** High · **Difficulty:** Moderate · **Category:** Runtime failure, Allocation hotspot

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** Completing review fixes 05/06: with `WithConcurrentCalls`, a call abandoned at its deadline is reclaimed even when it has no HTTP request in flight. Today the instance owner only learns of an abandonment when resuming one of that call's requests. If call A keeps HTTP busy (keeping the TS guest's request count non-zero) and call B returns a never-settling Promise without making requests, B's lane is released at its deadline but B stays in the host's flight table and the guest's pending-call array forever. Repeated short-deadline B calls grow both without bound and exceed the concurrency limit. Notify the owner when a caller leaves, and reclaim or discard requestless abandoned calls within the total grace from ticket 06 (Lua coroutines included).

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A Lua and a TS integration test (A loops on HTTP, many short-deadline B calls with no requests) reproduces the bug (fails on the current code) and passes with the fix
Flight table and guest pending calls stay bounded by the concurrency limit; abandoned B calls are gone within the grace
Mux unit + integration tests pass 5× under `-race`
- [ ] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
