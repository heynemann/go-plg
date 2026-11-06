# Rules

These rules apply to every change in this repository. A ticket is **not done** until its change satisfies all of them. The design lives in [PLAN.md](PLAN.md).

## 1. Lint: golangci-lint v2, strict

- **Config:** [.golangci.yml](.golangci.yml), with `default: all` linters. The only exceptions are listed in the file, each with its reason. `wsl_v5` is enabled; the deprecated `wsl` is not.
- **Before every commit**, run:
  ```sh
  make fmt-check lint
  ```
  Both must pass with zero issues; CI runs the same targets and fails on any issue. They use the golangci-lint pinned in the Makefile, built with the repo's toolchain into `bin/` with its own cache. Don't rely on a system-wide golangci-lint: one built with an older Go silently skips type-aware rules, so it can pass where CI fails.
- **`//nolint` needs justification:** it must name the specific linter and give an explanation (`nolintlint` enforces both). It's acceptable only when the linter is demonstrably wrong for that line. Never use it to silence a real finding.
- **Changing the config:** adding a disable or exclusion requires a written reason in the config itself. Never weaken thresholds to make a change pass.
- **The config enforces project rules mechanically:**

| Rule | Enforced by |
|---|---|
| No `sync.Mutex`, `sync.RWMutex`, `xsync.RBMutex` (§4) | forbidigo |
| No `sync.Map`; use `xsync.Map` (§4) | forbidigo |
| No `encoding/json`; use sonic (§5) | depguard |
| No stray `print`/`fmt.Print*`; use slog | forbidigo |

## 2. Tests: trustworthy, not theatrical

This library runs third-party code inside other people's servers, so tests are the main evidence that it is safe. **Every behavior change ships with the tests that prove it.**

### Unit tests
- **Coverage:** aim for 100% statement coverage of non-generated code. CI fails if any package drops below **95%**. An uncovered block is acceptable only if it is unreachable defensive code, and the change that leaves it must say why.
- **Each test must fail for a plausible bug.** Cover the happy path *and* every edge case:
  - boundaries: empty, nil, zero, maximum, just over the limit;
  - every error path;
  - state transitions: load → serve → reload → drain → close;
  - precedence: Register options vs `plugin.yaml`; last good version vs a new failure;
  - concurrency invariants.
- **Banned (test theater):**
  - tests of wiring or forwarding;
  - tests that only check what a mock echoes back;
  - getter/setter tests and tautologies;
  - "does not panic" with no assertion on the result;
  - assertions that something is merely "non-empty";
  - pinning wording or internal implementation details;
  - duplicate tests of the same path.
- **Style:**
  - Table-driven with `t.Parallel()`, testify (`require` for preconditions, `assert` for checks).
  - Black-box `package plg_test` by default. Internal tests live only in `internal_test.go` / `export_test.go`.
- **Determinism:** no `time.Sleep` for synchronization. Use `testing/synctest` for time-dependent logic (refill backoff, polling, draining) and channels or explicit signals otherwise. No dependence on test order: CI runs with `-shuffle=on`.
- **Race detector always:** every test run uses `-race`. CI also runs the suite with `-count=10` to flush out flakiness.
- **Goroutine hygiene:** `go.uber.org/goleak` verifies in `TestMain` that every package leaks no goroutines.
- **Fuzzing:** every parser or decoder that touches untrusted input has a fuzz target: the codec, `plugin.yaml`, interface validation, HTTP policy and host matching. Crashers found by fuzzing become permanent seed-corpus cases.

### Integration tests
- **Real dependencies only:** they exercise **real storages and real plugins**, with no mocks of wazero, guests or storages.
  - **Storages:** real filesystem directories, `embed.FS`, S3 (MinIO) and Postgres via testcontainers. The S3 and Postgres storages live in `examples/` with their own module, so the core stays free of those dependencies.
  - **Plugins:** the committed guest artifacts running real Go, Lua and TS plugin sources.
- **Hostile plugin corpus:** a shared set of malicious and broken plugins in every language, run against every guest. It must include:
  - out of memory;
  - infinite loop;
  - unbounded recursion;
  - panic or throw;
  - oversized results;
  - SSRF attempts and blocked hosts;
  - a broken `Create()`;
  - malformed manifests;
  - duplicate keys;
  - reloads under load.

  In every case the host process survives, and the call returns the documented error or `PanicError`.
- **Contract suite:** one behavior suite runs against equivalent plugins in Go, Lua and TS, so all three languages are held to the same semantics.
- **Running them:** integration tests are behind the `integration` build tag (`go test -race -tags integration ./...`) and run in CI on every change.

## 3. Benchmarks: prove fast *and* safe

- **Every hot path has a benchmark** using `b.ReportAllocs()`, plus a `b.RunParallel` variant wherever concurrent use is expected:
  - `Get`/`All`;
  - one plugin call per guest;
  - the codec, per data shape;
  - pool acquire/release under contention;
  - Host dispatch;
  - the HTTP round trip;
  - Register and Reload.
