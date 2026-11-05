# 09: URL parse errors never log credentials

**Priority:** Medium · **Difficulty:** Easy · **Category:** Security, credential disclosure

Source: second adversarial review of `4a3c31a` (model gpt-6.1-sol), Review finding 14.

Inferred from reading the code: reproduce before fixing.

**What to build:** A refused HTTP request never writes credentials or query strings to host logs. Today URL parse failures keep Go's `*url.Error`, whose message contains the full input, so `https://alice:password@api.example/%zz?access_token=secret` logs the password and token in the error field. Replace parse errors at the boundary with a non-sensitive reason, in both places the handler parses URLs, and check every other error logged by httpcap for the same leak.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A test with such a URL (password and token) through the handler and a plugin fetch, capturing logs reproduces the bug (fails on the current code) and passes with the fix
Neither the password nor the token appears in logs, metrics or the error returned to the plugin; the refusal reason is still clear
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
