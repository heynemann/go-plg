# 10: The Postgres example rejects oversized plugins before transferring them

**Priority:** Medium · **Difficulty:** Moderate · **Category:** Security, memory exhaustion

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 4 (example code users will copy).

Inferred from reading the code: reproduce before fixing.

**What to build:** The Postgres storage example never loads more plugin bytes than plg would accept. Today `Open` selects every `bytea` content and holds them in memory before plg's snapshot limits run, and `Put` reads files with unbounded `fs.ReadFile`. Check per-file and total sizes in the database (e.g. `octet_length`) within the same consistent snapshot as the read, refuse oversized plugins before transferring content, and use bounded reads in `Put`, reusing plg's exported limits.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A testcontainers test storing an oversized file and an oversized total for an enabled plugin reproduces the bug (fails on the current code) and passes with the fix
`Open` fails with `ErrPluginTooLarge` (or equivalent) without transferring the content; `Put` refuses oversized files
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
