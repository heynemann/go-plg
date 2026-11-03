# 01: Go plugin sleeps respect the call deadline

**Severity:** high (from the code review of `a1988b6`)

**What to build:** A Go plugin that blocks in the Go runtime's scheduler wait (`time.Sleep`, `<-time.After`, timers) is stopped when its call's ctx ends, exactly like a CPU loop is today. Today the guest's sleep is serviced by a host function that sleeps uninterruptibly (wazero's default nanosleep/poll), and close-on-context-done only fires inside compiled wasm, so a 20 s sleep under a 1 s deadline runs the full 20 s (reproduced by the code review). Consequences fixed by this ticket: a sleep in a plugin call overruns its deadline; a sleep in `Create` blocks `Register`; a sleep in a refill-time `Create` blocks pool close, so `Container.Close` hangs whatever ctx it is given.

Approach suggested by the review (pick and document): a per-instance ctx-aware sleep (the current call's or instantiation's ctx is already tracked per instance for host imports) that selects on the timer and ctx.Done, or overriding the WASI poll function with a ctx-receiving implementation. After a deadline-stopped sleep the instance is discarded like any other deadline stop.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] An integration test with a real Go plugin calling `time.Sleep(20*time.Second)` under a short deadline reproduces the bug (fails on the current code) and passes with the fix
- [ ] The call returns within a small bound after its deadline with an error matching `context.DeadlineExceeded`, and the instance is discarded
- [ ] A sleeping `Create` fails `Register` at Register's ctx deadline instead of blocking
- [ ] `Container.Close(ctx)` returns by its ctx even while a refill `Create` is sleeping
- [ ] Plain `time.Sleep` shorter than the deadline still works (sleep is not broken, just bounded)
- [ ] Same behaviour verified for Lua/TS guests if they can reach the same WASI sleep path (or documented why not)
- [ ] PLAN.md decision log updated
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
