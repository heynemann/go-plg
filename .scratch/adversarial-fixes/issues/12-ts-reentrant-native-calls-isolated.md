# 12: Reentrant TypeScript host calls cannot corrupt each other's arguments

**Priority:** Medium · **Difficulty:** Moderate · **Category:** Bug

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** A TS host call's arguments cannot be overwritten by another host call made while they are being encoded. Today `host_method` (and `native_http`) encode into one global request buffer, and encoding a struct runs plugin getters; a getter calling `host.Inner(99)` resets and overwrites the outer request, so `host.Outer(1, {get N(){ host.Inner(99); return 2 }})` silently reaches the host as `Outer(99, {N:2})`. Make request storage and conversion state per invocation when native calls re-enter (keeping the shared buffer for the common non-reentrant case).

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A TS test with a getter that makes a nested host call (and a nested fetch) during argument encoding reproduces the bug (fails on the current code) and passes with the fix
The host receives exactly the outer and inner arguments; no per-call allocation added to the non-reentrant path (benchmark unchanged)
- [ ] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
