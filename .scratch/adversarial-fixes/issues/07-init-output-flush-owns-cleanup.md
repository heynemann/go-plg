# 07: A failed instance start always closes its module

**Priority:** High · **Difficulty:** Moderate · **Category:** Runtime failure, Bug

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** An instance that fails to start is always closed, including when flushing its init output fails. Today, after a successful `Init`, the guest's leftover partial output line is flushed through the user's logger before the instance is handed to the pool; if that log handler panics, the start is reported as failed and the pool place and budget charge are returned, but the wasm module is never closed — repeated registrations accumulate live modules outside memory accounting. A Go plugin whose `Create` does `fmt.Print("x")` triggers it. Guard guest-output logging with the callback boundary and close the instantiated module on every failure before ownership passes to the pool.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A test with a log handler that panics on guest output and a plugin printing a partial line in Create reproduces the bug (fails on the current code) and passes with the fix
The module is closed (instance count/memory back to baseline), budget accounting exact, PhaseCallback recorded
Every early-return path between instantiate and pool hand-off is covered by a test closing the module
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket

Resolution: instances log their output through the user's handler behind the callback boundary (`panicReporter.guardedLogger`), so a handler panicking on guest output is a `PhaseCallback` entry and the step (Create, a call) carries on; and `launcher.start` closes the instance itself on every path that does not hand it to the pool: a failed Create as before, and a panic unwinding start in a deferred close under an ended context. `TestAPanickingLoggerNeverFailsAStepOnGuestOutput` failed Register on the old code (a partial line in Create leaked its module, a complete line was the plugin's trap) and passes; `TestAFailedStartClosesItsInstance` covers the trap, Create's error, the load error, the guest's panic and a host panic (left one module open before) with instances, memory in use and reservations back to zero. `TestGuestOutputGuardsDoNotAllocate` pins that the guard adds no allocation per line, with and without `-race`. Evidence in PLAN.md (Adversarial review fixes, 07).

Gate: `make test` now runs at `-parallel 4` (was 8): with other workloads resident, the root package's run was OOM-killed at 8 in five runs out of five; tests and thresholds unchanged (PLAN.md).
