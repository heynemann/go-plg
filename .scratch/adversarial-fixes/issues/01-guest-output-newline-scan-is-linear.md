# 01: Guest output processing is linear in the write size

**Priority:** Critical · **Difficulty:** Easy · **Category:** Allocation/CPU hotspot, Runtime failure

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** A plugin cannot burn host CPU by writing to stdout/stderr. Today the guest-output stream searches the WHOLE remaining write for a newline on every iteration but only consumes up to `MaxLine` (4 KiB) of it, so a newline-free write costs quadratic time: one 64 MiB write (fits a Go guest's 128 MiB limit) is ~512 GiB of scanning. Processing also continues after the step's 64 KiB log budget is spent. It all runs inside a host function (the WASI write), which the call's deadline cannot interrupt, so it breaks the "CPU cap" promise.

Search only within the next bounded chunk, and once the step budget and its truncation notice are spent, skip line processing for the rest of the step (keeping the bounded stderr tail used for tracebacks).

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A test writing a large newline-free buffer (bounded so it stays fast after the fix, e.g. measuring bytes scanned via a counter or a benchmark-style time bound with wide margin) reproduces the bug (fails on the current code) and passes with the fix
- [x] Cost of `Write` is O(len(data)) — a benchmark over 1, 8, 64 MiB newline-free writes shows linear scaling
- [x] After the budget is spent, writes do no per-line work; the stderr tail used for `PanicError.Guest` is still captured
- [x] Existing guestout tests (budget, empty lines, truncation notice) pass unchanged
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket

Resolution: `TestOutputPastTheBudgetIsNotScanned` (a 64 MiB write whose middle is unreadable memory) faulted on the old code and passes; `BenchmarkNewlineFreeWrite` 1/8/64 MiB went from 1.09 ms / 76.8 ms / 11.3 s to 18 µs / 10 µs / 8.5 µs. Details in PLAN.md (Adversarial review fixes, 01).
