# 08: Typed Host interface

**What to build:** Plugins can call back into the service through a typed Host interface declared in the contract package (for example `Lookup(ctx, id string) (User, error)`). It follows the same rules as plugin interfaces: ctx first, error last, data only. The service passes `plg.WithHost[contract.Host](impl)` to Register, and the host calls `impl` by reflection. For Go plugins, plg synthesizes Go stub source for the Host interface at Register and the guest interprets it, so `func Create(host contract.Host) (contract.Fooer, error)` receives a working typed host; `Create()` without a parameter remains valid when no Host is registered. Host methods receive the ctx of the plugin call in progress.

**Blocked by:** 03 (Full data-type encoding)

**Status:** done

- [x] A Go plugin calls a Host method and gets typed results, including structs
- [x] Values stored in the plugin call's ctx are visible inside the Host implementation
- [x] Invalid Host interfaces fail Register with precise messages
- [x] Errors returned by Host methods reach the plugin as errors
- [x] A panicking Host implementation returns a PanicError (Phase Host) to the plugin call without crashing (recovered on the host and returned to the plugin as an error through the `hostPanic` seam; the `*PanicError` type itself is ticket 07's, see PLAN.md)
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
