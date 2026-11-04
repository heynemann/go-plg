# 09: Script redirects strip credentials across origins

**Priority:** High · **Difficulty:** Moderate · **Category:** Security, Duplication

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** Following a redirect in a TS or Lua plugin never forwards credentials to another origin. Today both redirect loops change the URL but keep all headers, so `Authorization` (and `Cookie`, `Proxy-Authorization`) reaches a different allowed origin — e.g. any host under a wildcard — and HTTPS→HTTP downgrades keep it too. The host uses `Transport.RoundTrip`, so Go's client-side stripping does not apply. Match Go's `http.Client` rules: drop sensitive headers when the redirect leaves the original host (or downgrades scheme), without mutating caller-owned header objects. The two copies of redirect logic (TS prelude, Lua C) should share one specification and, where possible, one test table.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A cross-language test (TS and Lua; Go too via net/http for parity) redirecting to another allowed origin and to an HTTP origin reproduces the bug (fails on the current code) and passes with the fix
Sensitive headers are absent on cross-origin/downgrade hops and kept on same-origin hops; caller's headers object unchanged
Behaviour documented in README HTTP section
- [x] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket

**Resolution:** one origin rule (scheme, host, port) for TS, Lua and Go — stricter than Go's `http.Client`, which keeps credentials on the same host across ports and schemes and on its subdomains; see PLAN.md, "Adversarial review fixes", 09. Shared test table: `TestPluginHTTPRedirectsKeepCredentialsWithinTheFirstOrigin`.
