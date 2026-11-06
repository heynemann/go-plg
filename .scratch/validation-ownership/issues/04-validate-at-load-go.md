# 04: Validate at load, for Go plugins

**What to build:** A registration can require its plugins to validate themselves before they go live. `plg.WithValidation[V any](func(ctx context.Context, info plg.PluginInfo) (V, error))`: `V` is a data type from the contract package, checked at Register like Host types; `PluginInfo` carries key, version, language, storage and owner. An error from the function is an error-severity issue.

Go plugins implement `Validate(ctx context.Context, in contract.V) ([]plugin.Issue, error)` on the instance `Create` returns. `plugin` is a new package `github.com/heynemann/go-plg/plugin` holding the `Issue` type, compiled into the Go guest's symbol tables (allowlisted; `plg build-guest` guests include it).

Detection (from the `ValidateSpike` findings recorded in PLAN.md): the dispatcher binds `Validate` the way it binds `Close` (interface assertion, then assignment to a typed variable under `recover`, which also catches yaegi's extra-leading-parameter false positive); a host-side `go/parser` pass reports the plugin's actual signature against the expected one when binding fails.

At load, after Create succeeds, plg starts a separate throwaway instance (charged to the memory budget, never waiting: no room fails with `ErrMemoryBudget`), calls Validate with the same Host and HTTP policy as a call under `WithValidationTimeout` (default 10 s, never past the caller's ctx), then closes it. `plg.IsValidation(ctx)` tells Host methods (and HTTP logging) it's a validation call. Error-severity issues, an error from Validate, a missing `Validate` or a wrong signature block the load; warnings and info are recorded in the load report.

**Blocked by:** 03 (Ownership checks and load reports)

**Status:** ready-for-agent

- [ ] Integration tests with real Go plugins: valid, warnings only, error issues (blocked Register, kept-last-good reload), Validate returning an error, panicking, looping past the timeout, missing `Validate`, wrong signatures (each with a clear message), the extra-leading-parameter case
- [ ] Validation runs in its own instance (serving instances' state untouched; asserted) and is charged to and released from the budget; no budget room fails with `ErrMemoryBudget` without waiting
- [ ] Host and HTTP work during validation and Host methods see `plg.IsValidation(ctx)`
- [ ] Contract validation rejects a `V` that is not a data type or not from the contract package
- [ ] Guest artifacts rebuilt with `make guests` and committed; `make verify-guests` clean
- [ ] README and PLAN.md ("Validation and ownership" section, plus an implementation-log entry) updated
- [ ] Meets [RULES.md](../../../RULES.md): lint clean (`make fmt-check lint`), happy path and edge cases tested (no test theater), integration tests with real plugins/storages where touched, benchmarks for hot paths touched, containers cleaned up via `t.Cleanup` (§7), `make check` passes, the CI run for the pushed commit is green, one commit titled exactly as this ticket
