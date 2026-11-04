# 08: Abandoned multiplexed calls without requests are reclaimed

**Priority:** High · **Difficulty:** Moderate · **Category:** Runtime failure, Allocation hotspot

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** Completing review fixes 05/06: with `WithConcurrentCalls`, a call abandoned at its deadline is reclaimed even when it has no HTTP request in flight. Today the instance owner only learns of an abandonment when resuming one of that call's requests. If call A keeps HTTP busy (keeping the TS guest's request count non-zero) and call B returns a never-settling Promise without making requests, B's lane is released at its deadline but B stays in the host's flight table and the guest's pending-call array forever. Repeated short-deadline B calls grow both without bound and exceed the concurrency limit. Notify the owner when a caller leaves, and reclaim or discard requestless abandoned calls within the total grace from ticket 06 (Lua coroutines included).

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A Lua and a TS integration test (A loops on HTTP, many short-deadline B calls with no requests) reproduces the bug (fails on the current code) and passes with the fix
Flight table and guest pending calls stay bounded by the concurrency limit; abandoned B calls are gone within the grace
Mux unit + integration tests pass 5× under `-race`
- [x] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket

Resolution: a caller that leaves now tells the owner (`left`: an atomic flag and a one-slot wake-up). The owner sweeps when woken and before it starts any call: it abandons each departed call, ending its context and requests and starting its grace. Any abandoned call left with no request in flight or refused is then dropped from the guest at once, within its grace, through the new optional export `plg_drop` (TypeScript: the call's Promise is released). It is then ended and removed from the flight table. A failed drop, or a grace already spent, discards the instance with the call reported. `TestAbandonedCallsAreReclaimedWithoutARequestToResumeThem` (busy call holding a request, 32 stalled calls one after the other, `CallsInFlight` checked at each). TypeScript (a promise nothing settles, no request): 3 calls in flight at the 2nd stall, before the fix → 2 at every stall and 1 after; the guest holds none of the stalls' numbers (`RestartCallNumbers`, 33 calls). Lua calls always wait for a request of their own, which the call's context ends, so standard contexts were not affected. With a caller context whose end never reaches derived contexts, the stalled call and its request stayed in flight (10 s under the default policy timeout; until the 1-minute safety in the test) → reclaimed as the caller leaves. The Lua guest exports no `plg_drop`, having no requestless call to drop. Mux unit tests: three new ones (all failed before) pass 500×, `TestMuxAbandonsACallWhoseCallerLeftFirst` updated (it pinned the old "its request lives on"). The mux unit and integration tests pass 5× under `-race`. Only the TypeScript artifact changed. See PLAN.md.