- **Benchmark with safety on:** headline numbers always run with the CPU cap, memory limits and recover boundaries in place. A benchmark with a protection disabled must say so in its name and never stand in for the headline number.
- **Budgets enforced in CI:** `Get` ≤ 50 ns/op and 0 allocs/op; `All` 0 allocs/op.
- **Tracking:** other results are recorded per commit and compared with `benchstat` against `main`. A regression worth noting, or any new allocation on a hot path, must be explained in the change that causes it.
- **Optimizations need proof:** an optimization ships only with benchmark output showing the gain (for example ticket 16, generated fast paths).

## 4. Concurrency safety

- **Everything is safe for concurrent use:** every exported API is goroutine-safe, and the docs say so.
- **No mutexes of any kind:** not `sync.Mutex`, `sync.RWMutex` or `xsync.RBMutex`, and no mutex disguised as a channel lock, custom spinlock or renamed wrapper.
- **Shared mutable state** uses `github.com/puzpuzpuz/xsync/v4` structures or `sync/atomic`. Prefer immutable snapshots published through `atomic.Pointer` (the plugin catalogue), so readers never contend.
- **Channels** are for ownership transfer and signaling (idle-instance pools, refill wake-ups, shutdown), not for guarding state.
- **Every goroutine has an owner and a shutdown path** tied to `Container.Close` or its plugin generation. goleak checks this in tests (§2).
- **No `context.Context` stored in structs.** ctx flows through calls.
- **Hot paths:** minimize allocations and copying, and reuse buffers when ownership and lifetime are clear. Never trade isolation between plugins or calls for fewer allocations.

## 5. JSON: sonic

- **Go code in this repository** uses `github.com/bytedance/sonic` for JSON (e.g. the log records tests decode), never `encoding/json` (depguard enforces this). sonic v1.15.5 has been verified on go1.27.3/amd64, including under `-race`.
  - **sonic depends on Go runtime internals,** so every Go upgrade needs a green CI run on the new version before it's adopted.
- **The exception is the guest stdlib symbol tables** (excluded in the config). Go plugin authors import the standard `encoding/json` from the guest allowlist. Inside wasm, sonic can't JIT, so routing plugin JSON to the host would only add a host call and copies.
- **JSON helpers for plugins**, by language:
  - **Go plugins:** use `encoding/json` from the allowlist; no host helper.
  - **TS plugins:** use the native `JSON`; no host helper.
  - **Lua plugins:** Lua has no JSON, so the Lua guest bundles an in-guest `json` module (`json.encode` / `json.decode`, compiled into the Lua artifact). This avoids a host round trip; see ticket 13.
- **The host–guest boundary never uses JSON.** It uses the binary codec ([PLAN.md](PLAN.md)).

## 6. Commits: one per ticket, only when every gate passes

- **One ticket, one commit.** Each ticket in `.scratch/plg/issues/` lands as exactly one commit.
  - **Message:** `NN: <ticket title>`, with a short body saying what was delivered and any decisions recorded in PLAN.md.
  - **Include the ticket file:** the same commit flips the ticket's **Status** to `done` and ticks its acceptance criteria.
  - **Never** split one ticket across commits, combine two tickets in one commit, or commit unrelated changes.
- **Commit only when every gate passes on the final tree.** If anything changes after a gate ran, run all of them again.
  1. **Formatting:** `make fmt-check` produces no diff.
  2. **Linting:** `make lint` reports zero issues.
  3. **Testing:**
     - unit tests with `-race -shuffle=on`;
     - integration tests with `-tags integration`;
     - the per-package 95% coverage gate;
     - goleak;
     - the enforced benchmark budgets (§3).

  A single local command runs exactly the gates CI runs (set up in ticket 01). Use it rather than a subset.
- **A failing gate means the work isn't done.** Fix the cause; never weaken a gate, skip a test, add an unjustified `//nolint` or use `--no-verify` to get a commit through.
- **Never rewrite history** of commits that have already been pushed.

## 7. Docker containers: never leave them running

Orphaned containers keep eating memory on the developer's machine (Docker Desktop shares the WSL VM's memory with everything else), so cleanup is mandatory, not best effort.

- **Any container started to test something MUST be terminated as soon as it isn't needed anymore.** This applies to containers started by tests (testcontainers), by scripts, by `make` targets and by hand while investigating. Leaving a container running after you're done with it is never acceptable.
- **Tests own their containers through `t.Cleanup`:** a test (or the helper it calls, which takes a `testing.TB`) registers the container's termination with `testcontainers.CleanupContainer(tb, ctr)` on the line right after starting it, before checking the start's error, so a container that started partially goes too, and the run passes, fails or panics without leaving it behind. `TestMain` never owns a container, and neither does a `defer`. Each top-level test that needs a server starts its own; its subtests share it. `scripts/gate-test.sh` fails a Go file that starts a container without that line or terminates one any other way. Keep testcontainers' reaper (Ryuk) enabled as a second line of defence for killed test processes; never disable it.
- **Compose stacks:** a test run may need a compose stack to be running, and that's fine. But once the run completes **or fails**, the stack MUST be brought down (`docker compose down -v --remove-orphans`), e.g. from a `trap` in the script or a Make target that always runs the teardown.
- **Interrupted runs:** if a run is killed (timeout, out-of-memory, Ctrl-C) before its cleanup ran, remove what it left behind before starting anything else.
- **Verify after every run that starts containers:** `docker ps -a` must show nothing left over from it. A leftover container is a failing gate, like a leaked goroutine.
