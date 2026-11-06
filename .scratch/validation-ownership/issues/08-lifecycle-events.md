# 08: Lifecycle events: WithOnEvent

**What to build:** Teams can route plugin problems to the right people. `plg.WithOnEvent(func(ctx context.Context, e plg.Event))` (container option) receives typed events: Loaded, Rejected (load, validation or ownership failure, with the report or error), Retired, Panicked (with the `PanicError`), Validated (on-demand results) and Skipped (language not enabled). Every event carries key, version, language, storage and owner.

Delivery is asynchronous: one goroutine per container drains a bounded queue (1024); when it is full, events are dropped and counted (`plg.events.dropped` metric and a warning log). The callback runs under the existing callback guard (a panic becomes a `PhaseCallback` PanicError and delivery continues). `Close` delivers what is queued, bounded by its ctx, and stops the goroutine.

**Blocked by:** 04 (Validate at load, for Go plugins), 06 (On-demand validation)

**Status:** ready-for-agent

- [ ] Tests observe each event type with correct fields, including owner
- [ ] A slow callback never delays calls, Register or reloads (deterministic test with a blocking callback), and overflow drops and counts events
- [ ] A panicking callback is contained; Close drains and stops delivery (goleak)
- [ ] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
