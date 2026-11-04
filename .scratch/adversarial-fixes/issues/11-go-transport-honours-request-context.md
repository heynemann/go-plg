# 11: The Go guest's HTTP transport honours the request's context

**Priority:** Medium · **Difficulty:** Easy · **Category:** Bug, Unfulfilled promise

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** A Go plugin's HTTP request whose context is already cancelled is not sent. Today the guest transport never checks `req.Context()`, so a POST built with a cancelled context still reaches the server (the host runs it under the outer call's ctx), causing unwanted side effects. Check cancellation before encoding/sending (closing the request body as `RoundTripper` requires), and document that request-level deadlines shorter than the call's are not yet propagated to the host (or propagate them, if feasible within this ticket).

**Blocked by:** None (can start immediately)

**Status:** done

- [x] An integration test with a Go plugin sending a POST under a cancelled context to a test server reproduces the bug (fails on the current code) and passes with the fix
The request fails with the context error and the server sees nothing; request body closed
README/PLAN document the request-deadline behaviour
- [x] Guest artifact rebuilt with `make guests` and committed; `make verify-guests` clean
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket

**Resolution:** the Go guest's transport refuses a request whose context has ended, before sending anything, closing its body; request deadlines shorter than the call's are documented as not propagated (a new ABI). See PLAN.md, "Adversarial review fixes", 11. Tests: `TestGoPluginHTTPRequestsWithACancelledContextAreNotSent`, `TestTransportRefusesARequestWhoseContextEnded`.
