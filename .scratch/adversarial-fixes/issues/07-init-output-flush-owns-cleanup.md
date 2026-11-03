# 07: A failed instance start always closes its module

**Priority:** High · **Difficulty:** Moderate · **Category:** Runtime failure, Bug

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** An instance that fails to start is always closed, including when flushing its init output fails. Today, after a successful `Init`, the guest's leftover partial output line is flushed through the user's logger before the instance is handed to the pool; if that log handler panics, the start is reported as failed and the pool place and budget charge are returned, but the wasm module is never closed — repeated registrations accumulate live modules outside memory accounting. A Go plugin whose `Create` does `fmt.Print("x")` triggers it. Guard guest-output logging with the callback boundary and close the instantiated module on every failure before ownership passes to the pool.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A test with a log handler that panics on guest output and a plugin printing a partial line in Create reproduces the bug (fails on the current code) and passes with the fix
The module is closed (instance count/memory back to baseline), budget accounting exact, PhaseCallback recorded
Every early-return path between instantiate and pool hand-off is covered by a test closing the module
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
