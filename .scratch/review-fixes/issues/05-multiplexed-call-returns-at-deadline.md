# 05: A multiplexed call returns at its own deadline

**Severity:** medium (from the code review of `a1988b6`)

**What to build:** With `WithConcurrentCalls`, each call honours its own ctx: when a call's deadline passes it returns promptly with the ctx error, even while another call's step is running on the same instance. Today, after ctx is done the caller blocks on the instance owner's unbuffered operation channel (or its mailbox) until the owner is free, so a call waiting on HTTP never returns while another call spins — contradicting the README ("each call keeps its own ctx"). The owner must tolerate callers that have gone (e.g. per-call buffered mailbox; late answers discarded), which the resume path already partly supports.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A regression test (call A with a short deadline waiting on HTTP while call B's step spins) reproduces the bug (fails on the current code) and passes with the fix
- [ ] Call A returns within a small bound of its deadline with its ctx error; call B and the instance are unaffected
- [ ] Late answers for a departed call are discarded without leaks (goleak) or panics
- [ ] The mux unit and integration tests pass 5× under `-race`
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
