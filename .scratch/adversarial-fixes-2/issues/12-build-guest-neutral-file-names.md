# 12: build-guest never drops packages through file-name build constraints

**Priority:** Low · **Difficulty:** Easy · **Category:** Bug

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 11.

Inferred from reading the code: reproduce before fixing.

**What to build:** `plg build-guest` includes every requested package in the guest. Today the extracted symbol file is named after the package path, so a package ending in `/helper_test` or `/helper_linux` produces a `_test.go` or `_linux.go` file that the wasip1 build ignores, and build-guest reports success while the package is missing. Rename extracted files to neutral names (or verify each symbol table is in the compiled file set) and keep the collision check from adversarial fix 14 working.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A buildguest test requesting packages ending in `_test` and `_linux` reproduces the bug (fails on the current code) and passes with the fix
- [x] Both packages are importable by a plugin in the built guest (or rejected with a clear error)
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
