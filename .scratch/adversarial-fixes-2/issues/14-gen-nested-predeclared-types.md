# 14: Generated codecs never use shadowed predeclared names

**Priority:** Low · **Difficulty:** Moderate · **Category:** Bug, code generation

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 10 (completes adversarial fix 13).

Inferred from reading the code: reproduce before fixing.

**What to build:** `plg gen` produces compiling code when the contract redeclares a predeclared type and uses an imported struct whose fields have that type. Today a package declaring `type int = int32` that returns `unicode.RangeTable` generates `plgimpl.ReadInt[int](...)` for the `LatinOffset` field; `int` then means `int32` and the code does not compile, while generation reports success. Render predeclared types through the unshadowable aliases everywhere in codec generation (fields of imported types included), or reject the conflict clearly.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A generator test with `type int = int32` and an imported struct containing an `int` field, compiling the output reproduces the bug (fails on the current code) and passes with the fix
Generated code compiles and round-trips; output for existing contracts unchanged
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
