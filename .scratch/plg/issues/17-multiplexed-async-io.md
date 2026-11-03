# 17: Multiplexed async I/O for TS and Lua

**What to build:** While a TS `fetch` or a Lua `http.request` (yielding a coroutine) is waiting, the instance accepts other calls, so one instance overlaps many I/O-bound calls instead of each holding a pooled instance. Go plugins stay one call per instance. Kept only if benchmarks of I/O-heavy plugins show a clear memory/throughput win, with each call still isolated.

**Blocked by:** 12 (TypeScript plugins), 13 (Lua plugins), 14 (OTel metrics and benchmark budgets)

**Status:** done

- [x] A benchmark with high-latency HTTP calls shows throughput and memory for blocking vs multiplexed
- [x] Calls interleaved on one instance can't observe each other's arguments or results (tests)
- [x] Deadlines, traps and discard rules still apply per call; one failing call discards the instance and fails only the calls running on it, with clear errors
- [x] If there's no clear win, the ticket closes with the data and blocking stays (there was a clear win: shipped, data in PLAN.md)
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
