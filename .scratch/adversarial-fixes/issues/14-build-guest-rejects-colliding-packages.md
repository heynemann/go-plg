# 14: build-guest rejects packages whose extracted files collide

**Priority:** Low · **Difficulty:** Easy · **Category:** Bug

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** `plg build-guest` never builds a guest silently missing a requested package. Today `example.com/lib/a-b` and `example.com/lib/a/b` both map to the same extracted symbol file name, so the second overwrites the first, the extraction check sees the surviving file twice and reports success, and plugins importing the lost package fail later. Detect normalized-name collisions before extraction and fail with a clear message (or isolate outputs so both work).

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A buildguest test requesting two colliding package paths reproduces the bug (fails on the current code) and passes with the fix
Clear error naming both packages (or both packages usable in the guest)
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
