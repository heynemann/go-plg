# 01: Go guest artifact + compilation cache

**What to build:** Ship the generic Go guest: yaegi compiled to wasip1 with the agreed stdlib allowlist (bufio, bytes, context, encoding/base64, encoding/hex, encoding/json, errors, fmt, io, maps, math and math/*, net/url, regexp, slices, sort, strconv, strings, sync, sync/atomic, time, unicode/*; net/http comes in ticket 09). It is built reproducibly with pinned toolchains, committed as an embedded artifact in the Go guest package (enabled by a blank import), and loaded by wazero.

This ticket settles the plan's riskiest unknown first: the guest must interpret a contract package's **source**, with a plugin package that imports it by its real import path. If that can't work, switch to the proven fallback, a guest built per contract package with `yaegi extract` exports, and record the decision in PLAN.md.

Because the guest compiles in about 11–15 s cold, wazero's compilation cache is on disk by default (os.UserCacheDir()/plg/<wazero version>, overridable with `WithCompilationCacheDir`), falling back to memory when the directory isn't writable.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A make target rebuilds the Go guest; CI rebuilds it and fails if the committed artifact differs
- [x] Importing the Go guest package registers it; nothing guest-related is linked without that import
- [x] A test loads a contract package's source and a plugin source importing it, then gets the result of a plugin function back in Go
- [x] A second load in a new process uses the disk cache (compile time logged; well under 1 s versus a cold 11–15 s)
- [x] An unwritable cache directory falls back to the in-memory cache with a warning, not a failure
- [x] Guest artifact size and cold/warm compile times recorded in PLAN.md
- [x] If contract-source interpretation fails: the fallback is implemented and PLAN.md updated with the evidence (not needed: interpretation works, evidence in PLAN.md)
- [x] CI pipeline in place: `golangci-lint fmt`/`run` with the repo config, unit tests with `-race -shuffle=on`, integration tests (`-tags integration`), the per-package 95% coverage gate, goleak, and benchmark recording with benchstat
- [x] One local command (e.g. `make check`) runs exactly the gates CI runs (RULES.md §6), and fails if `golangci-lint fmt` leaves a diff
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
