# 11: Pool resilience: panicking meter/log handlers and the in_use metric

**Severity:** low (from the code review of `a1988b6`)

**What to build:** Three small pool defects found by the review:
1. A panicking `MeterProvider` (metrics recorded on release) skips returning the instance to the idle queue, leaking the instance and its pool place. Return the instance before recording metrics, and contain the panic via the panic reporter.
2. Log calls in the pool's discard and idle-close paths run outside any recover, so a panicking `slog` handler escapes into the plugin call. Contain them like other user callbacks (Phase Callback).
3. With `WithConcurrentCalls` > 1, the `plg.pool.instances.in_use` gauge counts lanes instead of instances. Report instances (and, if useful, add a separate lanes/in-flight instrument — documented).

**Blocked by:** None (can start immediately)

**Status:** done

- [x] Regression tests for each of the three defects fail on current code and pass after
- [x] Pool size, idle count and budget accounting are unchanged after a panicking meter or log handler (asserted via test hooks); host survives
- [x] Metrics test asserts `in_use` reports instances under `WithConcurrentCalls`; README/PLAN metric catalogue updated
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
