# 10: The manifest's memory limit matches Register's rules

**Severity:** low (from the code review of `a1988b6`)

**What to build:** A manifest that `ParseManifest` accepts also registers. Today `memoryLimit` values from 1 to 65535 pass `ParseManifest` and `List`, then fail at Register with `ErrInvalidDescriptor` (minimum is one 64 KiB wasm page). Enforce the same minimum (and maximum) in manifest decoding, from one shared definition.

**Blocked by:** None (can start immediately)

**Status:** done

- [x] A regression test with `memoryLimit: 1` and `65535` reproduces the bug (fails on the current code) and passes with the fix
- [x] `ParseManifest`/`List` reject them with `ErrInvalidManifest`; 65536 is accepted by both manifest parsing and Register
- [x] Fuzz target for manifests still passes
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
