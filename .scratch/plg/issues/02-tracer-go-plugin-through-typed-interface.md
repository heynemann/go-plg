# 02: Tracer bullet: a Go plugin called through a typed Go interface

**What to build:** The first end-to-end path. A service creates a container, registers an interface from an in-memory storage, and calls a Go plugin through its own interface type:

- `plg.New()` and `plg.Register[Fooer](ctx, c, "fooer", plg.WithStorage(memoryStorage))` load synchronously and fail fast.
- Interface validation: every method must take `context.Context` first, return `error` last, and use only data types. Anything else is rejected at Register with a message naming the method and the offending type.
- Storage contract: `Name`, `List(ctx) []Descriptor`, `Open(ctx, Descriptor) fs.FS`; descriptors carry Key, Language, Entrypoint, Version. A memory storage implements it.
- One uniform guest call interface, the same one Lua and TS reuse later: guest exports alloc, init (load the entrypoint and run `Create`), call (method, buffer) and close.
- A length-prefixed binary encoding for scalars (bool, ints, floats, string) and errors.
- The Go plugin exports `func Create() (contract.Fooer, error)` and imports the real contract package.
- `plg.Get[Fooer](ctx, c, "fooer/<key>")` returns `ErrNotFound` or `ErrTypeMismatch`; `plg.All[Fooer](ctx, c)` yields an `iter.Seq2[string, Fooer]` in key order.
- The catalogue is an immutable snapshot published atomically (no mutexes anywhere).
- One instance per plugin for now; callers wait on ctx while it's busy.
- The adapter (a struct of `W<Method>` func fields plus forwarding methods) and the embedded contract source are written by hand in tests, in the shape `plg gen` will produce in ticket 04.
- Register fails with a message naming the import when a plugin's language guest isn't linked in.

**Blocked by:** 01 (Go guest artifact + compilation cache)

**Status:** done

- [x] A test registers a Go plugin from a memory storage and calls `Foo(ctx, ...)` through the interface, getting the plugin's result
- [x] Interfaces missing ctx first or error last, or using funcs/channels/interfaces, fail Register with a precise message
- [x] Get returns ErrNotFound for unknown keys and ErrTypeMismatch when the key belongs to another interface
- [x] All iterates in key order over a fixed snapshot
- [x] Registering the same name twice is an error
- [x] A missing guest import produces an error naming the package to import
- [x] Concurrent Get/call under `go test -race` passes
- [x] Meets [RULES.md](../../../RULES.md): `golangci-lint` clean, unit tests for the happy path and every edge case (no test theater), integration tests with real storages/plugins where the behavior touches them, and benchmarks for any hot path touched
