# 08: plg pack and .plg artifacts

**What to build:** Validated plugins become one reproducible file the platform can store anywhere. `plg pack <plugin or repo>` runs validate (refusing on errors, and while a contract `replace` is active), copies shared directories in, and writes a deterministic `.plg` archive (tar.gz with fixed ordering, times and modes: same input, same bytes) containing the plugin files, `plugin.yaml` and a provenance record: content digest, git commit (if any, with a dirty flag), plg version, contract module versions and the validate report. `plg.OpenArtifact(r io.ReaderAt, size int64)` (or equivalent) returns an fs.FS over an archive with the same safety limits as storages, plus the provenance. Inspection (`plg.Info`, the admin handler) shows a plugin's provenance when it was loaded from an artifact; nothing is enforced at load.

**Blocked by:** 05 (Author repos: plg.yaml), 07 (Full validate report)

**Status:** ready-for-agent

- [ ] Tests: byte-identical archives from identical input, pack refusing on validate errors and on active replaces, OpenArtifact on good, truncated, oversized and path-traversal archives, a plugin loaded from an artifact through a storage showing provenance in Info and the admin JSON
- [ ] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
