# 06: Pooling and resource limits

**What to build:** Plugins are served by a bounded pool of instances, each with its own `Create()` result:
- `WithPool(min, max)` (max defaults to GOMAXPROCS). A background refill goroutine per plugin keeps `min` warm. Callers take an idle instance, create one only while under max and the budget, or wait on ctx. A `Create()` that fails during refill backs off exponentially and is reported.
- Per-instance memory limit: `WithMemoryLimit` per Register (Go default 128 MiB, Lua/TS 64 MiB), which plugin.json may lower but never raise. One wazero runtime per distinct limit, all sharing the compilation cache.
- Container budget: `WithMemoryBudget(total)`. Register fails if plugins × pool max × limit exceeds it, and pools stop growing at the budget.
- CPU cap is always on: a call is stopped preemptively when its ctx ends, and that instance is closed and replaced.
- Instances are discarded on a trap, deadline, panic or encoding failure, and kept when the plugin returns an ordinary error. (wazero leaves trapped instances callable, and they then return `err=nil`.)
- Shared state uses atomics, xsync or channels only.

**Blocked by:** 02 (Tracer bullet)

**Status:** done

- [x] 64 goroutines × thousands of calls against a pool pass under `go test -race` with correct results
- [x] A Go plugin in an infinite loop is stopped at the ctx deadline; the next call succeeds on a fresh instance
- [x] A plugin allocating past its limit fails that call; other plugins and instances keep working
- [x] Register fails when the worst-case memory exceeds WithMemoryBudget; pools never exceed the budget at runtime
- [x] After discards, the pool returns to min in the background without callers creating instances
- [x] A plugin returning an ordinary error keeps its instance (verified by instance identity)
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
