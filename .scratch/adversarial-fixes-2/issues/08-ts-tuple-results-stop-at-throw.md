# 08: TypeScript multi-result encoding stops at the first throwing getter

**Priority:** Medium · **Difficulty:** Easy · **Category:** Bug, error mapping

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 13.

Inferred from reading the code: reproduce before fixing.

**What to build:** When a TS method returns several results and reading one throws, the call fails with the plugin's own error, no later getter runs, and an out-of-memory there discards the instance like any other OOM. Today every element is read without checking for `JS_EXCEPTION`, so the exception becomes `ErrConversion`, later getters (possibly with Host side effects) still run, and OOM keeps an instance that should be discarded. Check each property read and unwind through the existing failure path.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] TS tests with a multi-result method whose result getter throws, whose later getter records side effects, and whose getter runs out of memory reproduces the bug (fails on the current code) and passes with the fix
Plugin error returned (not ErrConversion), later getters not run, OOM discards the instance
- [x] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
