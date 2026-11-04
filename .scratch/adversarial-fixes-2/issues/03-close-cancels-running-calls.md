# 03: Container.Close stops calls still running

**Priority:** High · **Difficulty:** Moderate · **Category:** Runtime failure, resource retention

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 2 (gap in review fix 01).

Inferred from reading the code: reproduce before fixing.

**What to build:** When `Container.Close` gives up draining and closes runtimes, every call still running on them ends promptly, including calls waiting in host functions. Today a Go plugin called with a long-lived ctx can sit in `time.Sleep(time.Hour)`: the ctx-aware `poll_oneoff` waits only for its timer or the call's own ctx, and closing the module does not cancel that ctx, so the call goroutine and its instance stay until the sleep ends. Host callbacks and HTTP requests have the same gap. Give each call (and each host operation it makes) a context that forced shutdown (and instance discard) can cancel, and make the sleep, HTTP and Host paths watch it.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] Tests where Close's ctx ends while a Go plugin sleeps, while a plugin waits on a slow HTTP server, and while a Host method blocks on its ctx reproduces the bug (fails on the current code) and passes with the fix
Each call returns promptly with a clear closed-container error after Close; Close returns; goleak clean
Calls not affected by Close are unchanged (existing deadline tests pass)
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
