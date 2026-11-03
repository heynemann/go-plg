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

**Status:** ready-for-agent

- [ ] Tests with Go plugins that panic, run out of memory, recurse forever and loop forever: the host process survives and each call returns a PanicError with the right Phase and a guest traceback
- [ ] A plugin containing a `go` statement fails Register with the file and line
- [ ] A panicking Storage implementation or OnPanic callback doesn't crash the host
- [ ] RecentPanics returns the newest n entries; concurrent recording passes -race
- [ ] Guest stdout/stderr appear in slog tagged with the plugin key
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
