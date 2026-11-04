# go-plg

go-plg (package `plg`) lets a Go service load plugins written in **Go, Lua and TypeScript** from storages you
choose (a directory, an `embed.FS`, S3, a database, …) and call them through **ordinary Go interfaces**. Plugins
are shipped as source: plugin authors never run a build step.

Every plugin runs inside **WebAssembly** ([wazero](https://github.com/tetratelabs/wazero): pure Go, no cgo), in an
interpreter compiled to WebAssembly and embedded in plg:

| Plugin language | Runs in |
|---|---|
| Go | [yaegi](https://github.com/traefik/yaegi), compiled to wasip1 |
| Lua | Lua 5.4, compiled with wasi-sdk |
| TypeScript (and JavaScript) | [QuickJS-ng](https://github.com/quickjs-ng/quickjs), compiled with wasi-sdk; [esbuild](https://github.com/evanw/esbuild) bundles the plugin on the host |

**Stability comes first: a plugin cannot take down the host process.** Panics, running out of memory, stack
overflows, infinite loops and runtime aborts inside a plugin become errors on that plugin's call; the faulty
instance is thrown away and replaced. Plugins see no filesystem beyond their own files, no environment and no
network; they reach the outside world only through what the service grants them (a typed Host interface, an
HTTP allowlist).

- [Concepts](#concepts)
- [Quick start](#quick-start)
- [The interface](#the-interface)
- [Code generation](#code-generation)
- [Writing plugins](#writing-plugins) ([Go](#go-plugins), [Lua](#lua-plugins), [TypeScript](#typescript-plugins))
- [Guests and blank imports](#guests-and-blank-imports)
- [Storages](#storages)
- [Limits, pools and the memory budget](#limits-pools-and-the-memory-budget)
- [HTTP](#http)
- [Calling back into the service: the Host interface](#calling-back-into-the-service-the-host-interface)
- [Reloading and polling](#reloading-and-polling)
- [Stability guarantees and `PanicError`](#stability-guarantees-and-panicerror)
- [Logs and metrics](#logs-and-metrics)
- [Deployment: `plg warm` and `plg build-guest`](#deployment-plg-warm-and-plg-build-guest)
- [Performance](#performance)
- [Limitations and residual risks](#limitations-and-residual-risks)
- [Examples](#examples)
- [Development](#development)

## Concepts

- **Container** (`plg.New`): holds every plugin a service loaded and the WebAssembly runtimes running them.
  Create one at startup, `Close` it at shutdown. Everything in plg is safe for concurrent use.
- **Interface** (the contract): an ordinary Go interface the service declares, such as `Greeter`. Every plugin
  registered for it implements it, whatever its language. The service and the plugins exchange only data
  (strings, numbers, structs, slices, maps, times), never functions or channels.
- **Adapter**: Go cannot create a type with methods at run time, so each interface needs a small compiled
  adapter. `plg gen` writes it, from a `//go:generate` line next to the interface.
- **Storage**: lists plugins and opens their files. plg ships a directory / `fs.FS` storage and an in-memory one;
  anything else (S3, a database) is a small interface to implement.
- **Register** (`plg.Register[T](ctx, c, name, ...)`): loads every plugin its storages list for interface `T`
  under a name, and fails fast if any of them does not load. A plugin's **key** is `<name>/<plugin key>`, such as
  `greeter/hello-go`.
- **Get / All**: `plg.Get[T](ctx, c, key)` returns one plugin as a `T`; `plg.All[T](ctx, c)` iterates over every
  plugin of `T` in key order. Neither allocates: look plugins up per request.
- **Guest**: the WebAssembly module running one language (the yaegi, Lua or QuickJS interpreter plus plg's glue).
  A program links the guests of the languages it accepts with blank imports.
- **Instance**: one running copy of a plugin inside its guest, with its own memory and its own state (each runs
  the plugin's `Create`). Each plugin has a **pool** of instances; an instance serves one call at a time.

## Quick start

A service with one interface, `Greeter`, and one plugin in each language. Create a module:

<!-- quickstart:commands -->
```sh
mkdir quickstart && cd quickstart
go mod init example.com/quickstart
go get github.com/heynemann/go-plg@latest
```

Declare the interface, with the line that generates its adapter:

<!-- quickstart:file greeter/greeter.go -->
```go
// Package greeter is the contract between the service and its plugins.
package greeter

//go:generate go run github.com/heynemann/go-plg/cmd/plg gen Greeter

import "context"

// Greeter is implemented by every plugin.
type Greeter interface {
	Greet(ctx context.Context, name string) (string, error)
}
```

Write the plugins, one directory each, with a `plugin.json` naming the language and the entrypoint. In Go:

<!-- quickstart:file plugins/hello-go/plugin.json -->
```json
{"language": "go", "entrypoint": "."}
```

<!-- quickstart:file plugins/hello-go/hello.go -->
```go
package hello

import (
	"context"

	"example.com/quickstart/greeter"
)

type hello struct{}

// Create is called once per instance of the plugin.
func Create() (greeter.Greeter, error) {
	return hello{}, nil
}

func (hello) Greet(_ context.Context, name string) (string, error) {
	return "Hello, " + name + "! (Go)", nil
}
```

In Lua:

<!-- quickstart:file plugins/hello-lua/plugin.json -->
```json
{"language": "lua", "entrypoint": "plugin.lua"}
```

<!-- quickstart:file plugins/hello-lua/plugin.lua -->
```lua
function Create()
  return {
    Greet = function(self, name)
      return "Hello, " .. name .. "! (Lua)"
    end,
  }
end
```

In TypeScript:

<!-- quickstart:file plugins/hello-ts/plugin.json -->
```json
{"language": "typescript", "entrypoint": "plugin.ts"}
```

<!-- quickstart:file plugins/hello-ts/plugin.ts -->
```ts
export function Create() {
  return {
    Greet(name: string): string {
      return `Hello, ${name}! (TypeScript)`;
    },
  };
}
```

The service registers the plugins found in `plugins/` and calls each of them:

<!-- quickstart:file main.go -->
```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/heynemann/go-plg"
	_ "github.com/heynemann/go-plg/guest/golang"     // runs Go plugins
	_ "github.com/heynemann/go-plg/guest/lua"        // runs Lua plugins
	_ "github.com/heynemann/go-plg/guest/typescript" // runs TypeScript plugins

	"example.com/quickstart/greeter"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	c := plg.New()
	defer c.Close(ctx)

	err := plg.Register[greeter.Greeter](ctx, c, "greeter", plg.WithStorage(plg.NewFSStorage("plugins")))
	if err != nil {
		return err
	}

	for key, g := range plg.All[greeter.Greeter](ctx, c) {
		msg, err := g.Greet(ctx, "world")
		if err != nil {
			return err
		}

		fmt.Printf("%s: %s\n", key, msg)
	}

	return nil
}
```

Generate the adapter and run:

<!-- quickstart:commands -->
```sh
go mod tidy
go generate ./...
go run .
```

<!-- quickstart:output -->
```
greeter/hello-go: Hello, world! (Go)
greeter/hello-lua: Hello, world! (Lua)
greeter/hello-ts: Hello, world! (TypeScript)
```

The first run compiles the Go guest to machine code, which takes a few seconds; the result is cached on disk
(see [`plg warm`](#deployment-plg-warm-and-plg-build-guest)), so later runs start in a fraction of a second. This
quick start is tested: `examples/quickstart` copies the files above out of this README into a fresh module and
checks the output.

## The interface

Register checks the interface by reflection (and the Host interface, if any) and refuses one that breaks these
rules with `plg.ErrInvalidInterface`:

- **Method shape:** every method is `M(ctx context.Context, args...) (results..., error)`: a context first and an
  error last, both required. Variadic methods are allowed.
- **Data only:** `bool`, integers, floats, `string`, `[]byte`, slices, maps with string keys, structs whose
  fields are all exported, pointers to structs, `time.Time`, and named types of these (recursive types included).
  Functions, channels, interfaces, arrays, complex numbers and `uintptr` are refused.
- **Integers cross exactly or not at all:** Lua and JavaScript numbers are float64, so an integer beyond ±2^53
  fails the call with `plg.ErrConversion` (naming the parameter or result and the field) instead of losing
  precision. Go plugins obey the same limit, so every language behaves alike.
- **Names in Lua and TypeScript:** methods keep their Go names (`Greet`); struct fields use their `json` tag name,
  else their Go name. A field tagged `json:"-"` does not cross.
- **nil and empty stay distinct:** a nil slice, map or pointer arrives as `nil` (Lua) or `null` (TypeScript), an
  empty one as an empty table or array.
- **Times** cross as an instant (Unix seconds and nanoseconds), in UTC.
- **Decoding is bounded by the bytes received:** the slices, maps and pointer targets decoded from n bytes may take
  at most 32 × n + 4 MiB of memory, measured by their Go types' sizes. Every supported type stays well within that
  (at most 16 bytes of memory per encoded byte), but a field tagged `json:"-"` takes memory its encoding does not
  pay for: decoding a value whose skipped fields would exceed the bound fails the call (a plugin's result fails
  with a `*plg.PanicError` and the instance is discarded) instead of letting a short, hostile answer allocate
  gigabytes. Keep large state out of the types that cross.

The interface lives in a package of its own (the **contract package**): Go plugins import it, so the Go guest
interprets its source, and it may import only the packages the Go guest allows (see [Go plugins](#go-plugins)).
Helpers declared there (like `pricing.Subtotal` in the examples) are shared by the service and the Go plugins.

A plugin's own error (`errors.New` in Go, `error(...)` in Lua, `throw` in TypeScript) reaches the caller as a
`*plg.PluginError` whose `Error()` is exactly the plugin's message; the instance stays in service.

## Code generation

Next to the interface, one line per package:

```go
//go:generate go run github.com/heynemann/go-plg/cmd/plg gen Greeter Other
```

`go generate ./...` then writes `greeter_plg.go` (named after the first interface) with:

- an adapter per interface, with a compile-time check (`var _ Greeter = ...`): a changed interface fails
  `go build` until you run `go generate` again;
- fast paths per interface: a typed adapter and an encoder/decoder per type its methods use, so calls skip
  `reflect.MakeFunc` and the reflective codec (≈ 0.3–3.5 µs less per call, 40–45 % fewer allocations; see
  `PLAN.md`, ticket 16). They import `github.com/heynemann/go-plg/plgimpl`, which follows plg's version: run
  `go generate` again after upgrading plg. If a struct they encode changes (a field added, reordered or tagged
  `json:"-"`) without regenerating, the program fails at start instead of encoding the old layout.
  `plg gen -fast=false` leaves them out, and plg calls plugins through its generic, reflective binder (as it
  does for files generated by older versions);
- the package's Go source, embedded with `go:embed` (the Go guest interprets it, so Go plugins can import the
  contract);
- an `init()` registering all of it with plg (`plg.MustRegisterContract`).

`plg gen` refuses imports outside the Go guest's allowlist, naming each one. Without the generated file, Register
fails with `plg.ErrNoContract`. Commit the generated file like any other source.

## Writing plugins

Every plugin is a directory of source files with an entrypoint and a `Create` that returns the plugin's
implementation. `Create` runs once per instance, so whatever it builds is per-instance state. A plugin may also
define a `Close` method, called when an instance is retired (after a reload, at `Container.Close`).

`ctx` is never visible to plugins: the host uses it to stop a call at its deadline and passes it to Host and HTTP
calls made during the call.

### Go plugins

A package (`"entrypoint": "."`, or a `.go` file of it) declaring:

```go
func Create() (greeter.Greeter, error)                  // without a Host
func Create(host greeter.Host) (greeter.Greeter, error) // with plg.WithHost[greeter.Host]
```

- Every `.go` file of the directory (and below) is loaded; `_test.go` files are skipped. Since a Go plugin is
  plain Go, unit-test it natively (as `examples/plugins/bulk` does).
- The package name does not matter (`package main` works too).
- **Standard library allowlist:** `bufio bytes cmp context encoding/base64 encoding/hex encoding/json errors fmt io
  maps math math/big math/bits math/cmplx math/rand math/rand/v2 net/http (client side, through the host) net/url
  regexp slices sort strconv strings sync sync/atomic time unicode unicode/utf16 unicode/utf8`, plus the contract
  package. Anything else fails Register with `plg.ErrLoad`. To allow more packages (`encoding/xml`, a pure-Go
  library), build a custom guest with [`plg build-guest`](#deployment-plg-warm-and-plg-build-guest).
- **No goroutines:** a `go` statement fails Register with `plg.ErrGoStatement` (file:line:col). A WebAssembly
  instance has a single thread, and guest goroutines would be paused between calls; parallelism comes from the
  instance pool.

### Lua plugins

A `.lua` entrypoint defining a global `Create()` that returns a table of methods, called as `inst:Method(args...)`:

```lua
local Rule = {}
Rule.__index = Rule

function Rule:Discount(order)           -- struct fields by json name
  local tier = host.Tier(order.customer) -- a Host method (plg.WithHost)
  if tier == "gold" then
    return { cents = 100, reason = "gold" }
  end
  return { cents = 0, reason = "" }
end

function Create()
  return setmetatable({}, Rule)
end
```

- A method returns its result, or an array table of several results (`return { true, 42 }`); `error("...")`, or
  returning `nil, "message"`, is the call's error.
- Libraries: `base` (without file loading), `string`, `table`, `math`, `utf8`, `coroutine`, a bundled `json`
  module (`json.encode`, `json.decode`, `json.null`, `json.array()`), `host.<Method>(...)` (raises the Host's
  error) and `http.request{...}`. `require` loads `.lua` files of the plugin's own directory (mounted read-only
  at `/plugin`), nothing else; there is no `io`, `os`, `package` or `debug`.
- Integers are Lua integers, floats are numbers, `[]byte` is a string, `time.Time` is `{seconds, nanos}`.

### TypeScript plugins

A `.ts` (or `.js`) entrypoint exporting `Create()`, which returns an object (a class instance or a literal) with
every method; methods, and `Create`, may be `async`:

```ts
declare const host: { Tier(customer: string): Promise<string> };

export function Create() {
  return {
    async Discount(order: { customer: string }) {
      const tier = await host.Tier(order.customer); // a Host method (plg.WithHost)
      return { cents: tier === "gold" ? 100 : 0, reason: tier };
    },
  };
}
```

- esbuild bundles the entrypoint and what it imports by relative path (`.ts`, `.js`, JSON and text files) from
  the plugin's own files; package, URL and absolute imports fail Register naming `file:line:col`. TypeScript is
  transpiled, not type-checked.
- Several results come back as an array; a `throw` or a rejected promise is the call's error.
- Globals: the ECMAScript built-ins, `console` (to the service's log), `host` (Host methods return Promises) and
  `fetch`. No `require`, `process`, `std` or `os`.
- `[]byte` is a `Uint8Array`, `time.Time` is `{seconds, nanos}` (a `Date` is accepted back), nil is `null`.

## Guests and blank imports

Each language's guest is a package embedding its WebAssembly module; a blank import links it and makes the
language available:

```go
import (
	_ "github.com/heynemann/go-plg/guest/golang"     // Go plugins (≈ 5 MB, compressed)
	_ "github.com/heynemann/go-plg/guest/lua"        // Lua plugins (≈ 0.3 MB)
	_ "github.com/heynemann/go-plg/guest/typescript" // TypeScript plugins (≈ 1.2 MB)
)
```

Link only the languages you accept. A plugin whose guest is not linked fails Register with
`plg.ErrGuestNotLinked`, naming the import to add. To accept only some languages from a storage, pass
`plg.WithGolangPlugins()`, `plg.WithLuaPlugins()` and/or `plg.WithTypescriptPlugins()` to Register: plugins in
other languages are then skipped (logged and counted), not errors. Without any of them every language is enabled.

## Storages

```go
type Storage interface {
	Name() string                                          // for logs and errors
	List(ctx context.Context) ([]plg.Descriptor, error)    // the plugins, each with a Key, Language, Entrypoint, Version, MemoryLimit
	Open(ctx context.Context, d plg.Descriptor) (fs.FS, error) // a plugin's files
}
```

A storage belongs to one Register call: every plugin it lists implements that call's interface. Register may take
several storages (`plg.WithStorage` repeated); a key listed twice fails with `plg.ErrDuplicateKey`.

Register reads an opened plugin's files once, into an in-memory copy that it hashes (for an empty `Version`),
bundles and loads, so files changing during a load are never half seen. Symbolic links are never followed, in
any storage: a link that stays inside the plugin's files is skipped as if it were not there (a plugin cannot
import or `require` it), and a link whose target is absolute or leads out of the plugin's files (or cannot be
read) fails the plugin's load with an error naming it.

The copy is bounded, in every storage: a regular file over `plg.MaxPluginFileSize` (16 MiB), or regular files
over `plg.MaxPluginSize` (64 MiB) together, fail the load with `plg.ErrPluginTooLarge`, naming the plugin and
the file, before they are read (every file counts, used by the plugin's language or not); a file growing while
it is read is read up to what is left of the limits only. Ending Register's context stops the copy at the next
file or read. `MemoryStorage.Put` and the FS storage's `Open` copy by the same rules.

Built in:

- `plg.NewFSStorage(dir)` and `plg.NewFSStorageFromFS(name, fsys)` (any `fs.FS`: an `embed.FS`, `fs.Sub` of one,
  …). Each top-level directory is a plugin, its name the key, with a `plugin.json`:

  ```json
  {"language": "go", "entrypoint": "plugin.go", "version": "1.4.0", "memoryLimit": 67108864}
  ```

  `language` (`go`, `lua` or `typescript`) and `entrypoint` are required; `version` is optional (without it,
  `List` leaves it empty and Register uses a SHA-256 of the files it loads); `memoryLimit` (bytes, from 65536 — one wasm page — to 4 GiB) may only lower
  the Register's limit. Unknown or misspelt keys are errors (`plg.ErrInvalidManifest`), so a typo never silently
  drops a limit. A `plugin.json` must be a regular file of at most `plg.MaxManifestSize` (64 KiB) bytes, else
  it is `plg.ErrInvalidManifest` too (a FIFO or a link to a device is never read). Top-level files, links and dot-directories are ignored. `NewFSStorage` reads `dir` through an
  `os.Root`, so no path or link leads it out of `dir`, and it copies a plugin directory entry by entry, each opened
  relative to its parent and checked to be that very entry: a file or directory swapped for a link mid-copy is
  never followed, out of the plugin or inside it. `NewFSStorageFromFS`, `MemoryStorage.Put` and your own storages
  are copied through the `fs.FS` interface, which opens a file by name: a tree that follows links (an `os.DirFS`,
  anywhere on the host) can be led out of the plugin by a link swapped in mid-copy, so serve directories others
  can write to with `NewFSStorage`.
- `plg.NewMemoryStorage(name)` with `Put(desc, files)` and `Delete(key)`: plugins a program builds itself, and
  tests.

Your own: implement the three methods. `plg.ParseManifest` parses a `plugin.json` exactly like the FS storage, for
storages that keep the same layout elsewhere; read at most `plg.MaxManifestSize`+1 bytes of it, as it rejects more. A `Version` that changes exactly when the plugin changes lets
reloads skip unchanged plugins (an empty one makes plg hash the files on every reload). Two complete storages,
kept out of plg's own dependencies, live in [`examples/`](#examples): **S3** (aws-sdk-go-v2, the plugin.json
layout under a key prefix, versions from object ETags) and **PostgreSQL** (pgx, only enabled plugins listed,
versions maintained by triggers, a migration included).

## Limits, pools and the memory budget

Per Register:

```go
plg.Register[pricing.Rule](ctx, c, "pricing",
	plg.WithStorage(storage),
	plg.WithPool(2, 16),          // instances per plugin: 2 kept warm, at most 16
	plg.WithMemoryLimit(32 << 20), // each instance's memory; default 128 MiB for Go, 64 MiB for Lua and TypeScript
)
```

- **Memory:** each instance's linear memory is capped (`WithMemoryLimit`, rounded down to 64 KiB pages; a
  plugin's `memoryLimit` may lower it). A call that needs more fails with a `PanicError` and its instance is
  replaced; others are unaffected.
- **CPU and time:** every call stops when its `ctx` ends (deadline or cancellation), even in an infinite loop or
  a Go plugin's `time.Sleep` (or any wait for a timer), and that instance is replaced. `Create` stops the same way
  at Register's `ctx`, and at `Container.Close` when the pool was starting it. Always call plugins with a deadline.
- **Pool:** `WithPool(min, max)` (default `1, GOMAXPROCS`; `1 ≤ min ≤ max ≤ 4096`). Register starts `min`
  instances of each plugin (so a broken `Create` fails Register), a background goroutine keeps `min` warm and
  replaces discarded ones (retrying a failing `Create` with backoff from 100 ms to 30 s), and a call takes an idle
  instance, starts one while under `max`, or waits on its `ctx`.
- **Budget:** `plg.New(plg.WithMemoryBudget(total))` caps all instances of the container together. Register counts
  each plugin's worst case (`max × limit`) and fails with `plg.ErrMemoryBudget` when it does not fit; pools never
  grow past the budget.

Size pools for I/O: an instance making an HTTP request or a Host call is busy until it returns, unless
`WithConcurrentCalls` lets it serve other calls meanwhile (see [HTTP](#http)). A Lua or TypeScript instance costs
about 3 MiB of host memory (its linear memory starts with a 2 MiB stack); a Go instance ≈ 29 MiB.

## HTTP

Off by default: every request is refused. Per Register:

```go
plg.WithHTTP(plg.HTTPPolicy{
	AllowedHosts: []string{"api.example.com", "*.cdn.example.com", "10.0.0.7:8443"}, // required
	// Defaults: BlockPrivateNetworks true, Timeout 10s, MaxRequestBytes 1 MiB, MaxResponseBytes 4 MiB,
	// MaxRequestsPerCall 16.
})
```

Go plugins use `net/http` (`http.Get`, `http.Client`, …), TypeScript plugins `fetch`, Lua plugins
`http.request{method = "POST", url = "...", headers = {...}, body = "..."}` (returning
`{status, headers, body}` or `nil, reason`). The host makes every request: it checks the allowlist (names,
wildcards, IPs, optional ports; http and https only), refuses private, loopback, link-local and metadata
addresses after DNS resolution, including IPv6 addresses embedding them (mapped, `64:ff9b::/96` NAT64, 6to4)
and the whole local-use NAT64 range `64:ff9b:1::/48`, whose embedding layout cannot be known
(`BlockPrivateNetworks`; set it to `new(false)` to reach them), never uses an
environment proxy, bounds sizes and time (never past the call's `ctx`), and logs and counts each request with the
plugin's key. Redirects are followed by the plugin's side, each hop checked like the first. Plugins get requests
and responses, not tunnels or upgraded protocols: `CONNECT` is refused, and a `101 Switching Protocols` answer
(WebSocket, h2c) fails the request with `plg http: protocol upgrade refused`, its connection closed at once.

Credentials stay with the origin they were set for, in every language: once a redirect leaves the first request's
origin — its scheme, host and port (a URL without a port has its scheme's default; names compare in any case) —
that hop and every later one go without the headers carrying credentials (`Authorization`, `Cookie`, `Cookie2`,
`Proxy-Authorization`, `Proxy-Authenticate`, `WWW-Authenticate`), even if a later redirect comes back. So an
`https` request's token never follows a redirect to `http`, another port, another host or a subdomain, wildcard
entries of `AllowedHosts` included. This is stricter than Go's `http.Client`, which keeps them for the same host
on any port or scheme and for its subdomains; Go plugins get the stricter rule too. The plugin's own headers (the
Go request's `Header`, `fetch`'s `headers`, `http.request`'s `headers` table) are left as they were.

A Go plugin's request whose own context (`http.NewRequestWithContext`) has already ended is not sent: it fails with
the context's error (`context.Canceled`, `context.DeadlineExceeded`) and its body is closed. Once sent, a request
runs under the call's `ctx` and the policy's `Timeout` only: a shorter deadline or a cancellation of the request's
own context does not stop it on the host yet (an `http.Client.Timeout` likewise ends the plugin's wait, not the
host's request).

### Many I/O-bound calls on one instance: `WithConcurrentCalls`

By default an instance serves one call at a time, so a call waiting 100 ms for a response holds a whole instance.
For Lua and TypeScript plugins, `plg.WithConcurrentCalls(n)` (per Register, 1 to 4096; default 1) lets each
instance serve up to `n` calls at once: while a call awaits a `fetch` or waits in `http.request` (which yields its
coroutine), the instance runs other calls. With 256 callers each awaiting a 100 ms response, one instance with
`WithConcurrentCalls(256)` served as many calls per second as a pool of 256 blocking instances (≈ 2,350/s) with
about 1/67 of the host memory (≈ 12 MiB instead of ≈ 800 MiB). Go plugins ignore the option.

```go
plg.Register[pricing.Rule](ctx, c, "pricing",
	plg.WithStorage(storage),
	plg.WithPool(1, 4),
	plg.WithConcurrentCalls(64), // up to 4 × 64 calls in flight; an instance counts once in the pool and budget
	plg.WithHTTP(policy),
)
```

Calls on one instance share it as concurrent requests share a Node.js process:

- They never see each other's arguments or results, and each keeps its own `MaxRequestsPerCall`. A request the
  policy refuses whatever it holds (HTTP disabled, over `MaxRequestBytes`, past `MaxRequestsPerCall`) is refused
  at once with the same error as without `WithConcurrentCalls`, never copied into host memory.
- They **do share the plugin's global state** (module-level variables in TypeScript, globals and upvalues in Lua,
  whatever `Create` built), which may change while a call waits, and the instance's memory limit.
- Each call keeps its own `ctx`: one that ends while the call waits for a response fails alone, at once (even
  while another call's code runs on the instance), with the context's error; the instance serves on (the plugin
  sees the request fail and gets 250 ms in all to finish, else the instance is discarded). A TypeScript call
  awaiting nothing of its own (no `fetch` in flight: a promise only another call, or nothing, settles) is dropped
  from the instance at once instead, so calls left behind do not pile up on it.
- A call that traps, runs out of memory, panics or is stopped while running discards the instance: it fails
  with a `PanicError`, every other call on that instance with `plg.ErrInstanceLost` (safe to retry).
- Only HTTP requests let other calls run; a Host call, and a Lua request made where the call cannot yield (inside
  the plugin's own coroutine or a C callback such as `table.sort`'s comparator), block the instance as before.

For gRPC or anything else, expose the operation as a [Host](#calling-back-into-the-service-the-host-interface)
method.

## Calling back into the service: the Host interface

Declare a second interface in the contract package, following the same rules, and pass an implementation:

```go
// in package pricing
type Customers interface {
	Tier(ctx context.Context, customer string) (string, error)
}

plg.Register[pricing.Rule](ctx, c, "pricing", plg.WithStorage(s), plg.WithHost[pricing.Customers](impl))
```

Go plugins receive it in `Create(customers pricing.Customers)`; Lua and TypeScript plugins call the global
`host.Tier(...)`. The host calls `impl` with the `ctx` of the plugin call in progress (Register's while `Create`
runs). An error it returns reaches the plugin with the same message; a panic in it is recovered and reaches the
plugin as an error, and the service keeps running. `impl` is called concurrently by a plugin's instances, so it
must be safe for concurrent use, and it must not wait on a call to the same plugin (that instance is busy).

## Reloading and polling

A registration follows its storages:

```go
err := c.Reload(ctx)                          // every registration
err = plg.ReloadInterface(ctx, c, "pricing")  // one
plg.WithPollInterval(30 * time.Second)        // Register option: reload by itself
```

A reload lists the storages and loads only the plugins that are new or whose descriptor or version changed, then
publishes every change at once:

- a plugin no longer listed is removed (`Get` and calls through it fail with `plg.ErrNotFound`);
- a new plugin that fails to load is skipped; an updated one that fails keeps serving its last good version
  (both logged, counted and returned, joined);
- a storage that cannot be listed changes nothing.

Calls running on a replaced version finish there; afterwards its instances are closed (running the plugin's
`Close`). Values obtained from `Get` keep working across reloads: they call the key's current version. Register,
by contrast, is all or nothing: any failure returns an error and publishes nothing.

## Stability guarantees and `PanicError`

The host process survives anything a plugin does. Each failure that is not the plugin's own error is a
`*plg.PanicError`:

```go
type PanicError struct {
	Value    any       // the recovered value, the guest's panic message or the trap
	Time     time.Time
	Key      string    // the plugin key, the Register name when no plugin is involved, "" for New and Close
	Language plg.Language
	Phase    plg.Phase // load, create, call, host, http, storage, close, poll, refill or callback
	Method   string
	Guest    string    // what the guest wrote to stderr: the Go plugin's panic trace, the runtime's fatal error, ...
	Stack    []byte    // the host stack, for failures recovered on the host
}
```

- **Inside the guest:** a panic, out of memory, a stack overflow, a runtime abort (`select {}`), a call stopped at
  its deadline, a broken result: the call fails with a `PanicError`, that instance is discarded, the pool replaces
  it. `errors.Is(err, plg.ErrCall)` holds for every failed call, and `errors.Is(err, context.DeadlineExceeded)`
  for a deadline stop.
- **On the host:** every boundary where plg runs code it does not control is behind `recover`: storages, esbuild,
  the codec, Host methods, the HTTP transport, pollers and refillers, your callbacks, your logger's handler (the
  plugins' own output included) and your meter provider (a panic there is a `PanicError` in phase `callback`;
  `New`, `Register`, the reload, the call, the pool and its instances carry on).
- **Reporting:** each `PanicError` is logged at Error level, counted (`plg.plugin.panics`), passed to
  `plg.WithOnPanic(fn)` and kept in `c.RecentPanics()` (the last 64, `plg.WithPanicHistory(n)`).

```go
_, err := rule.Discount(ctx, order)

var pe *plg.PanicError
if errors.As(err, &pe) {
	slog.Error("rule crashed", "key", pe.Key, "phase", pe.Phase, "guest", pe.Guest)
}
```

## Logs and metrics

- **Logs:** `plg.New(plg.WithLogger(logger))` (a `*slog.Logger`; discarded by default) gets loads, reloads,
  skips, failures, panics, pool backoffs, HTTP requests and the plugins' own output (stdout at Info, stderr at
  Warn, with the plugin key).
- **Metrics:** OpenTelemetry, through `plg.WithMeterProvider(mp)` (default: the global provider): calls, latency
  and errors by key, method and outcome; panics by phase; pool instances, instances in use (serving at least one
  call), calls in flight (more than instances in use only with `WithConcurrentCalls`), waits and refills; instance
  memory; loads, skips and reload failures; HTTP requests, latency and sizes. Names and attributes are listed on
  `plg.WithMeterProvider`. Recording on the call path does not allocate.

## Deployment: `plg warm` and `plg build-guest`

Guests are compiled to machine code at startup and cached on disk (default `$PLG_CACHE_DIR`, else
`os.UserCacheDir()/plg/<wazero version>`; `plg.WithCompilationCacheDir(dir)` overrides it; an unwritable
directory falls back to memory with a warning). Compile your image warm:

```dockerfile
ENV PLG_CACHE_DIR=/var/cache/plg
RUN go run github.com/heynemann/go-plg/cmd/plg warm
```

`plg warm` (or `plg.Warm(ctx)` from your own binary) compiles every linked guest into that directory. The cache
is keyed by the guest, the wazero version and the CPU, so a mismatch is just a cold compile, never an error.

To let Go plugins import more packages than the allowlist, build a custom Go guest from your service module and
pass it per Register:

```sh
go run github.com/heynemann/go-plg/cmd/plg build-guest -pkg encoding/xml -pkg github.com/acme/money -o guest.wasm
```

```go
//go:embed guest.wasm
var customGuest []byte

plg.Register[T](ctx, c, "name", plg.WithStorage(s), plg.WithGoGuest(customGuest))
```

Extra packages must build for wasip1 without cgo. yaegi names each package's symbol file after its import path with
`/` and `-` both mapped to `-`, `.`, `~` and `_` to `_`, so one guest cannot have two packages whose paths differ only
there (`example.com/a-b` and `example.com/a/b`): `plg build-guest` refuses them. A custom guest runs under the same limits and checks, but is not
covered by `plg warm`: its first compile happens at startup.

## Performance

Measured on go1.27.3, linux/amd64 (Ryzen 9 9950X3D, 32 threads), no race detector, **every protection on**
(memory limit, CPU cap, recover boundaries). Ranges are the spread over runs (see `PLAN.md` for the method and
the history).

| | Go | Lua | TypeScript |
|---|---|---|---|
| Call, one string in and out | 118–126 µs | 6.1–6.6 µs | 2.0–2.4 µs |
| Call, nested struct both ways | 287–296 µs | 32–33 µs | 26–28 µs |
| Call making one Host call | 210–225 µs | 6.4 µs | 4.3–4.7 µs |
| Call making one HTTP request (loopback) | 0.82–0.89 ms | 0.21 ms | 0.39–0.40 ms |
| Throughput per call, 32 goroutines, `WithPool(32, 32)` | 4.5–4.7 µs | 1.5 µs | 1.2–1.3 µs |
| Allocations per call (host side) | 6 | 7 | 6 |
| Memory of an instance (host heap) | ≈ 29 MiB | ≈ 3 MiB | ≈ 3 MiB |
| 256 callers awaiting a 100 ms HTTP response | — (one call per instance) | 2,360 calls/s in 11 MiB (1 instance, `WithConcurrentCalls(256)`) vs 2,360/s in 760 MiB (256 blocking) | 2,330/s in 12 MiB vs 2,430/s in 800 MiB |
| Starting an instance | 40–60 ms | < 1 ms | ≈ 2 ms |
| Guest compile, cold / from the disk cache | 4.3–5.1 s / 0.16–0.22 s | 70–85 ms / 4 ms | 0.28 s / 13 ms |

| Catalogue | |
|---|---|
| `plg.Get` | 11–13 ns, 0 allocations (budget ≤ 50 ns, enforced in CI) |
| `plg.All`, two plugins | 18–25 ns, 0 allocations (enforced in CI) |

Go plugins are interpreted inside WebAssembly with the CPU cap on: about 20–50× slower per call and about 10×
heavier per instance than Lua or TypeScript. Prefer Lua or TypeScript for hot paths; Go plugins suit plugin
authors who want Go, at a call rate where 0.1 ms per call is acceptable.

## Limitations and residual risks

- **Host memory:** instance memory is ordinary Go heap. After a large instance closes, the process's RSS stays
  high until the Go runtime returns memory to the OS.
- **Lua depends on wazero's experimental exception handling** (Lua's errors are setjmp/longjmp compiled to wasm
  exceptions). It is pinned and covered by tests, but experimental upstream.
- **Deadline stops have no traceback:** a plugin stopped at its deadline reports only what it printed to stderr.
- **Polling** lists every storage each interval (no change notifications); plugins without a version are hashed
  on every poll. A plugin's `Close` does not run for instances still busy when `Container.Close`'s `ctx` ends.
- **Go plugins:** goroutines started indirectly by allowed packages (`time.AfterFunc`) are paused between calls;
  yaegi does not support every Go construct (for example `for range n` over an integer); deep recursion runs out
  of memory instead of failing gracefully. With a custom guest, interpreted types are not seen as implementing the
  extra packages' interfaces (e.g. `xml.Marshaler`).
- **Lua:** an uncaught out-of-memory error carries no traceback; `json.encode` writes integral floats without a
  fraction and an empty table as `{}` (use `json.array()`).
- **TypeScript:** bundles are not source-mapped (stack traces point into the bundle); recursion stops at about
  3000 frames with a `RangeError`; a variadic parameter arrives as one array; a response header's values are
  joined into one, as in `fetch`.
- **`WithConcurrentCalls`:** interleaved calls share the plugin's global state and memory limit; a call abandoned
  at its deadline (or finished with requests still in flight) still runs its remaining code, within 250 ms in all
  from its end however many steps that takes: a call still running then, say retrying a failing request in a loop,
  discards the instance (its other calls fail with `plg.ErrInstanceLost`); an abandoned TypeScript call with no
  `fetch` in flight is dropped at once, its result discarded, though code awaiting what another call settles later
  still runs then; a Lua request that falls back to blocking counts against `MaxRequestsPerCall` per step rather
  than per call; in TypeScript a request belongs to the call whose step made it, so a promise shared between calls
  through global state is attributed to the call that started it.
- **Rebuilds:** the guest artifacts must be rebuilt and re-verified (`make verify-guests`) whenever wazero,
  wasi-sdk, Lua, QuickJS-ng, yaegi or Go are upgraded.

## Examples

[`examples/`](examples) is a separate Go module, so plg's own `go.mod` never gains aws-sdk, pgx or testcontainers:

- [`examples/pricing`](examples/pricing): a contract (`Rule`, the `Customers` host) and its generated adapter.
- [`examples/plugins`](examples/plugins): three rules, in Go (`bulk`), Lua (`loyalty`, calls the host) and
  TypeScript (`coupon`, throws on an unknown code).
- [`examples/service`](examples/service): an HTTP pricing service serving those rules, with a host, a memory
  budget, per-call deadlines, optional polling of a plugin directory, and a clean shutdown:
  `cd examples && go run ./service`.
- [`examples/s3storage`](examples/s3storage): a `plg.Storage` over S3, tested against MinIO.
- [`examples/pgstorage`](examples/pgstorage): a `plg.Storage` over PostgreSQL listing only enabled plugins, with
  its migration, tested against PostgreSQL.
- [`examples/quickstart`](examples/quickstart): runs this README's quick start.

Their tests start MinIO and PostgreSQL with [testcontainers](https://golang.testcontainers.org/), so they need
Docker: `make examples`.

## Development

- `make check` runs every gate CI runs: formatting, golangci-lint (host and wasip1), the unit and integration
  tests with `-race`, the 95% per-package coverage gate (on that run's profile; `scripts/gate-test.sh` tests it), allocation and benchmark budgets, a `-count=10` repeat
  of the unit tests, and `make examples` (lint and tests of the examples module, Docker required).
- `make guests` rebuilds the guest artifacts with pinned toolchains; `make verify-guests` checks that the
  committed ones are reproducible.
- [`RULES.md`](RULES.md) holds the engineering rules every change follows; [`PLAN.md`](PLAN.md) the design and
  the log of decisions and measurements.
