# 04: The local-use NAT64 range is blocked under private-network protection

**Priority:** High · **Difficulty:** Easy · **Category:** Security

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** The SSRF check cannot be bypassed through the local-use NAT64 prefix. Today every address in `64:ff9b:1::/48` is checked by reading its low 32 bits, which is only correct for a /96 layout; under a /64 layout (RFC 6052 §2.2) `64:ff9b:1:0:c0:a801:800:0` embeds 192.168.1.8 but is checked as 8.0.0.0 and allowed. RFC 8215 §5 does not allow assuming one layout across the /48. With `BlockPrivateNetworks` on, block the whole `64:ff9b:1::/48` range (the well-known `64:ff9b::/96` keeps its exact check). Document the decision.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A test dialing `64:ff9b:1:0:c0:a801:800:0` (and other /48 layouts embedding private IPv4) reproduces the bug (fails on the current code) and passes with the fix
All `64:ff9b:1::/48` addresses are refused when private networks are blocked; allowed when `BlockPrivateNetworks` is false
Existing NAT64/6to4/mapped tests pass; README/PLAN SSRF notes updated
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
