# 01: plugin.yaml replaces plugin.json

**What to build:** Plugin manifests are YAML. `plugin.yaml` replaces `plugin.json` in a clean cutover with no fallback: a directory with only `plugin.json` is not a plugin. The fields keep their meaning (`language`, `entrypoint`, `version`, `memoryLimit`).

Parsing uses `go.yaml.in/yaml/v3` (the maintained successor of the archived `gopkg.in/yaml.v3`) in a strict subset:
- the file is still capped at `MaxManifestSize`, regular files only;
- it is parsed into a node tree that must contain no anchors, aliases, merge keys or custom tags, and exactly one document;
- it is then decoded with unknown fields disallowed; duplicate keys are errors.

`plg.ParseManifest` takes YAML. Every testdata plugin, the examples (the S3 storage keeps `plugin.yaml` under its prefix; the Postgres storage stores the manifest as a `plugin.yaml` file row instead of language/version/memory columns, read with `ParseManifest`), the README quick start and all docs move over in the same commit.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] A regression/behaviour test suite for `ParseManifest` covers valid manifests and each rejected construct (anchor, alias, merge key, custom tag, two documents, unknown field, duplicate key, wrong types, oversize)
- [ ] The existing manifest fuzz target is converted to YAML and run
- [ ] No `plugin.json` remains in the repository (testdata, examples, docs); a directory with only `plugin.json` is reported as not a plugin, with a hint naming `plugin.yaml`
- [ ] Postgres and S3 examples store and parse `plugin.yaml` through `ParseManifest`; their testcontainers tests pass
- [ ] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
