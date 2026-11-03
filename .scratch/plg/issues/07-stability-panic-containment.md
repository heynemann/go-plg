# 07: Stability: panic containment and inspection

**What to build:** No plugin failure can crash the host, and every failure can be inspected:
- `recover()` at every host-side boundary: storage List/Open, encoding, Host dispatch, HTTP, pollers, refillers, user callbacks.
- Guest traps (out-of-memory, stack overflow, runtime abort, deadline) become errors.
- Inside the Go guest, panics are recovered at the call boundary and returned to the host as `PanicError`s (the instance is still discarded).
- `go` statements are rejected at load (parse plugin source before interpreting it): goroutines in a WASM guest are paused between calls and give no parallelism.
- Guest stdout/stderr are captured per instance (bounded) and sent to slog with the plugin key.
- Every failure is a `*plg.PanicError` you can extract with `errors.As`. Shape from PLAN.md:
  `Key, Language, Phase (Load|Create|Call|Host|HTTP|Storage|Close|Poll|Refill|Callback), Method, Value, Stack (host), Guest (guest traceback), Time`
- `WithOnPanic(func(ctx, *PanicError))` (itself guarded), a bounded lock-free history via `c.RecentPanics()` (`WithPanicHistory(n)`, default 64), Error-level logs.

**Blocked by:** 06 (Pooling and resource limits)

**Status:** done

- [x] Tests with Go plugins that panic, run out of memory, recurse forever and loop forever: the host process survives and each call returns a PanicError with the right Phase and a guest traceback
  - The guest traceback is present for panic, out of memory, unbounded recursion and runtime abort. For loop forever (a call stopped at its deadline) none exists: wazero v1.12 returns a bare `sys.ExitError` for close-on-context-done (`wasmdebug.FromRecovered` drops the stack for exit errors) and a single-threaded wasm yaegi cannot report its position; that PanicError is told apart by `errors.Is(err, context.DeadlineExceeded)`. Documented in PLAN.md (ticket 07) and accepted by the coordinator.
- [x] A plugin containing a `go` statement fails Register with the file and line
- [x] A panicking Storage implementation or OnPanic callback doesn't crash the host
- [x] RecentPanics returns the newest n entries; concurrent recording passes -race
- [x] Guest stdout/stderr appear in slog tagged with the plugin key
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
