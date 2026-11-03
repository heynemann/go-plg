# 09: plugin.json reads are bounded

**Severity:** low (from the code review of `a1988b6`)

**What to build:** The filesystem storage only reads `plugin.json` when it is a regular file, and caps its size. Today it is read with no type check or limit, so a `plugin.json` that is (or links to) `/dev/zero` grows memory until the host dies during `List`, and a FIFO blocks `Register` or the poller forever. Open it, require a regular file, and read through a size limit (the S3 example already caps manifests at 64 KiB — use one shared limit and expose it in docs).

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] Regression tests for a FIFO, a `/dev/zero` symlink and an oversized manifest reproduces the bug (fails on the current code) and passes with the fix
- [ ] Each fails with `ErrInvalidManifest` naming the plugin directory, promptly, with bounded memory
- [ ] The cap is documented and shared with `ParseManifest`/the examples' storages
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, deterministic tests (no sleeps/retries), `make check` passes on the landed commit (and `make verify-guests` if guest sources changed), one commit titled exactly as this ticket
