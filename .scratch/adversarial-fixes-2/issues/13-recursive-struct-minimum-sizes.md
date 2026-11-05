# 13: Mutually recursive structs are accepted

**Priority:** Low · **Difficulty:** Moderate · **Category:** Bug, contract validation

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 9.

Inferred from reading the code: reproduce before fixing.

**What to build:** Valid mutually recursive contract types register and round-trip. Today, for `type A struct{ Children []B }` and `type B struct{ Parent A }`, planning A first lets B capture A's unfinished minimum size of 0, which is never recomputed, so `[]B` is rejected as elements that encode as nothing. The generated layout planner has the same problem. Compute struct minimum sizes after the type graph is complete (pointers, slices and maps have fixed minimums), in both the wire planner and the layout planner.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] Contract tests registering and round-tripping the A/B shape through a Go plugin with both binders reproduces the bug (fails on the current code) and passes with the fix
- [x] Wire and layout planners agree; types that really encode as nothing are still rejected
- [x] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
