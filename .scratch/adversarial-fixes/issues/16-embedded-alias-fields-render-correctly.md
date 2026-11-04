# 16: Anonymous structs with embedded aliases are rendered correctly

**Priority:** Low · **Difficulty:** Moderate · **Category:** Bug

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** The Go guest dispatcher and Host stubs render the exact contract types. Today, for `type Alias = Original`, a parameter `struct{ Alias }` has embedded field name `Alias` but reflection reports type `Original`; the renderer emits `struct{ p.Original }`, a different Go type, so a correct plugin cannot bind. Preserve the alias using contract source/type information, or reject this shape clearly at contract registration (and in `plg gen`) instead of generating a mismatching type.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A contract with an anonymous struct embedding an alias, loaded by a Go plugin reproduces the bug (fails on the current code) and passes with the fix
The plugin binds and round-trips (or registration fails with a clear, documented error)
- [x] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
