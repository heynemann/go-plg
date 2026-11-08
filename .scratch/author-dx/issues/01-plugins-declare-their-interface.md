# 01: Plugins declare the interface they implement

**What to build:** Every plugin says which interface it implements, and registrations take only the plugins that implement theirs. `plugin.yaml` gains a required `implements: <module path>/<package>.<Interface>@<version>` (clean cutover: every testdata plugin, example and doc gains it; a manifest without it is invalid). `Descriptor` carries it.

At Register and reload, a registration loads only plugins whose declared interface is its own; other plugins in the same storage are skipped (status `skipped`, Skipped event, a distinct reason from "language not enabled"). Version rule: same major as the service's contract, minor not newer. The service's contract version comes from its build info (the contract module in `runtime/debug.ReadBuildInfo`); `(devel)` (local checkout, tests) accepts with a warning issue. An incompatible version is rejected with a reason naming both versions.

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] Tests: manifest parsing of `implements` (valid forms, missing, malformed, versions), one storage holding plugins of two interfaces serving two registrations, incompatible major and newer-minor rejections, `(devel)` warning
- [ ] Build-info version detection is tested (including a module replaced locally)
- [ ] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
