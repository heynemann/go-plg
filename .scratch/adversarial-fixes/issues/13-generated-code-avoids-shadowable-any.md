# 13: Generated code does not depend on the shadowable any identifier

**Priority:** Low · **Difficulty:** Easy · **Category:** Bug

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** `plg gen` produces compiling code even when the contract package redeclares `any` (e.g. `type any = int`). Today the generated fast-path constructor returns literal `any`, which then resolves to the package's alias and fails to compile, while generation reports success because output is only formatted, not type-checked. Emit `interface{}` (or a qualified/aliased form) for implementation-only types, and audit the generator for other predeclared identifiers it relies on (`error`, `string`, `len`, `make`, …).

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A generator test with a contract package declaring `type any = int` (and other shadowed predeclared identifiers) that compiles the generated output reproduces the bug (fails on the current code) and passes with the fix
Generated code compiles and passes the contract checks; output for existing contracts unchanged or regenerated
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
