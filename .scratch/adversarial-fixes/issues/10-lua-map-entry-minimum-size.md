# 10: Lua accepts maps whose values encode to zero bytes

**Priority:** Medium · **Difficulty:** Easy · **Category:** Bug

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** Lua decodes every valid wire map. Today the Lua codec assumes each map entry takes at least 5 bytes, but `map[string]struct{}{"": {}}` encodes in 4 bytes per entry (empty key length prefix + zero-byte value), so Lua rejects a valid argument or Host result as truncated, reports an ABI failure and discards the instance; Go and TS accept it. Use 4 bytes (key length prefix) plus the value's minimum encoded size as the bound.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A shared contract-suite case passing and returning `map[string]struct{}` with empty keys, as an argument and as a Host result reproduces the bug (fails on the current code) and passes with the fix
Go, Lua and TS agree; truncated maps are still rejected
- [x] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket

**Resolution:** Lua bounds a map's count by 4 bytes plus the value type's minimum size, computed once per schema; see PLAN.md, "Adversarial review fixes", 10. Test: `TestMapsOfValuesThatEncodeAsNothingCross`.
