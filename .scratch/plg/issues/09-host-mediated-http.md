# 09: Host-mediated HTTP

**What to build:** Plugins can call HTTP APIs, with the host making each request on their behalf; it is off by default. `WithHTTP(plg.HTTPPolicy{...})` per Register, with these defaults once enabled: required AllowedHosts, BlockPrivateNetworks true (SSRF protection), Timeout 10s and never past the call's ctx deadline, MaxRequestBytes 1 MiB, MaxResponseBytes 4 MiB, MaxRequestsPerCall 16. Inside the Go guest, `http.DefaultTransport` is a transport that calls the host, so plugins use the standard `net/http` client (net/http is added to the guest allowlist; the client side only works through the host). Every request is logged by plugin key; metrics follow in ticket 14.

**Blocked by:** 02 (Tracer bullet)

**Status:** done

- [x] A Go plugin's `http.Get` against an allowlisted test server returns the response
- [x] Non-allowlisted hosts and private-network addresses (including via DNS) are refused
- [x] Oversized requests or responses and exceeding MaxRequestsPerCall produce errors, never memory blowups
- [x] Cancelling the call's ctx aborts an in-flight request
- [x] Without WithHTTP every request is refused
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
