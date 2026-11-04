# 11: Nested Lua host calls cannot overwrite each other's answers

**Priority:** Medium · **Difficulty:** Moderate · **Category:** Bug, reentrancy

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 8 (the Lua counterpart of adversarial fix 12).

Inferred from reading the code: reproduce before fixing.

**What to build:** A Lua Host call's answer cannot be overwritten by another Host call made while it is being decoded. Today the host answer goes into one global buffer, and decoding a struct calls `lua_createtable`, which can run a plugin's `__gc` finalizer that makes another Host call and overwrites or reallocates that buffer; equal-length answers silently corrupt results (`{N=123}` decoded as `{N=99}`). Give each in-progress Host call its own answer buffer (surviving Lua errors), and audit the Lua JSON module and HTTP path for the same shared-buffer assumption.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A Lua test whose `__gc` finalizer makes a Host call while an outer struct answer is decoded reproduces the bug (fails on the current code) and passes with the fix
The outer call gets exactly its own answer; no extra allocation on the non-nested path (benchmark)
- [ ] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
