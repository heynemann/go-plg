# 03: HTTP protocol upgrades are refused

**Priority:** High · **Difficulty:** Easy · **Category:** Runtime failure, Security

Source: adversarial review of `4503972` (model gpt-6.1-sol), inferred from reading the code.

**What to build:** A plugin's HTTP request cannot hang when an allowed server answers `101 Switching Protocols`. Today the handler encodes the 101 response's body; Go's transport has handed the upgraded connection over and no longer enforces the request's timeout or cancellation, so a silent server blocks a blocking call inside the host import forever and makes a multiplexed instance's shutdown wait forever. plg does not support upgraded protocols, so refuse 101 responses (close the body immediately) with a clear error, in both the blocking and multiplexed paths.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] Tests with a server that answers 101 and then stays silent, for a blocking Go plugin call and a multiplexed TS/Lua call reproduces the bug (fails on the current code) and passes with the fix
The request fails promptly with a documented error; the call and `Container.Close` return; no goroutine leak (goleak)
README HTTP section states upgrades are refused
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
