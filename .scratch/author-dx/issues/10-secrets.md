# 10: Secrets

**What to build:** Plugins that call third-party APIs get credentials without hard-coding them. `plugin.yaml` declares `secrets: [names]`. `plg.WithSecrets(func(ctx, plg.PluginInfo, name string) (string, error))` per registration resolves them. Guests read only declared names: `secrets.get(name)` in Lua and TypeScript, `plugin.Secret(ctx, name)` in Go (the compiled `plugin` package). Values are resolved on access with a short per-version cache (rotation without reloads); undeclared names, resolver errors and missing resolvers are clear errors. plg scrubs every resolved secret value from logs, errors, panic values and guest output. Test cases supply fakes under `secrets:`.

**Blocked by:** 04 (Test cases)

**Status:** done

- [x] Integration tests in all three languages: declared read, undeclared refused, resolver error, rotation after the cache expires (synctest), redaction of a secret printed by the plugin, returned in an error and in a panic, and test-case fakes
- [x] Guest artifacts rebuilt with `make guests` and committed; `make verify-guests` clean
- [x] README and PLAN.md ("Author developer experience" section plus an implementation-log entry) updated
- [x] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/guests where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket (CI: verified on the PR)
