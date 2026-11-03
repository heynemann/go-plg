# plg — sandboxed plugin system for Go services

Module `github.com/heynemann/go-plg`, package `plg`, Go ≥ 1.27.

Plugins written in **Go, Lua and TypeScript** are loaded **as source** from pluggable storages and called through ordinary Go interfaces. Every plugin runs inside **WebAssembly** ([wazero](https://github.com/tetratelabs/wazero), pure Go, no cgo):

| Plugin language | Interpreter inside WASM |
|---|---|
| Go | [yaegi](https://github.com/traefik/yaegi) compiled to wasip1 |
| Lua | C Lua 5.4 built with wasi-sdk |
| TypeScript | QuickJS-ng built with wasi-sdk; [esbuild](https://github.com/evanw/esbuild) transpiles on the host |

Plugin authors never run a build step.

**Stability is the top priority.** A plugin must never be able to take down the host process. Out-of-memory, panics, stack overflow, runaway CPU and data races inside a plugin all become errors on that plugin's call; the faulty instance is discarded.

Engineering rules (strict linting, testing, benchmarks, concurrency, JSON) are in [RULES.md](RULES.md); every ticket must meet them.

## Public API

```go
import (
    "github.com/heynemann/go-plg"
    _ "github.com/heynemann/go-plg/guest/golang"     // only the guests you use
    _ "github.com/heynemann/go-plg/guest/lua"
    _ "github.com/heynemann/go-plg/guest/typescript"
)

c := plg.New(
    plg.WithLogger(logger),
    plg.WithMeterProvider(mp),
    plg.WithMemoryBudget(4 << 30),
    plg.WithCompilationCacheDir("/var/cache/plg"), // default: os.UserCacheDir()/plg/<wazero version>
)
defer c.Close(ctx)

err := plg.Register[contract.Fooer](ctx, c, "fooer",
    plg.WithStorage(plg.NewFSStorage("/srv/plugins/fooer")),
    plg.WithGolangPlugins(), plg.WithLuaPlugins(), plg.WithTypescriptPlugins(), // none = all
    plg.WithPool(1, 64),
    plg.WithMemoryLimit(64 << 20), // per instance
    plg.WithPollInterval(30*time.Second),
    plg.WithHost[contract.Host](hostImpl),
    plg.WithHTTP(plg.HTTPPolicy{AllowedHosts: []string{"api.example.com"}}),
)

f, err := plg.Get[contract.Fooer](ctx, c, "fooer/my-own-fooer")
bar, err := f.Foo(ctx)
for key, f := range plg.All[contract.Fooer](ctx, c) { /* key order */ }

err = c.Reload(ctx)                        // every registered interface
err = plg.ReloadInterface(ctx, c, "fooer") // one interface
```

| Function | Semantics |
|---|---|
| `New(opts...)` | Container-wide options: `WithLogger` (slog; discard by default), `WithMeterProvider` (OTel metric API; global by default), `WithMemoryBudget`, `WithCompilationCacheDir`, `WithOnPanic`, `WithPanicHistory(n)` (default 64). |
| `Register[T](ctx, c, name, opts...) error` | Loads synchronously and fails fast. Validates `T` (and `Host`), lists the storages, fetches, compiles, warms `min` instances, then publishes a new snapshot atomically. Registering the same `name` twice is an error. Fails with a message naming the import if a needed guest package isn't linked in. |
| `Get[T](ctx, c, key) (T, error)` | `key` = `<name>/<descriptor key>`. Errors: `ErrNotFound`, or `ErrTypeMismatch` if the key belongs to another interface. 0 allocations, ≤ 50 ns. |
| `All[T](ctx, c) iter.Seq2[string, T]` | Plugins implementing `T`, in key order, over a fixed snapshot. 0 allocations. |
| `c.Reload`, `ReloadInterface` | Mirrors the storage catalogue (see Lifecycle). |
| `c.Close(ctx)` | Stops pollers and refillers, drains in-flight calls, closes instances and runtimes. |

### Interface contract (checked by reflection at Register, for both `T` and `Host`)

- **Method shape:** every method is `M(ctx context.Context, args...) (results..., error)`. ctx first and error last are both required.
- **Allowed types:** data only: bool, integers, floats, string, `[]byte`, slices, string-keyed maps, structs, struct pointers, `time.Time`, error. Funcs, channels and interfaces are rejected.
- **Big integers:** values Lua/JS numbers can't represent exactly (|n| > 2^53) fail with an error rather than losing precision.
- **Name mapping in Lua/TS:** method names stay exact. Struct fields use the `json` tag name, otherwise the Go field name.

## Storage

```go
type Storage interface {
    Name() string // used only in logs and metrics
    List(ctx context.Context) ([]Descriptor, error)
    Open(ctx context.Context, d Descriptor) (fs.FS, error)
}

type Descriptor struct {
    Key         string   // unique within one Register call
    Language    Language // LanguageGo | LanguageLua | LanguageTypeScript
    Entrypoint  string   // path inside the fs.FS returned by Open
    Version     string   // change detection; empty → content hash
    MemoryLimit uint64   // per-instance bytes; 0 → the Register limit, which it may only lower
}
```

- **Scope:** storages belong to one Register call. Every plugin they list implements that interface.
- **Built-in storages:** memory (`NewMemoryStorage`), filesystem directory (`NewFSStorage(dir)`), and any `io/fs.FS` (`NewFSStorageFromFS(name, fsys)`). S3 (aws-sdk-v2) and Postgres (pgx) are provided only as `examples/`.
- **Built-in layout:** one directory per plugin containing `plugin.json` with `{"language", "entrypoint", "version"?, "memoryLimit"?}`. The key is the directory name. `memoryLimit` may only *lower* the Register limit.
- **Multi-file plugins:** the plugin's fs.FS is mounted read-only into the guest at `/plugin`.
  - yaegi compiles the package directory.
  - Lua `require` resolves only inside `/plugin`.
  - esbuild bundles TS imports from the fs.FS on the host.
- **Duplicate keys** across a Register's storages: an error at Register; on Reload, keep the previous version and log.
- **Languages not enabled:** a plugin in a language the Register didn't enable is skipped, logged and counted; not an error.

## Codegen (service developer only)

The host still needs one compiled adapter per interface, because Go can't create a type with methods at runtime. The service adds one line next to the interface:

```go
//go:generate go run github.com/heynemann/go-plg/cmd/plg gen Fooer
```

It generates:
- **The adapter for `Fooer`:** `struct{ W<Method> func(...) }` plus forwarding methods, with a compile-time check (`var _ Fooer = ...`) so a changed interface fails the build until you regenerate. Its `init()` registers the adapter with plg.
- **The contract package's source,** embedded with `go:embed` (non-test files, excluding the generated file). The generic Go guest interprets this source, so Go plugins can `import` the real contract package. The contract package may import only allowlisted packages; `plg gen` checks this.

Not generated:
- **Host dispatch:** the host calls `Host` methods by reflection.
- **Go guest stubs for `Host`:** plg synthesizes this Go source at Register and the guest interprets it.

Other commands:
- **`plg warm`:** compiles every linked guest into the compilation cache. Run it in the Docker build so containers start warm. Cold compile: yaegi guest ≈ 11–15 s, QuickJS/Lua ≈ 0.1–0.5 s; from cache ≈ 0.02–0.3 s.
- **`plg build-guest -pkg encoding/xml -o guest.wasm`:** builds a custom Go guest with extra packages, passed in via `WithGoGuest(embedded)`. The service takes on that build and its cold compile.

## Architecture

```mermaid
flowchart LR
  Caller -->|"f.Foo(ctx)"| Adapter["generated adapter<br/>(W funcs = generic binder)"]
  Adapter --> Binder["binder: encode args,<br/>acquire instance, call, decode"]
  Binder --> Pool["pool per plugin<br/>(min/max, background refill)"]
  Pool --> Inst["wazero instance<br/>guest: yaegi | Lua | QuickJS<br/>+ plugin loaded + Create() done"]
  Inst -->|"host_call"| Host["Host impl (reflection)"]
  Inst -->|"http_roundtrip"| HTTP["HTTP policy + client"]
  Inst -->|"log"| Slog["slog"]
```

**One guest ABI for all three languages,** so the host side is the same for every language:
- **Guest exports:** `plg_alloc`, `plg_init` (load the entrypoint, run `Create`), `plg_call(method, ptr, len)`, `plg_close`.
- **Host imports:** `plg.host_call`, `plg.http_roundtrip`, `plg.log`.

**Codec:** our own length-prefixed little-endian binary format.
- Host and Go guest use encoder/decoder plans built once per type.
- The Lua and QuickJS glue (C) build tables and objects directly.

**Plugin entry points:**

| Language | Entry |
|---|---|
| Go | `func Create(host contract.Host) (contract.Fooer, error)`, or `func Create() (contract.Fooer, error)` when no Host is registered |
| Lua | `function Create() return { Foo = function(self, ...) end } end`, with `host` and `http` as globals |
| TS | `export function Create() { return { Foo(...) {} } }`; methods may be `async`; `host` and `fetch` are globals |

ctx is never visible to scripts. The host uses it to enforce deadlines and passes it to Host and HTTP calls.

**Guest capabilities:**
- **WASI:** read-only `/plugin` mount, monotonic and wall clocks, random. No filesystem beyond `/plugin`, no env, no sockets.
- **stdout/stderr** are captured per instance (bounded) and sent to slog with the plugin key.

**Go guest stdlib allowlist** (fixed in the prebuilt guest; the list lives in `internal/goallow/packages.txt`):
`bufio bytes context encoding/base64 encoding/hex encoding/json errors fmt io maps math math/* net/http (client only, via host transport) net/url regexp slices sort strconv strings sync sync/atomic time unicode/*`, plus the contract source and the synthesized `Host` stubs.

**Goroutines are banned in Go plugins.** Plugin source is parsed with `go/parser` before loading, and any `go` statement is rejected. Reasons, observed in probes:
- A WASM guest has no parallelism.
- Guest goroutines are paused between calls (a background counter advanced 0 while the host was idle).
- They freeze during host calls.

Parallelism comes from the instance pool.

**Lua/TS libraries:**
- Lua: `base` (minus file loading), `string`, `table`, `math`, `utf8`, `coroutine`, plus a bundled in-guest `json` module (`json.encode`/`json.decode`).
- QuickJS: ECMAScript built-ins only (no `std`/`os` modules), plus `console`, `host` and `fetch`.
- TS async: when a method returns a Promise, the host runs QuickJS's pending jobs until it settles or ctx ends.

## Resource limits

- **Memory:**
  - **Per instance:** `WithMemoryLimit` per Register, rounded down to whole 64 KiB pages. Defaults: Lua/TS 64 MiB, Go 128 MiB (yaegi's baseline is ≈ 29 MiB). The limit is enforced by wazero's `WithMemoryLimitPages`. A descriptor's `MemoryLimit` (plugin.json `memoryLimit`) may lower it; asking for more is capped at the Register limit, with a warning.
  - **One runtime per limit:** one wazero runtime per distinct limit in the container, shared by every Register and language using it, all sharing the compilation cache.
  - **Container budget:** `WithMemoryBudget(total)`. Register fails if the worst case (Σ plugins × pool max × limit, over every plugin registered) exceeds it, and pools stop growing once the budget is reached.
- **CPU:** `WithCloseOnContextDone` is always on. Every call is stopped preemptively when its ctx ends; that instance is then closed and replaced. Cost ≈ 2 µs per call plus a 1.5–4× slowdown of guest code. This is accepted.
- **Pool:** `WithPool(min, max)`, `1 ≤ min ≤ max ≤ 4096`, default `WithPool(1, GOMAXPROCS)`.
  - **Background refill:** a goroutine per plugin keeps `min` instances warm. Callers take an idle instance; they create one only while under `max` and the budget; otherwise they wait on ctx.
  - **Failing `Create()`:** a `Create()` that fails during refill backs off exponentially (100 ms doubling to 30 s) and is logged at Warn.
  - **Fresh instances:** each pool member is a separate instance with its own `Create()` result, so state is per instance.
- **Instance discard:** an instance is discarded on a trap, deadline, panic, or codec/ABI failure. It's kept when the plugin returns an ordinary error.
  - Reason: wazero leaves a trapped instance callable, and it then answers with `err=nil` (observed), so trapped instances must never be reused.
- **Call depth:** Lua (`LUAI_MAXCCALLS`) and QuickJS (`JS_SetMaxStackSize`, linked with a larger wasm stack) turn deep recursion into ordinary errors. Go recursion hits the memory limit and traps.

## HTTP (built in, off by default)

`WithHTTP(plg.HTTPPolicy{...})` per Register. Default values once enabled:
- `AllowedHosts` (required)
- `BlockPrivateNetworks: true` (SSRF protection)
- `Timeout: 10s` (and never later than the call's ctx deadline)
- `MaxRequestBytes: 1 MiB`, `MaxResponseBytes: 4 MiB`
- `MaxRequestsPerCall: 16`

How each language reaches it:
- **Go plugins** use the standard `net/http` client: the guest sets `http.DefaultTransport` to a transport that calls the host. Proven: `http.Get` from yaegi inside wazero → `200 OK`; a host not on the allowlist → `403 Forbidden host not allowlisted`.
- **TS** uses `fetch()`; **Lua** uses `http.request{...}`.
- **Every request** is logged and counted in metrics (status, latency, bytes) by plugin key.

Requests block the instance for their duration (one call per instance), so size pools for I/O: Lua/TS instances are 0.2–0.5 MiB; Go instances are ≈ 29 MiB.

**gRPC:** for now, expose gRPC-backed operations as typed `Host` methods implemented with your own grpc-go clients. A generic gRPC capability will be designed when it's needed.

## Concurrency and performance

- **Snapshot:** the catalogue is an immutable snapshot behind `atomic.Pointer`. Register and Reload publish with compare-and-swap. **No mutexes anywhere**; shared mutable state uses `sync/atomic`, `xsync/v4` or channels.
- **Get/All:** `Get` is a map lookup plus a type assertion; `All` iterates a pre-sorted slice per type. Neither allocates.
- **Measured per call (spike, wazero v1.12.0, go1.27.3, CPU cap on / off):**

| Guest | Per call | Memory per instance | Instantiate |
|---|---|---|---|
| QuickJS-ng | 3.1 / 1.05 µs | 0.5 MiB | 1.7 ms |
| C Lua 5.4 | 7.5 / 3.7 µs | 0.2 MiB | < 1 ms |
| yaegi | 78 / 19 µs | ≈ 29 MiB | 38–59 ms |

- **Budgets:** Get/All 0 allocations and Get ≤ 50 ns, enforced in CI. Call costs are tracked with benchstat but don't fail the build.

## Lifecycle

- **Register:** any failure returns an error. Nothing is published.
- **Reload / polling** (`WithPollInterval` per Register; unchanged plugins are skipped by version or hash):
  - Removed from the catalogue: removed from the snapshot.
  - New but broken: skipped, logged, counted.
  - Updated but broken: the last good version stays, logged, counted.
  - Duplicate keys: the previous version stays, logged.
- **Draining:** each plugin generation keeps an atomic count of calls in flight. When it reaches 0 after replacement or removal, `plg_close` runs (calling the plugin's optional `Close`) and the instances are freed.

## Stability and panic containment

**Inside the guest:**
- **Traps:** out-of-memory, stack overflow, a Go runtime abort, or a deadline become errors on that call. The instance is discarded. Other instances and the host are unaffected (observed for every case in the spike).
- **Go plugin panics** are recovered by the guest at the call boundary and returned as `*plg.PanicError`. That instance is discarded anyway.

**On the host:** every host-side boundary runs behind `recover()`:
- Storage `List`/`Open`
- esbuild
- the codec
- Host method dispatch
- HTTP policy/transport
- pollers and refillers
- user callbacks

**Reporting:** every failure is a `*plg.PanicError` you can extract with `errors.As`:

```go
type PanicError struct {
    Key      string
    Language Language
    Phase    Phase  // Load | Create | Call | Host | HTTP | Storage | Close | Poll | Refill | Callback
    Method   string
    Value    any    // recovered value or trap message
    Stack    []byte // host Go stack (host-side failures)
    Guest    string // guest traceback: Go runtime/yaegi, Lua traceback or JS stack, from captured stderr
    Time     time.Time
}
```

It is:
- logged at Error level;
- counted in the `plg.plugin.panics{key,phase}` metric;
- passed to `WithOnPanic` (itself guarded by recover);
- kept in a bounded, lock-free history (a ring of `atomic.Pointer`) available through `c.RecentPanics()`.

**Residual risks:**
- Host memory is ordinary Go heap: after a large guest closes, RSS stays high until the Go scavenger returns memory (observed).
- Lua's error handling depends on wazero's *experimental* exception-handling feature. Pinned and covered by tests.
- Untested: whether yaegi can interpret typical contract packages, and whether the reflection codec works on interpreted types. This is the first task in M5; if it fails, the fallback is a per-contract guest built by `plg gen`, which the spike proved works.

## Observability

- **slog:** loads, reloads, skips, failures, panics and traps, pool exhaustion, budget refusals, HTTP requests, guest stdout/stderr.
- **OTel metrics** (`go.opentelemetry.io/otel/metric` API only), labelled by plugin key:
  - calls, latency, errors, panics/traps;
  - pool in-use, waits, refills;
  - instance memory;
  - loads and reload failures;
  - HTTP requests, latency, bytes.

## Package layout

- **Core:** `github.com/heynemann/go-plg` (package `plg`, flat).
- **Guests:** `github.com/heynemann/go-plg/guest/{golang,lua,typescript}`, each embedding its `.wasm` and registering itself with a blank import. The `.wasm` files are committed, built by `make guests` with pinned wasi-sdk/Go versions, and checked in CI for reproducible builds.
- **Guest sources:** `guestsrc/` (C glue for Lua/QuickJS, Go sources for the yaegi guest).
- **CLI:** `cmd/plg` (`gen`, `warm`, `build-guest`).
- **Examples:** `examples/` (S3 storage, Postgres storage, an end-to-end service).

## Milestones

Each milestone is implemented, tested with `go test -race`, smoke-run against real plugins, and committed separately.

1. **M1 Core:** Container, snapshot, `Register`/`Get`/`All`, interface and Host validation, `Storage`/`Descriptor`, memory/FS/fs.FS storages, `plugin.json`, errors, `PanicError` and the recover helpers on the host.
2. **M2 WASM host:**
   - Runtime management: memory limits, CPU cap, compilation cache.
   - The guest ABI, codec, binder, pool with background refill, memory budget.
   - Host interface dispatch and the HTTP capability on the host side.
   - Instance discard rules and trap → `PanicError` mapping.
3. **M3 TypeScript guest:** QuickJS-ng build plus glue, esbuild bundling from fs.FS, async methods (job loop), `fetch`, `console`, call depth limit.
4. **M4 Lua guest:** C Lua 5.4 build (wasm exception handling), sandboxed `require`, `http.request`, call depth limit.
5. **M5 Go guest:**
   - First, prove that yaegi can interpret contract source.
   - Generic yaegi guest with stdlib allowlist and host transport for net/http.
   - `go` statement rejection, guest-side panic recovery, synthesized Host stubs.
   - `plg gen` and `plg build-guest`.
6. **M6 Lifecycle:** Reload, polling, last-good handling, draining, `plg_close`, `c.Close`.
7. **M7 Observability:** slog/OTel wiring, panic history, `plg warm`, benchmarks with Get/All budgets enforced and call costs tracked.
8. **M8 Fast paths:** generated codec plans and direct binders, kept only where benchmarks show the gain.
9. **M9 Multiplexed async I/O:** TS and Lua instances serve other calls while a `fetch`/`http.request` is pending. Kept only if benchmarks of I/O-heavy plugins show the memory and density win.

## Known risks

- Go plugins are ≈ 80× slower per call and ≈ 60× heavier per instance than Lua/TS in this design. This was accepted for full isolation.
- wazero's exception-handling feature (needed by Lua) is experimental.
- The yaegi guest's cold compile (≈ 11–15 s) depends on the disk cache and `plg warm` for fast startups.
- The guest `.wasm` artifacts must be rebuilt and re-verified whenever wazero, wasi-sdk, Lua, QuickJS, yaegi or Go are upgraded.

## Decisions and measurements (implementation log)

### Ticket 01: Go guest artifact and compilation cache

- **Contract-source interpretation works; no fallback needed.** Inside the committed wasip1 artifact running in wazero v1.12.0, yaegi v0.16.1 interprets the contract package's source at its real import path (`github.com/heynemann/go-plg/internal/testcontract`, an `internal/` path: yaegi does not enforce Go's internal-import rule) and a plugin package importing it, and calls through a host-rendered dispatcher (`internal/engine` integration test `TestGoGuestInterpretsContractSource`). Plugins may be `package main` (the dispatcher imports them under an alias); yaegi then runs their `main()` at load, like an `init`.
- **Artifact:** `guest/golang/golang.wasm`, 14.5 MB raw (3.3 MB gzipped), so it is committed uncompressed (the gzip threshold was ~20 MB). Built by `make guests` with `-trimpath -buildvcs=false -buildmode=c-shared` and `GOTOOLCHAIN=go1.27.3`; `make verify-guests` (CI) rebuilds it and the symbol tables and fails on any diff.
- **Compile times (Go guest, wazero compiler, linux/amd64, measured by `TestCompileCacheAcrossProcesses` in two fresh non-race processes):** cold ≈ 2.9–3.7 s, warm from the disk cache ≈ 0.12–0.14 s. Under `-race` the same compile is ≈ 33 s cold / 0.7 s warm, which is why that test builds its helper without the race detector. (The 11–15 s quoted earlier was for a guest with the full stdlib.)
- **Stdlib allowlist** is extracted with `yaegi extract` for GOOS=wasip1 into `guestsrc/golang/symbols` (`make symbols`). `cmp` is available too, because yaegi compiles it from source for its generic `maps`/`slices`. `encoding/json.Options` is excluded: its method takes an internal type, so no wrapper can be generated.
- **yaegi v0.16.1 workarounds, both in the guest:** (1) methods with *unnamed* parameters (`func (f *T) M(context.Context) ...`) read their receiver from the wrong frame slot and panic, so `internal/goguest` rewrites every unnamed parameter to `_` as it reads sources (line numbers unchanged); (2) yaegi does not type-check that `Create`'s result implements the interface and its run-time assertions on interpreted interfaces are unreliable, so the dispatcher assigns `plugin.Create` to a `func() (contract.Fooer, error)` variable (a wrong signature fails at load) and reports a nil instance as a load error.
- **wazero v1.12.0 data race:** `internal/version` caches the module version unsynchronized on first use, so the first concurrent `NewCompilationCacheWithDir`/`NewRuntime` calls race. `engine.New` resolves it once per process first (`primeWazeroVersion`).
- **Guest ABI as built:** `plg_alloc(size) ptr`, `plg_init(len) packed`, `plg_call(method, len) packed`, `plg_close() packed`; results are `ptr<<32|len` of a status-prefixed buffer (statuses OK, Error, Panic, Load, ABI; zero is invalid). Messages are capped at 64 KiB. Panic and ABI statuses, traps, deadline stops and malformed buffers mark the instance broken.
- **Lint config:** `exhaustruct_v5` (golangci-lint 2.14's successor of the already-disabled `exhaustruct`) is disabled for the same reason.

### Ticket 02: tracer bullet (a Go plugin through a typed interface)

- **Contract registration:** `plg.Contract{Interface, Adapter, Source}` (the import path is `Interface.PkgPath()`), registered with `RegisterContract` (returns an error) or `MustRegisterContract` (panics; what `plg gen`'s `init()` will call). It validates the interface, requires one `W<Method>` field per method of exactly the method's type (parameter names are irrelevant to type identity, so generated fields may omit them) and a pointer that implements the interface, snapshots `Source` (at least one `.go` file at its root) and builds, once per contract, the rendered dispatcher, the Go init payload and the per-method codecs. `internal/testcontract/fooer_plg.go` is the hand-written stand-in for ticket 04's output. A contract whose types the codec cannot encode yet (slices, maps, structs) is rejected until ticket 03.
- **Interface validation** runs at Register before the contract lookup, so the precise message wins over "no contract". Data types are bool, integers, floats, string, `time.Time`, and slices, string-keyed maps, structs with only exported fields and struct pointers built from them (recursive types allowed). `error` and `context.Context` are accepted only as the last result and the first parameter; variadic methods are allowed (the variadic parameter is a slice). Arrays, complex numbers and `uintptr` are rejected along with funcs, channels and interfaces.
- **Errors:** exported sentinels (`ErrNotFound`, `ErrTypeMismatch`, `ErrClosed`, `ErrInvalidInterface`, `ErrInvalidContract`, `ErrNoContract`, `ErrInvalidName`, `ErrDuplicateName`, `ErrNoStorage`, `ErrInvalidDescriptor`, `ErrDuplicateKey`, `ErrUnsupportedLanguage`, `ErrGuestNotLinked`, `ErrLoad`, `ErrCall`) wrapped with details. A plugin's own error is a `*PluginError` whose `Error()` is exactly the plugin's message; failures outside the plugin's code (trap, deadline stop, guest panic, ABI violation) wrap `ErrCall` and discard the instance. `ErrCall` is what ticket 07 refines into `*PanicError`.
- **Get/All type identity:** `Get[T]` compares the plugin's registered interface type with `T` before asserting, so an interface with the same method set (which the adapter would satisfy) is still `ErrTypeMismatch`.
- **All's snapshot is taken when the iteration starts**, not when `All` is called: `All` must stay small enough to inline (with the race detector too), otherwise the compiler cannot see that the loop body does not escape and every `range` allocates. Coverage instrumentation defeats that inlining, so the allocation test skips under `-cover` and `make allocs` (part of `make check`) runs it uninstrumented, with `-race`.
- **Names** are non-empty, contain no `/`, and are unique per container across interfaces; they are reserved (xsync.Map) before loading, so concurrent duplicates fail fast, and released when a Register fails. Register lists every storage and checks every key, entrypoint and duplicate before resolving guests, then loads plugins one by one; nothing is published unless all load. A Register racing `Close` returns `ErrClosed` and closes what it loaded.
- **Languages:** only `LanguageGo` exists so far; any other language is `ErrUnsupportedLanguage` until its guest ticket adds it. A Go plugin whose guest is not linked fails with `ErrGuestNotLinked` naming `import _ "github.com/heynemann/go-plg/guest/golang"`. Guest linking is per program, so the not-linked and broken-guest cases are test-only packages (`internal/noguest`, `internal/brokenguest`).
- **Go entrypoint:** a package directory, or a `.go` file in it; every file below that directory is copied into the plugin's immutable source tree (`src/plg.local/plugin`, next to the contract source and the dispatcher), shared by all of the plugin's instances.
- **Runtime and module:** one wazero runtime per container with the Go limit of 128 MiB (ticket 06 makes it one per distinct limit); each guest module is compiled once per container by the first Register needing it (concurrent Registers wait; a failed compile is forgotten and retried).
- **Pool (size 1):** a buffered channel of slots; a caller takes a slot (waiting on its ctx, or failing with `ErrClosed`), spawns an instance into it if it is empty, and gives it back. A broken or discarded instance is closed and its slot left empty, so the next caller spawns a fresh one with a fresh `Create`. Each slot owns a reusable argument buffer. `Close` takes every slot back (waiting on its ctx for calls in flight) and closes the instances.
- **testcontract** gained `Spin(ctx, n)` (a deadline stops it preemptively in tests) and the corpus a `createpanics` plugin.
- **Measured (go1.27.3, linux/amd64, 32 threads, no race detector):** `Get` 10.7 ns/op, `All` over two plugins 17.7 ns/op, both 0 allocs (parallel: ~1 ns/op); one `Foo` call into a Go plugin ≈ 244 µs/op, 10 allocs, almost all of it inside the guest (yaegi interpreting the dispatcher, the plugin and the contract code), with every protection on.

### Ticket 03: full data-type codec

- **Wire format** (documented in `internal/wire`'s package doc, pinned by layout tests): integers of every kind are 8-byte words, but only |n| ≤ 2^53 crosses; slices and maps carry a uint32 count with `0xFFFFFFFF` = nil (nil and empty stay distinct, also for `[]byte`, which is raw bytes, not words); map entries are key string + value, in map iteration order, and a decoder rejects duplicate keys; pointers are a presence byte; structs are their fields in declaration order with no names; `time.Time` is Unix seconds + nanoseconds (0–999999999), the instant crosses and decodes in UTC (location and monotonic reading dropped; seconds also obey ±2^53). A struct field tagged `json:"-"` is not encoded and decodes as zero. Nesting is capped at `MaxDepth` = 10000 levels (a pointer cycle fails with `ErrTooDeep` instead of overflowing the stack, and so does a hostile encoding); a length prefix is checked against the remaining input (count × minimum element size) before anything is allocated. Slices whose elements encode as nothing (`[]struct{}`) are rejected at plan time, since no input could bound their length.
- **Big integers:** encoding and decoding fail with `wire.ErrPrecision` beyond ±2^53 (2^53 itself crosses: it is exact in a float64), on both sides, for parity with Lua/JS numbers. Errors name the location: `parameter 1: Lines[1].Qty: wire: integer beyond ±2^53: 9007199254740993` (fields by Go name, `[i]` indexes, `["key"]` map keys). On the host, an argument that cannot be encoded and a result the guest cannot encode both fail with the new sentinel **`plg.ErrConversion`**, naming key, method, parameter/result and field; the instance is kept. The guest reports the latter with the new status **`StatusConvert`** (6).
- **Field naming = `json` tag name, else the Go name**; an embedded struct is one field named after its type (not flattened like encoding/json: yaegi's interpreted types do not even mark embedded fields as anonymous). Two fields with the same name are rejected by the codec, so `RegisterContract` fails with `ErrInvalidContract`.
- **Schema for Lua/TS (`wire.Schema`):** built from the codec plans; a type table (`typeCount`, then per type a kind byte + payload: bit size for ints/uints/floats, element index for slices/maps/pointers, field count + (name, type index) per encoded field for structs) followed by the methods (name, parameter type indexes, result type indexes; ctx and the final error excluded, as on the wire). Counts and indexes are uint32, names are wire strings, indexes make recursive types finite. Layout pinned by `TestSchemaLayout`; tickets 12/13 send it in the Lua/TS init payload.
- **Plans once per type:** `wire.Planner` memoizes plans by `reflect.Type` (recursive types share one node; a failed `For` forgets everything it planned); one planner per contract on the host (at `RegisterContract`) and per instance load in the guest, so calls never plan. Evidence: encoding `SampleOrder`-like structs and decoding fixed-size structs allocate 0 (`TestEncodingAndFixedSizeDecodingDoNotAllocate`, run by `make allocs`). The guest decodes into yaegi's interpreted types by reflection (verified: tags and field order are preserved, recursive types are self-identical); variadic methods are called with `CallSlice`.
- **Validation fix:** `checkData` looped forever on recursive slice/map types (`type L []L`, `type T map[string]T`); it now tracks every visited type.
- **Test contract:** `Shaper` (Echo/EchoPtr/Inspect/Bump/Grow/Index/Reverse/Later/Sum over `Order`, `Line`, `Point`, `Tree`, `[]byte`, `time.Time`, variadic) in `internal/testcontract/shapes.go`, with `SampleOrder`/`EmptyOrder` fixtures; plugin `testdata/plugins/go/shaper`. Fuzz targets: `FuzzDecode` (scalars, canonical bytes) and `FuzzDecodeComposites` (every composite shape; no panic, re-encodes to the same length and decodes again).
- **Per-call cost investigation.** Profiles: host pprof of `BenchmarkCallGo` shows ~80% of CPU in JIT-compiled guest code and wazero's call loop; host-side plg code is ~2–3 µs of the call. Guest-side timing (temporary instrumented artifact, `time.Now` around each phase of `plg_call`): decode 6 µs, the yaegi call 204 µs, encode 2 µs, host + transitions 25 µs. Native yaegi runs the same call in 2.75 µs: the wasm penalty is a near-constant ≈ 88× here and in the spike (913 ns native → 81.5 µs), so the gap to the spike was more interpreted work per call, not the host. Found and fixed: the dispatcher forwarded each call through an interpreted `PlgCall_<M>` function calling `inst.M(...)` on an interface, i.e. a second interpreted frame plus yaegi's dynamic method lookup (`getMethodByName`). The dispatcher now binds the instance's method values once in `PlgInit` (`var PlgCall_M func(...)`) and the guest calls them directly: native 2.75 µs/62 allocs → 1.5 µs/31 allocs; in wasm a trivial method (`Count`) went from 107 µs to 35 µs. A method yaegi cannot bind (one promoted from a nil embedded interface, i.e. a plugin implementing part of the interface) falls back to forwarding through a recover-guarded binder and panics only when called, as in Go (`TestPartialImplementationPanicsOnlyWhenCalled`).
- **Result:** `BenchmarkCallGo` (Foo, every protection on) **244 µs → 117–143 µs/op, 10 allocs** (unchanged; 3 of them are wazero's per-call ctx watcher goroutine). New `BenchmarkCallGoStruct` (Echo of `SampleOrder`: nested slices, maps, pointers, bytes, time): ≈ 270–290 µs/op, 41 allocs; the extra ≈ 150 µs is the guest-side reflection codec (natively 0.6 µs encode + 1.3 µs decode, `BenchmarkCodec`), times the same wasm factor.
- **What remains is inherent to the CPU cap.** With `WithCloseOnContextDone(false)` (measurement only) the same call costs 21.8 µs (5.4× less); a Go loop iteration costs 21 ns vs 0.7–1.1 ns, an allocation 370–550 ns vs 40–130 ns. wazero v1.12's `ensureTermination` inserts at every wasm loop header a call to `check_module_exit_code_trampoline`, which exits native code to Go (`ExitCodeCheckModuleExitCode` → `FailIfClosed` → re-entry via `afterGoFunctionCallEntrypoint`), so the Go scheduler can preempt; Go's wasm backend wraps every function body in a loop (for goroutine resumption), so every guest function call pays a native→Go→native round trip. perf with wazero's `perfmap` build tag attributes 8 % to the trampoline and ≈ 25 % to the Go-side transition (`afterGoFunctionCallEntrypoint`, `callWithStack`). Dropping the cap would break the stability guarantee (and native code that never yields also blocks the host's stop-the-world GC). The rest of the spike gap is the generic guest's `reflect.Value.Call` into yaegi's `MakeFunc` wrapper (10–20 µs in wasm vs a typed call, which only a per-contract guest can make: ticket 16) and the plugin's own interpreted work (`Greeting`, an interpreted contract function).

### Ticket 04: `plg gen`

- **Command:** `cmd/plg` is one statement, `os.Exit(cli.Main(os.Args[1:], ".", os.Stderr))`; `internal/cli` dispatches commands (ticket 11 adds `warm` and `build-guest` there) and `internal/gen` generates. Exit codes: 0 ok, 1 failure, 2 usage. `plg gen A B ...` writes one file, `<first name in lower case>_plg.go`, for all named interfaces, starting with `// Code generated by plg gen. DO NOT EDIT.`. It never overwrites a file without that header and leaves an identical file untouched (no needless rebuilds).
- **Generated code:** per interface, `type plg<Iface> struct{ W<Method> func(<param types>) <results> ... }` (methods in go/types order, i.e. sorted like reflect's), forwarding methods with parameters named `p0, p1, ...` (interface parameter names could clash with the file's imports), `var _ Iface = (*plg<Iface>)(nil)` so a changed interface fails `go build` until regenerated; per file, `//go:embed *.go` into `plg<First>Source` and an `init()` calling `plg.MustRegisterContract` for each interface. Imports and package-level names avoid every identifier the package declares (deterministic numeric suffixes: `context2`, `plgFooer2`); an import is aliased when its name is not its path's last element (`plg "github.com/heynemann/go-plg"`, `rand2 "math/rand/v2"`).
- **Source handed to the guest:** ticket 02 filtered nothing (the hand-written `Source` embedded `contract.go` by name). `go:embed` has no exclusions, so the generated embed takes every `.go` file and `internal/contractsrc` drops `_test.go` files and plg-generated files (first line equal to the header, CRLF tolerated) when the contract is registered; other files are kept as before. `plg gen` checks the imports of exactly the files the guest receives, including files the host's build constraints exclude (they are embedded too), and type-checks the ones that build on the host. Loading uses `go/parser` + `go/types` with the standard library's `gc` importer (export data from the go command's build cache): no `golang.org/x/tools` dependency, and the allowlist check runs before type-checking, so only allowlisted stdlib packages are ever imported. The `source` importer was measured ≈ 90× slower under `-race` (1.36 s vs 15 ms per load) and made the `-count=10` run take a minute.
- **Refused, with the reason:** names that are not identifiers or are repeated; directories without a package; parse, build-constraint and type errors; names that are not interfaces, generic interfaces (and generic aliases), type constraints, and interfaces with methods `M` and `WM` (the field would clash with the method); imports outside the allowlist, each listed as `file:line:col: "path"`, followed by the allowlist. `plg gen` does not re-check plg's interface contract (ctx first, error last, data types): `MustRegisterContract` does at init.
- **One allowlist:** `internal/goallow/packages.txt` (package `goallow`) is the Go guest allowlist; the Makefile derives `GUEST_STDLIB` from it (minus `cmp`, compiled by yaegi from source, and `encoding/json`, extracted separately), `plg gen` checks against it, goguest's allowlist test imports every package of it, and a goallow test fails if the committed symbol tables do not match it. Same extracted list as before, so the artifact is unchanged.
- **testcontract** is generated (`//go:generate` in `contract.go`); `source.go` and the hand-written adapter are gone and the tracer tests run unchanged on the generated code. A gen test fails if the committed `fooer_plg.go` is not what `plg gen` produces; the engine and goguest tests read the contract through `contractsrc.Snapshot`. The `cmd/plg` integration test runs `go generate` in a scratch module that requires this checkout: twice with no diff, `go vet`, a program whose `Register` succeeds only if the generated `init` registered the contract, a changed interface failing `go build` (`missing method`) until regenerated, and a disallowed import failing `go generate`.
- **Coverage:** `cmd/plg/main.go` is excluded from the gate (`os.Exit` cannot run in a test process; the integration test runs it as a subprocess, whose coverage `go test` does not collect). `internal/gen` leaves 2 statements uncovered, the unreachable `go/format` failure of its own output.
- **CI fix:** CI's `go test -race -shuffle=on -count=10 ./...` panicked: the `internal/guestreg` tests registered fixed language names in the process-global registry, so the second run registered them twice. They now use fresh random names. `make check` runs the same `-count=10` unit run (`make repeat`) and CI's separate step is gone since `make check` covers it.

### Ticket 05: filesystem and fs.FS storages with plugin.json

- **API:** `NewFSStorage(dir)` (an `os.DirFS`, named `dir`) and `NewFSStorageFromFS(name, fsys)` (any `io/fs.FS`, e.g. an `embed.FS` narrowed with `fs.Sub`) both return `*FSStorage`. Register options `WithGolangPlugins`, `WithLuaPlugins`, `WithTypescriptPlugins`; constants `LanguageLua` (`"lua"`) and `LanguageTypeScript` (`"typescript"`); `Descriptor.MemoryLimit uint64` (bytes, 0 = the Register limit); sentinel `ErrInvalidManifest`.
- **Layout:** every top-level directory is a plugin whose key is its name; top-level files and dot-prefixed entries are ignored (so `.git` or a README next to the plugins are harmless). `List` returns the plugins sorted by key; `Open` returns `fs.Sub(fsys, key)` after rejecting keys that are not a single directory name. Symlinked files are skipped, as `memfs.AddTree` skips them at load.
- **plugin.json** is parsed with sonic configured `CaseSensitive`, `DisallowUnknownFields`, `ValidateString`, `UseUnicodeErrors`: a misspelt or wrongly cased key (`memory_limit`, `Language`) is an error instead of a silently ignored limit. `language` must be exactly `go`, `lua` or `typescript`; `entrypoint` a non-empty `fs.ValidPath`; `memoryLimit`, when present, an integer from 1 to 4 GiB (the wasm32 maximum, 65536 pages; rounding to pages is ticket 06's). Any failure, a missing plugin.json included, is `ErrInvalidManifest` naming the directory, and Register wraps it in `ErrLoad` naming the storage. Fuzzed by `FuzzFSStorageManifest` (a rejected manifest names its directory; an accepted one meets every constraint and survives re-encoding unchanged); 60 s / 7.8 M execs found nothing.
- **Content-hash versions:** a missing `version` becomes `sha256:<hex>` over each regular file's path (relative to the plugin directory, varint-length-prefixed) and the SHA-256 of its contents, in lexical order, so it changes exactly when a file is added, removed, renamed or edited, and does not depend on the key or on the backing FS (a directory and an embed.FS of it hash the same). Register applies the same hash to an empty `Version` from any storage (memory, custom), so every loaded plugin has a version for ticket 10's change detection.
- **Allowlist semantics:** no language option enables every language. With options, a plugin in another language (unknown ones like `cobol` included) is skipped and logged at Info with `key`, `storage` and `language` (counting it is ticket 14's metrics). Keys and duplicates are checked over every listed plugin before skipping, so enabling a language later can never turn a valid configuration into a duplicate-key error. An enabled Lua/TypeScript plugin fails Register with `ErrUnsupportedLanguage` until its guest ticket lands.
- **Multi-file Go plugins** needed no new loading code: ticket 02 already copies the entrypoint's whole package directory into the read-only source tree; `testdata/storages/disk/multifile` (three files, entrypoint `.`) proves it end to end through a real directory.
- **Not enforced yet:** `MemoryLimit` is parsed, validated and carried on the Descriptor only; ticket 06 applies it (it may only lower the Register limit).
- **Measured (`BenchmarkFSStorageList`, the per-storage cost Register pays before loading, testdata tree of three plugins with one hashed):** directory 53 µs/op, 11.6 KB, 100 allocs; embed.FS 6.2 µs/op, 7.6 KB, 63 allocs. Hashing reads through one 4 KiB buffer per plugin and hides `*os.File`'s `WriteTo`, which otherwise allocates 32 KiB per file (168 KB/op before).

### Ticket 09: host-mediated HTTP

- **API:** `WithHTTP(HTTPPolicy{AllowedHosts, BlockPrivateNetworks, Timeout, MaxRequestBytes, MaxResponseBytes, MaxRequestsPerCall})` per Register; zero fields take the defaults (10 s, 1 MiB, 4 MiB, 16). `BlockPrivateNetworks` is a `*bool` so that its zero value keeps the safe default (nil = true); tests and services that must reach a private host set `new(false)` and list it in AllowedHosts. Invalid policies (no AllowedHosts, a malformed entry, negative limits, sizes over 1 GiB) fail Register with `ErrInvalidHTTPPolicy` before the name is reserved. Without `WithHTTP` every request is refused (and logged).
- **AllowedHosts entries:** host names (case-insensitive, one trailing dot ignored, Unicode converted to punycode with `golang.org/x/net/idna`'s lookup profile exactly as net/http converts the names it dials; ASCII names must be letters, digits, `-`, `_`), `*.name` wildcards (strict subdomains only), IPv4/IPv6 literals (IPv4-mapped IPv6 unmapped, zones rejected), each with an optional `:port`; without a port any port matches, and a URL's port is its explicit one or its scheme's default. Only http and https; CONNECT is refused. An IP rule never matches a name and vice versa (so `127.000.000.001` or `2130706433` are not `127.0.0.1`). Matching is fuzzed (`FuzzHostMatching`: no panics, an entry allows its own host, ASCII case and one trailing dot are ignored, wildcards exclude the apex).
- **SSRF:** with BlockPrivateNetworks the host's `net.Dialer.Control` checks every address a connection is about to use, after DNS resolution, so names resolving or rebinding to private addresses are refused like IP literals. Refused: everything not global unicast (loopback, link-local incl. 169.254.169.254, multicast, unspecified, broadcast), RFC 1918 and fc00::/7 (incl. AWS's fd00:ec2::254), 0/8, 100.64/10 (incl. 100.100.100.200), 192.0.0/24, 198.18/15, 240/4, documentation ranges, fec0::/10, Teredo, and NAT64/6to4 addresses embedding any of these. The transport never uses an environment proxy. The plugin is told "private network address refused" without the address; the log has it.
- **Host side** (`internal/httpcap`, one `http.Transport` per Register, idle connections closed with the container or a failed Register): each request runs under `context.WithTimeout(callCtx, Timeout)`, so it never outlives the call; the request body is copied out of guest memory (the transport may read it after RoundTrip returns); the response body is read straight into the answer buffer and fails with `ErrResponseTooLarge` after at most MaxResponseBytes (+ one 32 KiB read), headers are capped at 64 KiB by the transport. MaxRequestBytes bounds the whole encoded request (URL, header, body). Redirects are not followed by the host: the guest's `http.Client` follows them, so every hop is a new request checked against the policy and counted in MaxRequestsPerCall. The guest's Host header override is not sent: the URL's host is always the virtual host. Panics in the capability are recovered, logged at Error with the stack and answered as an internal error. Every request is logged by plugin key (`plg: http request` at Info with method, URL without query or credentials, status, sizes, duration; refusals and failures at Warn with the reason).
- **ABI:** host imports in wasm module `plg`: `http_roundtrip(ptr, len u32) u32` hands over an `internal/httpabi` request (method, URL, header pairs, then the body as the remaining bytes) and returns the length of the encoded answer the host keeps pending (`wire.StatusOK` + status code + header + body, or `wire.StatusError` + reason); `take_result(ptr, len u32)` copies it to guest memory (an exact-length, same-call handover: anything else traps with `ErrABI` and the instance is discarded). The engine finds an instance's host state by its wazero module name (unique per runtime), so calls pay no per-call context allocation; the per-call request counter and the pending answer reset at each `plg_init`/`plg_call`. `take_result` is generic so ticket 08's `host_call` can reuse it.
- **Guest:** `internal/goguest.Transport` is installed as `http.DefaultTransport` by the wasip1 main, so plugins use plain `net/http` (`http.Get`, `http.Client`, `http.NewRequest`). The allowlist (`internal/goallow/packages.txt`) gained `net/http`, with client symbols only (`make symbols` excludes the server side: `ListenAndServe`, `Server`, `ServeMux`, `Handler`, `ResponseWriter`, `FileServer`, ...). yaegi v0.16.1 does not support `for range <int>` (`nil type` panic at load); the test plugins use classic loops.
- **Artifact:** net/http grew the Go guest from 14.6 MB to 22.7 MB raw, over the ~20 MB threshold, so it is now committed as `guest/golang/golang.wasm.gz` (5.2 MB; `scripts/gzipwasm.go`: compress/gzip BestCompression with an empty header, byte-identical across rebuilds) and inflated once per process by `guest/golang` (`sync.OnceValues`). Cold compile ≈ 4.3–5.0 s, warm from the disk cache ≈ 0.17–0.22 s (`TestCompileCacheAcrossProcesses`, non-race helper, on a loaded 32-thread machine).
- **Measured (go1.27.3, linux/amd64, loopback httptest server, no race detector):** host side of one request (`httpcap` `BenchmarkRoundTrip`) ≈ 145 µs/op, 77 allocs (parallel ≈ 41 µs/op), almost all net/http and the loopback server; a Go plugin call making one `http.Get` through the host (`BenchmarkHTTPRoundTripGo`) ≈ 0.89 ms/op, 93 allocs, against ≈ 0.24 ms for a plain call.
- **Flaky tests fixed later:** `TestTransportFailures` expected a refused connection on the port of an `httptest` server it had just closed, but under the full parallel suite the kernel sometimes handed that port to another listener: 16 parallel runs × 3000 reproduced it 9 times, each time reaching `TestHandlerRefusals`'s server, which then failed its no-hits check too. Its server now hangs up without answering, so the request fails in transit (`ErrFailed`) on a port it owns. Tests must not assume a closed port stays free. `TestRequestsEndWithTheirContext` waited for the server to see two requests, the first being the one its 50 ms policy timeout ends; under load that one timed out before reaching the server and the test hung until `go test`'s 10 min timeout (2 of 60 full loaded runs). Only the request the test cancels now signals the server's start, and its client has a one-hour timeout so that only the call's context ends it.

### Ticket 06: pooling and resource limits

- **API:** Register options `WithPool(min, max)` and `WithMemoryLimit(bytes)`; container option `WithMemoryBudget(total)` (0 = no budget); sentinels `ErrInvalidOption` and `ErrMemoryBudget`. `ErrInvalidDescriptor` now also covers a descriptor `MemoryLimit` under one 64 KiB page or over 4 GiB.
- **`min >= 1`:** `WithPool` needs `1 <= min <= max <= 4096` (default `1, GOMAXPROCS`). One warm instance at least means Register always runs every plugin's `Create` (fail fast), and no call to an idle plugin pays a cold start (≈ 40–60 ms for Go). `max` is capped because each pool allocates room for `max` idle instances.
- **Pool design (`pool.go`, no locks):** a buffered channel of idle slots (capacity `max`, so giving an instance back never blocks), an atomic `size` counting the places taken by instances alive or starting, an atomic `waiting` count and a one-slot `wake` channel. A place is reserved (CAS on `size` under `max`, then the budget) before an instance starts and given back when it fails to start or is closed. A caller with nothing idle announces itself as waiting, then tries to grow; a discard after its failed attempt sees the waiter, and the refiller then starts an instance for it (`size < min || (waiting > 0 && size < max)`), so no wake-up is lost. Callers start instances with their own ctx (a call with a tight deadline on an exhausted pool can fail while starting one).
- **Refiller:** one goroutine per plugin, started by Register after the `min` instances, with a context cancelled by the pool's close; it waits on `wake` and starts instances while the pool needs them. A failed start (Create error, trap, or a budget refusal) is logged at Warn with `key`, `error` and `retry_in` and retried after 100 ms, doubling to 30 s; a success resets the delay. Close sets `closed`, closes `done` (waiting callers get `ErrClosed`), cancels the refiller (which also stops an instance it is starting) and waits for it, then closes instances as they come back, waiting on ctx; instances a caller or the refiller starts during close are collected too. Every goroutine is owned by `Container.Close` (goleak in every package). Pool timing (backoff sequence, refill, close) is tested with `testing/synctest` and fake instances in `internal_test.go`.
- **Discard rules (unchanged from 02, now tested by identity):** a trap, deadline stop, guest panic, ABI violation or undecodable result closes the instance and the refiller replaces it; an ordinary plugin error keeps it. Export-test hooks `HoldInstance` (instances are numbered per plugin), `Stats` and `MemoryInUse` make identity, refills and accounting observable.
- **Memory limit:** effective limit = Register limit (default 128 MiB for Go), lowered by the descriptor's, rounded down to whole pages; a descriptor asking for more is capped and logged at Warn (`plg: plugin memory limit capped at the Register limit`), not an error. Measured with `testdata/plugins/go/hog` (allocates n MiB per call): a fresh Go instance can hold ≈ 28 MiB at a 48 MiB limit, ≈ 44 at 64, ≈ 60 at 96, ≈ 92 at 128; the call over the limit traps in the Go runtime (`runtime.throw` in `allocLarge`), fails with `ErrCall` and only its instance is replaced.
- **One runtime per distinct limit,** shared by every Register and language with that limit (rather than per (Register, language, limit)): the runtime only fixes the memory cap, so sharing loses nothing and saves a module compile per Register. Runtimes are created lazily; one created by a Register racing `Close` is closed by that Register (wazero's `Close` is idempotent). **Compile cost per extra runtime** (Go guest with net/http, 22.7 MB raw, non-race, compilation cache warm): the first runtime loads from the disk cache in ≈ 160 ms; each further runtime still takes ≈ 125–145 ms (≈ 67–90 ms with the 14.5 MB guest before ticket 09), because wazero shares the machine code through the container's cache (its engine is per cache, keyed by module hash) but decodes the module again per runtime, and decoding dominates. Hence one runtime per limit, never per Register. `TestRegistersWithTheSameMemoryLimitShareARuntime` checks four Registers over two limits compile twice.
- **Budget:** two atomic counters. `reserved` holds the worst case (`max × limit`) of every registered plugin: Register reserves its plugins after listing them and before loading anything (fail fast), and gives the reservation back if any load fails. `used` charges each instance's limit while it lives; a pool grows only when it fits, so at run time pools never exceed the budget even where reservations would allow it (ticket 10's overlapping generations). With Register reserving the worst case, run-time refusals cannot happen yet; the pool tests construct pools over a smaller budget to prove growth stops (callers wait; the refiller logs and backs off).
- **CPU cap:** `engine.NewRuntime` always sets `WithCloseOnContextDone(true)`; `TestDeadlineStopsAnInfiniteLoopAndDiscardsTheInstance` stops a real `for {}` at its deadline and the next call runs on a new instance.
- **For later tickets:** 07 should put the refiller's start behind `recover` (`Phase: Refill`) and map traps to `PanicError`; 10 can drain a generation with `pool.close` (it already waits for instances in use) and must `unreserve` a removed plugin's `max × limit`; runtimes are never closed before the container (a limit no plugin uses any more keeps its runtime); 14 can export `size`, `waiting`, `refills`, `grows` and the budget counters as metrics.
- **Measured (go1.27.3, linux/amd64, Ryzen 9 9950X3D, 32 threads, no race detector, after rebasing on 03 and 09):** pool acquire + release with an instance idle 28 ns/op, 0 allocs; under `RunParallel`, 32 goroutines sharing one instance 227–231 ns/op, with 32 instances 47 ns/op, 0 allocs. One `Foo` call into a Go plugin 119–127 µs/op (10 allocs); from 32 goroutines with one instance 132–148 µs/op (serialized), with `WithPool(32, 32)` 4.5–4.7 µs/op per call: ≈ 30× the throughput. Under `-race`, `TestPoolServesManyConcurrentCalls` makes 6400 real calls (64 goroutines × 100, ordinary errors and panics mixed in, pool of 2–8) in ≈ 17 s, and `TestPoolUnderContention` runs 64 goroutines × 1000 acquire/release cycles with discards.
- **Pre-existing test-only race fixed here (`internal/goguest`):** `make check`'s repeated unit run intermittently reported a data race inside yaegi v0.16.1: it builds a recursive struct type (testcontract's `Tree`, added by 03) by patching the field types of a `reflect.StructOf` type in place (`unsafe2.SetFieldType`), and reflect caches StructOf types process-wide, so two interpreters in parallel tests patch the same type. Every goguest test that interprets the testcontract package now runs serially (`//nolint:paralleltest` with that reason); production is unaffected, since each interpreter runs alone in its own wasm instance. The package's tests take ≈ 7 s for `-race -count=10`.
- **Flaky pool tests fixed later:** `warm` starts the refiller goroutine, and its first look at the pool could come after a test's caller had announced itself waiting: seeing the waiter, the refiller started the instance the test expected the caller to start (`TestPoolCloseCollectsAnInstanceACallerStartedMeanwhile` got an instance instead of `ErrClosed` in 3 of 12000 `-race` runs). The pool was right (the caller took the instance given back, within `max`); the tests were not. With a `runtime.Gosched()` widening that window, five tests failed (in up to 97 % of runs, two by deadlock). `startPool` now ends with `synctest.Wait()`, so the refiller is parked on `wake` before the test acts; tests outside a bubble use `warmPool`.

### Ticket 08: typed Host interface

- **API:** `plg.WithHost[H any](impl H) RegisterOption` and the sentinel `ErrInvalidHost`. Register fails with `ErrInvalidHost` (wrapping `ErrInvalidInterface` for contract violations) when H breaks the interface contract (same `validateInterface` as T), when its types cannot be planned by the codec, when H is declared outside the registered interface's package (Go plugins interpret only that package's source), when impl is a nil interface, or when WithHost is given twice. H may be any interface of the contract package, T itself included; it needs no generated adapter (`plg gen` is unchanged; `internal/testcontract` gained `Host`, `User` and the plugin interface `Hosted`, added to its `go:generate` line).
- **Host dispatch:** per Register, `newHost` plans every Host method once with a `wire.Planner` (reusing the plugin-method `method` plans: arguments after ctx, results before the error) and binds impl's methods through an interface-kind `reflect.Value`, so `Method(i)` follows H's method order. The handler decodes the arguments, calls the method with the ctx wazero hands the host import (the ctx of the `plg_call` in progress, or Register's/the spawning call's ctx while `Create` runs) and answers status-prefixed: `StatusOK` + results, `StatusError` + the method error's exact message, `StatusConvert` + an `ErrConversion` message naming `Host.Method: result k` and the field, or `StatusPanic` + the panic message. An unknown method index or malformed arguments are ABI violations: the import panics, the call traps and the instance is discarded. Every one of these is guarded by `recover`; a panic is logged at Error with key, method, value and stack and never discards the instance.
- **Panic seam for ticket 07:** `hostPanic(ctx, logger, key, method, value, stack) error` in `host.go`, called from inside the deferred recover (so `debug.Stack` is the panicking stack), is the single place a Host panic becomes an error; 07 swaps the logger for its panic reporter and returns the `*PanicError` (Phase Host), whose `Error()` becomes the `StatusPanic` message the plugin receives (agreed with the 07 agent). Today the plugin gets `plg: host method panicked: contract.Host.Method: <value>`.
- **Wire / engine:** host import `plg.host_call(method, ptr, len u32) u32` next to 09's `http_roundtrip`, sharing its pending-result/`take_result` protocol and per-instance `hostState`; `engine.InstanceOptions.Host engine.HostFunc` (nil: any `host_call` traps as an ABI violation). The Go init payload gained `Host` (`"pkg.Name"`), `HostPath` and `HostMethods` (empty without a Host).
- **Guest stubs (how interpreted code reaches the compiled import):** `goabi.RenderDispatcher(iface, host)` renders, with a Host, `type plgHost struct{}` whose methods forward to package variables `PlgHostCall_<M>` of the method's exact type, and `PlgInit` calls `create(plgHost{})` with `create` typed `func(contract.Host) (contract.Fooer, error)`. Before `PlgInit`, the guest `Eval`s each variable (settable: yaegi returns the variable itself) and sets it to a `reflect.MakeFunc` closure in compiled `internal/goguest` that encodes the arguments with codecs planned once at load, calls `plg.host_call` through `goguest.HostFunc` (wired to the wasm import in `guestsrc/golang`), and decodes the answer; host errors of any status reach the plugin as `errors.New(message)`. No symbol package (`interp.Exports`) was needed: interpreted code calls straight into compiled code with its own typed values (verified natively and in wazero). An argument the guest cannot encode fails that host call with an error naming the parameter, without reaching the host; a malformed answer is recorded as an ABI fault and turns the plugin call (or init) into `StatusABI`, discarding the instance.
- **Create signature:** without WithHost, `func Create() (T, error)`; with it, `func Create(contract.Host) (T, error)`, strictly (as the PLAN entry-point table says), each mismatch failing Register with a message naming the expected form and `plg.WithHost`. yaegi represents every interpreted interface as one reflect type (`interp.valueInterface`) and compiled the dispatcher's assignment of a `Create(contract.Hosted)` to `func(contract.Host) ...` without complaint (observed), so the guest also checks Create's parameter in the plugin's source (`go/parser`, already linked): it must be the Host's qualified name through the file's import of the contract package (any import name) or the bare name through a dot import; a local alias of the type is rejected.
- **Not done here:** Lua/TS host bindings (12/13) reuse `host_call` and the handler unchanged (arguments and results in the wire encoding, schema from `wire.Schema`). A Host implementation must not wait on a call to the plugin that is calling it (its instance is busy).
- **Measured (go1.27.3, linux/amd64, 32 threads, no race detector):** host side of one call (`BenchmarkHostDispatch`, Lookup returning a struct with a nested pointer, a slice and a time) ≈ 540 ns/op, 275 B, 8 allocs (≈ 155 ns/op parallel); one plugin call making one host call (`BenchmarkCallGoHost`, every protection on) ≈ 210–225 µs/op, 15 allocs vs ≈ 120–150 µs/op, 10 allocs for `BenchmarkCallGo`, so the host round trip costs ≈ 80–90 µs, nearly all of it inside the guest (yaegi calling the stub and the reflect.MakeFunc closure, encode/decode in wasm, two host-import transitions); with a struct result (`BenchmarkCallGoHostStruct`) ≈ 335 µs/op, 26 allocs.

### Ticket 07: stability, panic containment and inspection

- **API:** `*PanicError{Value, Time, Key, Language, Phase, Method, Guest, Stack}` (field order is alignment's; the PLAN shape otherwise), `type Phase string` with `PhaseLoad`, `PhaseCreate`, `PhaseCall`, `PhaseHost`, `PhaseHTTP`, `PhaseStorage`, `PhaseClose`, `PhasePoll`, `PhaseRefill`, `PhaseCallback`; container options `WithOnPanic(func(ctx, *PanicError))` and `WithPanicHistory(n)` (default 64, `<= 0` keeps none); `(*Container).RecentPanics()` (oldest first, at most n); sentinel `ErrGoStatement`. `Error()` is `plg: <key>: <phase> [<method>] failed: <value>`; `Unwrap` returns `ErrCall` in `PhaseCall` (so every `ErrCall` from 02–06 is now a PanicError and the old `errors.Is(err, ErrCall)` checks hold) and `Value` when it is an error, so a call stopped at its deadline is `errors.Is(err, context.DeadlineExceeded)`.
- **What is a PanicError:** a host-side panic recovered at a boundary, or a guest failure that discards the instance: trap (OOM, stack overflow, runtime abort), deadline stop, guest-recovered panic, ABI violation/undecodable result. Not: plugin errors, conversion errors, a `Create` returning an error, source that fails to load (all ordinary errors, as before). Phases: `Create` covers the whole `plg_init` guest step (interpreting the source, the plugin's `init`/`main`, `Create`); `Load` is the host loading a plugin (the storage's fs.FS is user code: hashing, copying, parsing it) and host panics while warm starts instances; `Call` also covers a host panic in the codec or while a caller starts an instance.
- **Boundaries (all with `recover`, stack = `debug.Stack()` taken in the deferred function):** Storage `Name`/`List` (key = Register name) and `Open` (`PhaseStorage`, via `storageCall`); the whole of `load` (`PhaseLoad`); the call boundary `exchange` (encode, guest call, decode; the instance is discarded); Host dispatch (08's seam: a panic in the Host method or in plg's decoding/encoding of its values becomes a `PhaseHost` PanicError, logged/recorded/called back, whose `Error()` text the plugin receives as the Host method's error; the instance keeps serving); the HTTP capability (`internal/httpcap` no longer logs itself: `Client.Handler(key, panicked)` passes the recovered value to the container's reporter, `PhaseHTTP`, and the plugin's request fails with `ErrInternal`); the pool's `spawn` (phase of whoever starts: `Load` from warm, `Call` from a caller, `Refill` from the refiller), each refiller iteration (`PhaseRefill`; counted as a failure, so it backs off) and `closeInstance` (`PhaseClose`); the `WithOnPanic` function and the logger's handler (`PhaseCallback`, `Method` `OnPanic` / `slog.Handler.Handle`; recorded in the history, never passed to OnPanic again, and a panicking handler's own failure is not logged with it). Reporting = Error log `plg: plugin failure` (key, phase, value, language, method, guest, stack), history, then OnPanic synchronously on the failing goroutine. Pollers are ticket 10's: its poll round runs behind the same reporter with `PhasePoll`.
- **History:** a ring of `atomic.Pointer[panicEntry{pe, seq}]` plus an atomic sequence counter; a writer takes `seq` and stores into `seq % n` by compare-and-swap, unless the slot already holds a newer entry; a reader walks the last n sequence numbers and keeps only slots whose entry carries exactly that `seq`, so overwritten or not-yet-stored slots are skipped instead of returned out of order. Proven by `TestPanicHistoryUnderConcurrency` (8 writers × 2000, 4 concurrent readers, `-race`: every read ≤ n entries and per-writer ordered; quiescent read is exactly n).
- **Lost-update race fixed later:** the store was unconditional, so a writer delayed between taking `seq` and storing overwrote the entry recorded n later in the same slot, which was then missing for good (the quiescent read of `TestPanicHistoryUnderConcurrency` had n − 1 entries: 26 of 4800 runs, 16 in parallel, `-race`). `TestPanicHistoryKeepsANewerEntryFromALateWriter` replays that interleaving deterministically.
- **Guest output:** `internal/guestout.Capture`, one per instance (created by `goLauncher.start`, wired as the wazero module's stdout/stderr). Complete lines are logged as `plg: guest output` with `key`, `stream` (stdout at Info, stderr at Warn), cut at 4 KiB per record, at most 64 KiB per stream per step (then one `plg: guest output truncated` notice); partial lines are flushed when each step (init, call, close) ends. stderr's last 32 KiB live in a lazily allocated ring; `Guest()` returns what stderr received since the step began — that is `PanicError.Guest`. The capture is owned like the instance (one goroutine at a time), so it needs no synchronization. Measured (no race detector): a step with no output (`Begin` + `End`, paid by every call) 1.5 ns/op, 0 allocs; one 80-byte stderr line kept and logged 45 ns/op, 1 alloc (the line's string, discarding handler). Calls stay at 10 allocs (`BenchmarkCallGo` 131–150 µs/op, unchanged); `BenchmarkHostDispatch` ≈ 565 ns/op, 8 allocs (≈ 540 before: the panic boundary is now an inner closure).
- **Guest tracebacks, as observed with the hostile corpus:** a Go plugin's panic: yaegi prints one `file:line:col: panic: pkg.Func(...)` line per interpreted frame it unwinds (`/gopath/src/plg.local/plugin/plugin.go:64:2: panic: plg.local/plugin.deeper(...)` … `plugin.go:35:9: … Fail(...)`), so `Guest` is the plugin-level traceback; this needed no guest change (the artifact is untouched). OOM: `fatal error: out of memory` + the Go runtime's goroutine trace (`runtime.throw`, `runtime.mallocgc`, …). Unbounded recursion at a 48 MiB limit: the stack copy fails first (`fatal error: out of memory` in `runtime.newstack`/`morestack`), no wasm-level stack overflow. Runtime abort: `select {}` ⇒ `fatal error: all goroutines are asleep - deadlock!` with the trace through `reflect.Select`.
- **Limitation (deadline stops, accepted):** a call stopped at its context's deadline (`for {}`) has no traceback. wazero v1.12's close-on-context-done raises a `*sys.ExitError` and `wasmdebug.(*stackTrace).FromRecovered` deliberately returns exit errors without the wasm stack it built ("Don't wrap an exit error!"), and a single-threaded wasm yaegi cannot be interrupted to report its position (guest goroutines do not run while the interpreter loops). So `Guest` holds only what the plugin wrote to stderr during that call (usually nothing); the stop is identified by `errors.Is(err, context.DeadlineExceeded)`.
- **`go` statements:** before copying a Go plugin's source, Register parses every regular non-test `.go` file below the package directory on the host (`go/parser`, `SkipObjectResolution`; symlinks skipped as memfs skips them) and fails with `ErrLoad` wrapping `ErrGoStatement: plugin.go:18:2, worker.go:8:2` (positions relative to the plugin's fs.FS, nested func literals and subdirectories included). A file that does not parse now fails here with the parser's `file:line:col` (still `ErrLoad`, as the guest's load error was). Not covered: stdlib functions that spawn goroutines internally (`time.AfterFunc`, `context.AfterFunc`) — they are paused between calls like any guest goroutine.
- **Hostile corpus (RULES §2), `testdata/hostile/go/<case>/` with plugin.json, loaded through `NewFSStorageFromFS`:** `hostile` (`Fail("panic"|"oom"|"recurse"|"abort"|"loop"|"print")`), `createoom` (Create runs out of memory ⇒ Register fails with a `PhaseCreate` PanicError carrying the OOM trace), `goroutine` (two go statements in two files). Every case runs in one pool-of-1 instance at a 48 MiB limit, the host survives, the next call runs on a fresh instance, each failure is in `RecentPanics` and logged at Error. Lua/TS cases join this directory with their guests.
- **For later tickets:** 10 — wrap the poll round with `c.panics.recovered(ctx, &PanicError{Key: name, Phase: PhasePoll}, v)`; `pool.closeInstance` reports host panics only: close *errors* (including a plugin `Close` returning an error once 10 adds it) stay ordinary errors logged at Warn — if 10 wants guest traps in `plg_close` reported, map them in `goInstance.Close` with `Guest()`. 12/13 — implement the pool's `instance` interface (incl. `Guest()`) and reuse `guestout.Capture`; Lua/JS tracebacks then land in `PanicError.Guest` the same way. 14 — count `report` calls as `plg.plugin.panics{key,phase}`.

### Ticket 10: reload, polling and draining

- **API:** `(*Container).Reload(ctx)` reloads every registration and returns the failures joined (in name order); `plg.ReloadInterface(ctx, c, name)` reloads one (`ErrNotFound` for an unknown name, `ErrClosed` once closed); Register option `WithPollInterval(d)` (negative ⇒ `ErrInvalidOption`, 0, the default, does not poll). A plugin instance may define `Close() error` or `Close()`: the dispatcher binds it to `goabi.CloseVar` (`PlgClose`; yaegi's interface assertions compare method names only, so each binder recovers the panic a mismatched signature causes and reports no Close, as Go would), and the guest's `plg_close` calls it once. The guest artifact was rebuilt for this (`make guests`, reproducible).
- **One reloader goroutine per registration** (`reload.go`), started once Register publishes, runs a round per request and per poll tick (`time.Ticker`: a round longer than the interval is followed by one more, not a backlog), so rounds of one registration never overlap and each round compares against exactly what it replaces. A request returns the next round to start, which lists the storages after the request; requests made while a round runs share the next round. ctx bounds only the caller's wait. `Close` cancels the rounds' context (a load in progress stops with it and the round fails with `ErrClosed`; a round that outlives Close's ctx publishes nothing, the catalogue being closed) and waits for the goroutines on ctx; a Register racing Close stops its own.
- **Change detection:** a listed plugin is unchanged when its whole `Descriptor`, version included, equals the served generation's: unchanged plugins are neither recompiled nor restarted (`TestPollingFollowsTheStorage` checks the generation and the instance identity). An empty `Version` is resolved as Register does, by opening the plugin and hashing its files, so a storage giving versions spares that per poll.
- **Mirroring rules:** a plugin no longer listed is removed (`Get` and calls through adapters obtained earlier: `ErrNotFound`); a new plugin that fails to load is skipped and logged at Warn (`plg: reload skipped a new plugin that failed to load`); an updated plugin that fails keeps its last good generation (`plg: reload kept the last good version of a plugin`) and is retried on every poll; a key listed twice or with an invalid descriptor keeps its previous generation (`listStorages` now collects per-key problems; Register still fails on the first); a storage that cannot be listed (a malformed `plugin.json` included) fails the round with nothing changed (`plg: reload failed`). Every failure is also returned, joined. All of a round's changes are published in one CAS (`snapshot.with(iface, put, removed)` rebuilds that interface's sorted slice).
- **Generations:** `plugin` is one generation of a key. `state` (`atomic.Int64`) counts calls in flight plus `retiredBit` (1<<62) once retired: `enter` adds 1 and backs out if the bit is set (the caller re-reads the catalogue, whose latest snapshot never holds a retired generation), `leave` subtracts 1 and closes `idle` when only the bit is left, `retire` ORs the bit in. `idle` is closed once through a CAS on `quiet`, since both the last leave and a refused late enter can see zero. `TestGenerationRetiringUnderLoad` retires under 32 goroutines' traffic: no call runs on an idle generation, and every worker is refused afterwards.
- **Adapters follow the key:** the adapter bound at a key's first load is shared by its later generations, and each call looks up the key's current generation (`c.serving`: one map lookup and two atomic adds, no allocation), so an adapter obtained before a reload calls the new version, fails with `ErrNotFound` after a removal and works again when the key comes back. `HoldInstance` counts as a call in flight too.
- **Draining:** retiring a generation starts a goroutine (owned through `c.retired`, an `xsync.Map` of cancel functions) that waits for `idle`, closes the pool (each instance running the plugin's Close) with `closeTimeout` = 10 s, gives the memory reservation back and closes `closed`; a Close that hangs is stopped at the timeout by wazero's close-on-context-done, so it cannot hold a generation's memory forever. `Container.Close` stops the reloaders, swaps in the closed snapshot, retires every generation and waits on ctx for all retired ones (when ctx ends it cancels the waits: idle instances are closed at once, those in use are left to the runtimes' close), and returns the failures of closing the generations it retired (`closeErr`, written before `closed` is closed).
- **Memory budget:** a new generation reserves its full worst case while the one it replaces keeps its own until it is closed, so an update needs that headroom; one that does not fit fails with `ErrMemoryBudget`, like a broken update. Register's reservations move onto their generations and are given back when each closes.
- **Merged with 07 (panics):** Register and reloads share 07's boundaries: storage `Name`/`List`/`Open` (`PhaseStorage`) and `loadStep` (`PhaseLoad`, around opening and launching; reloads split the two so that only a changed plugin takes a reservation, given back when its launch fails or panics). A panic once a plugin's instances have started (in `launch`: only the user's slog handler, on `plg: plugin loaded`) now closes them before the boundary reports it; on 07's main such a pool was left with no owner, its refiller never stopped. Anything else panicking in a round (the user's slog handler, say) is recovered around the round as `c.panics.recovered(ctx, &PanicError{Key: name, Phase: PhasePoll}, v)`, returned to the callers waiting for it; the reloader survives. The retired-generation closer recovers too (`PhaseClose`, the generation counts as closed). Removed generations are retired before their `plg: plugin removed` log, so a panicking handler cannot leave one undrained. `plugin.lang` (07) is `plugin.desc.Language` here.
- **Plugin Close failures follow 07's Close conventions:** `engine.Instance.Close` reports a plugin Close that did not succeed as `*engine.CloseError{Status, Message}`, and no longer runs `plg_close` once its ctx has ended (a forced Close then frees the instance without running plugin code, instead of reporting a stop for every idle instance). `goInstance.Close` keeps a Close returning an error an ordinary error (logged at Warn by the closer: `plg: closing a plugin version failed`, and returned by `Container.Close` for the generations it closes); a guest panic, a trap, a stop at the close timeout or an ABI violation in `plg_close` becomes a reported `PanicError` in `PhaseClose` carrying the guest's stderr (`Guest`: yaegi's plugin-level traceback for a panic).
- **Logs:** reloads log plugins in a language the Register leaves out at Debug only, so polling does not repeat Register's Info line every interval; the memory-limit-capped warning moved from listing to `launch`, so it fires once per actual load. Loaded, closed and removed versions are logged at Info with key and version.
- **Flake fixed in 09's `TestPluginHTTPEndsWithTheCall`:** "call deadline before the policy timeout" raced a 100 ms deadline against the request reaching the server and hung on `<-srv.abandoned` when the deadline won. It now calls with an `expiring` context (its `Done` closes and its `Err` becomes `context.DeadlineExceeded` when the test closes it) closed only once the server saw the request, and also checks the failure is the call's deadline, not the policy's timeout. "policy timeout" (a real 50 ms timer in httpcap) waits for the abandonment only when the request reached the server before the timer fired. The HTTP test file passed 5× under `-race` (`-count=5`).
- **Measured (go1.27.3, linux/amd64, Ryzen 9 9950X3D, 32 threads, no race detector, benchstat over 6 runs against main at 07):** one Go plugin call (`BenchmarkCallGo`) 113.8 µs ± 20% before vs 125.9 µs ± 16% after (p = 0.093, no significant change), 10 allocs both; `BenchmarkCallGoParallel` pool=1 126.0 vs 139.6 µs (p = 0.13, ~), pool=32 4.697 vs 4.622 µs (−1.6%); `Get` 11.25 vs 10.98 ns, 0 allocs; `GetParallel` 1.27 vs 1.31 ns. A reload finding nothing to do (`BenchmarkReloadUnchanged`: a directory of three version-less plugins listed and hashed, compared, the round handed over to and back from the reloader) 119 µs/op, 22 KiB, 150 allocs; asked for from 32 goroutines (`BenchmarkReloadUnchangedParallel`) 17.4 µs per request, the requests sharing rounds; a reload replacing a plugin (`BenchmarkReloadChanged`: copy and check the source, start the instance, run Create, publish, close the old generation) 129 ms/op, 42.7 MiB, 138k allocs. `BenchmarkRegister` (new; RULES §3 lists Register, which had none: one plugin under a new name each time) 151–169 ms/op, 52–62 MiB, 252–272k allocs (3 runs).
- **Limitations:** polling lists every storage each interval (no change notifications); version-less plugins are hashed on every poll. A plugin's Close runs only on instances closed while their close context lives: after `Container.Close`'s ctx ends, instances still in use are freed with their runtime, without their Close.
