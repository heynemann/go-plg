# 10: Reload, polling and draining

**What to build:** A running container follows changes in its storages without a restart:
- `c.Reload(ctx)` refreshes every registered interface; `plg.ReloadInterface(ctx, c, name)` refreshes one; `WithPollInterval(d)` per Register polls in the background.
- Unchanged plugins (same version or hash) are skipped.
- Mirroring rules: removed plugins disappear; new but broken plugins are skipped and logged; updated but broken plugins keep their last good version; duplicate keys keep the previous version and are logged.
- Each plugin generation counts its in-flight calls atomically. Once a replaced or removed generation has drained, its instances close and the plugin's optional `Close` runs.
- `c.Close(ctx)` stops pollers and refillers and drains everything.

**Blocked by:** 05 (Filesystem and fs.FS storages), 06 (Pooling and resource limits)

**Status:** ready-for-agent

- [ ] Editing a plugin on disk and reloading serves the new version; calls already running finish on the old one
- [ ] A broken update keeps the previous version serving and logs the failure
- [ ] Removing a plugin makes Get return ErrNotFound after reload
- [ ] Polling detects changes without explicit Reload; unchanged plugins don't recompile
- [ ] Draining calls the plugin's Close after the last in-flight call; c.Close returns after everything is drained
- [ ] Reload racing with heavy call traffic passes -race
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
