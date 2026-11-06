# 02: Plugin ownership in plugin.yaml, shown in errors, logs and metrics

**What to build:** A plugin's manifest says who owns it, and everyone who hits a problem with the plugin sees it. `plugin.yaml` gains an optional `owner` block:

```yaml
owner:
  maintainers: [{name, email, handle}]   # the first is primary
  team: {name, department}
  org: ...
  company: ...
  contact: {oncall, channel, email}
  links: {repository, docs, runbook, issues}
  labels: {key: value}
```

It is parsed into `plg.Owner` on `Descriptor.Owner`, with documented limits on string lengths and the number of maintainers and labels. Ownership is shown:
- on `PanicError` and `PluginError` (new `Owner` field; messages unchanged);
- on every log record plg writes about a plugin (owner fields as attributes);
- on every plugin metric, as `team`, `org` and `company` labels (empty when absent); person names, emails and free-form labels never become metric labels.

Owner changes are picked up by reload like any descriptor change.

**Blocked by:** 01 (plugin.yaml replaces plugin.json)

**Status:** ready-for-agent

- [ ] Tests cover parsing every owner field, limits being enforced, and owner-less manifests
- [ ] Integration tests show the owner on a `PanicError` (Go, Lua, TS), on a `PluginError`, in a plugin's log records, and as metric labels via the SDK ManualReader
- [ ] Reload picks up an owner change without a version change
- [ ] Get/All stay at 0 allocations and call benchmarks show no added allocations (attribute sets precomputed per plugin version)
- [ ] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
