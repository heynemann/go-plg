# 07: TypeScript buffer extraction failures raise errors

**Priority:** Medium · **Difficulty:** Easy · **Category:** Bug, data loss

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 12.

Inferred from reading the code: reproduce before fixing.

**What to build:** A detached or otherwise unreadable `Uint8Array`/`ArrayBuffer` raises an error instead of crossing as empty bytes. Today the TS glue treats `!p && n` as failure, but QuickJS returns NULL with size 0 on failure, so failures pass as empty data for plugin results, Host arguments, fetch POST bodies and UTF-8 decoding. Detect failure by NULL plus a pending exception (or an explicit detached check) at every buffer-extraction site and propagate it.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] TS tests returning, passing to a Host method, and POSTing a detached `Uint8Array` (via `ArrayBuffer.prototype.transfer`) reproduces the bug (fails on the current code) and passes with the fix
Each fails with the plugin-side TypeError; genuine empty buffers still cross as empty
- [x] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
