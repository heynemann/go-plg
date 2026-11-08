# 03: Contract directives: //plg:limits and //plg:guest-packages

**What to build:** A contract package can state, on its interface's doc comment, the limits its plugins should be tested under (`//plg:limits memory=64MiB timeout=2s pool=1..8`) and extra Go guest packages its Go plugins may import (`//plg:guest-packages encoding/xml`). `plg gen` validates the directives (unknown keys, malformed values are generation errors). `plg validate` runs plugins under the declared limits (printing the limits used) and, when guest packages are declared, builds the custom Go guest (as `plg build-guest` does) and caches it by content so later runs are instant.

**Blocked by:** 02 (plg validate: resolve the contract and load the plugin offline)

**Status:** done

- [x] Tests: directive parsing (each key, units, errors), validate under a tight memory limit failing a plugin that exceeds it, a Go plugin importing a declared extra package passing and failing without the directive, the custom guest cache being reused
- [x] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket (CI run 42472553664 for `00ff89c`, which contains `8e4b74e` and its acceptance fixes `135cd03`: success; earlier runs on `8e4b74e` and `135cd03` failed on a test timeout fixed in `079f35d` and on a runner Docker daemon fault)
