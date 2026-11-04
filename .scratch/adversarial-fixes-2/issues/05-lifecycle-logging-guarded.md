# 05: Every call into the user's logger is guarded

**Priority:** High · **Difficulty:** Moderate · **Category:** Runtime failure, panic containment

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 6 (completes adversarial fixes 06/07).

Inferred from reading the code: reproduce before fixing.

**What to build:** No log call plg makes can let a panicking `slog` handler escape or corrupt state. Today lifecycle logs still call the handler directly: a panic on "plugin skipped" escapes Register via `admit` and leaves the registration name reserved; cleanup logging in `closeUnpublished` can panic from `reloadRound`'s deferred cleanup and kill the reload goroutine; a panic on "guest compiled" closes the compile entry's `done` channel without an error, caching a nil module as a success. Route every logger call through the guarded logger (one place), and make state changes complete before logging. Audit and list every log call site in PLAN.md, like the metrics audit.

**Blocked by:** None (can start immediately)

**Status:** done

**Resolution:** every record plg logs goes through the panic reporter's guarded logger (the container's own, keyless, reporting under a record's `key` attribute; one per plugin version for its instances and pool; `Warm`'s), so a panicking handler is a `PanicError` in `PhaseCallback` and the record is dropped; `Container.compile` completes its entry however it ends, forgetting one a panic unwound. Audit table of every logger call site in PLAN.md, "Second adversarial review fixes", 05.

- [x] Tests with a handler panicking on each of the cited records (skipped plugin, cleanup during reload, guest compiled) reproduces the bug (fails on the current code) and passes with the fix
Register leaves no name reserved, the reloader keeps running, a later Register compiles the guest correctly; each panic recorded as PhaseCallback
Audit table of every logger call site in PLAN.md, each guarded
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
