# 03: HTTP limits are enforced before a request is copied to the host

**Severity:** high (from the code review of `a1988b6`)

**What to build:** In multiplexed (non-blocking) HTTP, the host copies every started request body into host memory and starts a goroutine for it before any policy check — even when HTTP is disabled. A TS plugin can loop `fetch(u, {method:"POST", body: new Uint8Array(32<<20)})` and grow host memory without bound while guest memory stays flat. The policy's limits must apply before copying: a request over `MaxRequestBytes`, beyond `MaxRequestsPerCall` (per call), or any request when HTTP is disabled is refused inline (answered with the existing failure encoding) without copying its body or spawning a goroutine. Started-request bookkeeping must not keep old bodies reachable after the step (the started slice is resliced without clearing today).

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A regression test (TS plugin issuing many large fetches under `WithConcurrentCalls`) showing bounded host heap reproduces the bug (fails on the current code) and passes with the fix
- [x] Oversized, over-count and HTTP-disabled requests are refused with the same errors as the blocking path, without a body copy or goroutine (assert via allocation/goroutine counts or test hooks)
- [x] Lua's non-blocking `http.request` path gets the same limits
- [x] Old request bodies are not retained after a step
- [x] Blocking-mode HTTP behaviour unchanged (existing HTTP tests pass)
- [x] PLAN.md decision log updated
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
