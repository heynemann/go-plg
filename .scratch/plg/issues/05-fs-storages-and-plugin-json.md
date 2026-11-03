# 05: Filesystem and fs.FS storages with plugin.json

**What to build:** Plugins can live on disk or in any `io/fs.FS` (embed, zip, …). Each plugin is a directory with `plugin.json` (`language`, `entrypoint`, optional `version`, optional `memoryLimit`). The directory name is the plugin key. A missing version is replaced by a content hash. The plugin's fs.FS is mounted read-only into the guest, so Go plugins can span several files in one package. Register options `WithGolangPlugins()`, `WithLuaPlugins()` and `WithTypescriptPlugins()` act as a language allowlist: none given means all; plugins in a language not enabled are skipped and logged, not treated as errors. Duplicate keys across the storages of one Register fail Register.

**Blocked by:** 02 (Tracer bullet)

**Status:** ready-for-agent

- [ ] A filesystem directory of plugins and an embed.FS of plugins both load through Register
- [ ] A multi-file Go plugin package works
- [ ] A plugin outside the language allowlist is skipped with a log line and Register still succeeds
- [ ] Duplicate keys across two storages fail Register with both storage names in the message
- [ ] Malformed or missing plugin.json fails Register naming the plugin directory
- [ ] Descriptor versions fall back to a stable content hash
- [ ] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
