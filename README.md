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
| TypeScript (and JavaScript) | [QuickJS-ng](https://github.com/quickjs-ng/quickjs), compiled with wasi-sdk; [esbuild](https://github.com/evanw/esbuild), compiled to wasip1, bundles the plugin in a sandbox of its own |

**Stability comes first: a plugin cannot take down the host process.** Panics, running out of memory, stack
overflows, infinite loops and runtime aborts inside a plugin become errors on that plugin's call; the faulty
instance is thrown away and replaced. Plugins see no filesystem beyond their own files, no environment and no
network; they reach the outside world only through what the service grants them (a typed Host interface, an
HTTP allowlist).

- [Concepts](#concepts)
- [Quick start](#quick-start)
- [The interface](#the-interface)
- [Code generation](#code-generation) and [contract directives](#contract-directives)
- [Writing plugins](#writing-plugins) ([Go](#go-plugins), [Lua](#lua-plugins), [TypeScript](#typescript-plugins))
- [Guests and blank imports](#guests-and-blank-imports)
- [Declaring the interface: `implements`](#declaring-the-interface-implements) and
  [checking a plugin before registering it: `plg validate`](#checking-a-plugin-before-registering-it-plg-validate)
  with [test cases](#test-cases-testsyaml),
  also of [author repositories (`plg.yaml`)](#author-repositories-plgyaml),
  with [typings for Lua and TypeScript: `plg types`](#typings-plg-types),
  [starting and iterating: `plg init` and `plg dev`](#starting-and-iterating-plg-init-and-plg-dev), and
  [packing a plugin: `plg pack`](#packing-a-plugin-plg-pack)
- [Storages](#storages)
- [Ownership](#ownership) and [Validation](#validation)
- [Limits, pools and the memory budget](#limits-pools-and-the-memory-budget)
- [HTTP](#http)
- [Calling back into the service: the Host interface](#calling-back-into-the-service-the-host-interface)
- [Secrets](#secrets)
- [Reloading and polling](#reloading-and-polling)
- [Stability guarantees and `PanicError`](#stability-guarantees-and-panicerror)
- [Logs, metrics and traces](#logs-metrics-and-traces), including
  [`plg status` and `plg events`](#from-a-terminal-plg-status-and-plg-events) from a terminal
- [Putting it together: fit to serve, and who to talk to](#putting-it-together-fit-to-serve-and-who-to-talk-to)
- [Author guide: from a contract to a served plugin](#author-guide-from-a-contract-to-a-served-plugin) and
  [platform guidance](#platform-guidance): publishing artifacts, contracts for large data, side effects
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

Write the plugins, one directory each, with a `plugin.yaml` naming the interface the plugin implements (with the
version of the contract module it is written against), its language and its entrypoint. In Go:

<!-- quickstart:file plugins/hello-go/plugin.yaml -->
```yaml
implements: example.com/quickstart/greeter.Greeter@v1.0.0
language: go
entrypoint: .
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

<!-- quickstart:file plugins/hello-lua/plugin.yaml -->
```yaml
implements: example.com/quickstart/greeter.Greeter@v1.0.0
language: lua
entrypoint: plugin.lua
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

<!-- quickstart:file plugins/hello-ts/plugin.yaml -->
```yaml
implements: example.com/quickstart/greeter.Greeter@v1.0.0
language: typescript
entrypoint: plugin.ts
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
(see [`plg warm`](#deployment-plg-warm-and-plg-build-guest)), so later runs start in a fraction of a second. Here
the interface lives in the service's own module, which `go run` builds from the local checkout, so the plugins'
pinned version cannot be checked: each load logs a `plg.contract.devel` warning (see
[Declaring the interface](#declaring-the-interface-implements)). This quick start is tested: `examples/quickstart`
copies the files above out of this README into a fresh module and checks the output.

## The interface

Register checks the interface by reflection (and the Host interface, if any) and refuses one that breaks these
rules with `plg.ErrInvalidInterface`:

- **Method shape:** every method is `M(ctx context.Context, args...) (results..., error)`: a context first and an
  error last, both required. Variadic methods are allowed.
- **Data only:** `bool`, integers, floats, `string`, `[]byte`, slices, maps with string keys, structs whose
  fields are all exported, pointers to structs, `time.Time`, and named types of these (recursive types included).
  Functions, channels, interfaces, arrays, complex numbers and `uintptr` are refused. One limit is Go plugins'
  alone: the Go interpreter lays out wrongly a struct holding by value a struct that holds it back through a
  slice or map (`Child{Parent Parent}` with `Parent{Children []Child}`) when it reaches the outer one first,
  and the Go guest then refuses the plugin (`does not fit in the struct as the Go interpreter laid it out`)
  rather than corrupt memory; a contract whose methods, sorted by name, reach `Child` first works.
- **Embedded aliases:** an anonymous struct may embed an alias (`struct{ Embedded }` with `type Embedded =
  Counter`): the field keeps the alias's name, so Go plugins bind to exactly the contract's type. The Go
  dispatcher declares one alias per name, so all such fields embedding an alias named `Embedded`, in the
  interface and its Host, must denote the same type, and the name cannot be `PlgInit`, `PlgClose` or start with
  `PlgCall_` or `PlgHostCall_`: otherwise registering the contract fails with `plg.ErrInvalidContract`, or
  Register with `plg.ErrInvalidHost` when the Host brings the clash.
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

### Contract directives

The interface's doc comment may carry plg directives, Go-style comments with no space after the slashes (godoc
hides them):

```go
// Rule decides a reward.
//
//plg:limits memory=64MiB timeout=2s pool=1..8
//plg:guest-packages encoding/xml
//plg:validation RuleInput
type Rule interface {
	Apply(ctx context.Context, user string) (Reward, error)
}
```

- **`//plg:limits`** states the limits the interface's plugins should be tested under, as `key=value` pairs, each
  at most once: `memory` (bytes, or a number of `B`, `KiB`, `MiB` or `GiB`, from 64 KiB to 4 GiB, as
  `WithMemoryLimit` takes it), `timeout` (a positive Go duration) and `pool` (`MIN..MAX`, or `N` for `N..N`, with
  1 ≤ MIN ≤ MAX ≤ 4096, as `WithPool` takes it). [`plg validate`](#checking-a-plugin-before-registering-it-plg-validate)
  runs plugins under them; a service still sets its own limits with its Register options.
- **`//plg:guest-packages`** lists packages beyond the Go guest's allowlist that the interface's Go plugins may
  import (standard library packages, or pure-Go packages of modules a plugin's module requires), checked as
  [`plg build-guest`](#deployment-plg-warm-and-plg-build-guest) checks its `-pkg`s. `plg validate` builds that custom
  guest for such plugins; a service serving them builds it with `plg build-guest` and passes it to `WithGoGuest`.
- **`//plg:validation`** names the data type, declared in the contract package, of the input the registration's
  `WithValidation` passes to plugins' `Validate`.

`plg gen` checks the directives of the interfaces it generates for: an unknown directive or key, a directive or key
given twice, or a malformed value fails generation, naming the declaration (`rules.go:12:6: Rule: //plg:limits
memory=64MB: invalid plg directive: memory=64MB: want bytes or a number of B, KiB, MiB or GiB from 64KiB (one wasm
page) to 4GiB, as in 64MiB`).

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

- The guest's interpreter (yaegi v0.16.1) loads the package's `.go` files but these: `_test.go` files; names
  starting with `_` or `.`; names whose `_`-separated elements after the first `_` are a single operating system or
  architecture yaegi knows other than `wasip1`/`wasm` (`x_windows.go`, `x_amd64.go`), or end in such an architecture
  (`x_y_arm64.go`, `x_wasip1_amd64.go`) or in such an operating system followed by any architecture yaegi knows
  (`x_linux_wasm.go`); and files whose `// +build` lines before the package clause do not all hold (the tags that
  hold are `wasip1`, `wasm` and `go1.N` up to the Go release that built the committed guest,
  `gorestrict.GuestGoMinor`, pinned by a test to the Makefile's `GOTOOLCHAIN`; a guest `plg build-guest` builds with
  another Go can load `go1.N` files differently). yaegi knows fewer names than `go build` (operating
  systems `aix android darwin dragonfly freebsd illumos ios js linux netbsd openbsd plan9 solaris windows`,
  architectures `386 amd64 arm arm64 loong64 mips mips64 mips64le mipsle ppc64 ppc64le s390x`) and reads an operating
  system as the last of several elements as no constraint, so `x_riscv64.go`, `x_hurd.go`, `x_linux_riscv64.go` and
  `x_y_windows.go` are loaded. It reads no `//go:build` line: a file only `//go:build` excludes
  (`//go:build ignore`) is loaded, so add `// +build ignore` below it. Register's go-statement check and
  `plg validate` (its `sandbox` and `types` steps, and the Host it reads from `Create`) read the same files. Since a
  Go plugin is plain Go, unit-test it natively (as `examples/plugins/bulk` does).
- The package name does not matter (`package main` works too).
- **Standard library allowlist:** `bufio bytes cmp context encoding/base64 encoding/hex encoding/json errors fmt io
  maps math math/big math/bits math/cmplx math/rand math/rand/v2 net/http (client side, through the host) net/url
  regexp slices sort strconv strings sync sync/atomic time unicode unicode/utf16 unicode/utf8`, plus the contract
  package and `github.com/heynemann/go-plg/plugin` (the `Issue` a [`Validate`](#validation) returns and
  `plugin.Secret`, which reads a [secret](#secrets)). Anything
  else fails Register with `plg.ErrLoad`. To allow more packages (`encoding/xml`, a pure-Go library), build a
  custom guest with [`plg build-guest`](#deployment-plg-warm-and-plg-build-guest).
- **No goroutines:** a `go` statement fails Register with `plg.ErrGoStatement` (file:line:col). A WebAssembly
  instance has a single thread, and guest goroutines would be paused between calls; parallelism comes from the
  instance pool.
- **Vendored packages:** the packages of a `vendor/` directory in the package's directory are importable by
  their import paths, as Go vendors packages (`vendor/example.com/team/lib` is `import "example.com/team/lib"`),
  under the same allowlist; they may not take the contract package's import path. `plg validate` puts an author
  repository's [shared directories](#author-repositories-plgyaml) there.

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
  error), `http.request{...}` and `secrets.get(name)` (see [Secrets](#secrets)). `require` loads `.lua` files of
  the plugin's own directory (mounted read-only
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
  the plugin's own files; package, URL and absolute imports fail Register naming `file:line:col`. At load,
  TypeScript is transpiled, not type-checked; `plg validate` type-checks it against the plugin's
  [typings](#typings-plg-types).
- Bundling runs in a WebAssembly sandbox of its own (esbuild compiled to wasm, shipped in the TypeScript guest),
  never on the host: a plugin can make esbuild work exponentially long with a few hundred bytes (nested
  `export *` layers, deeply nested arrow functions), so a bundling stops when the load's context ends and fails
  the load at its limits: 512 MiB of memory and 10 s plus 5 s per MiB of the plugin's files ("bundling took
  longer than its time limit"). It costs about 40 ms for a small plugin; esbuild in wasm is 50 to 90 times
  slower than natively, about 1 s for 600 KB of source.
- Several results come back as an array; a `throw` or a rejected promise is the call's error.
- Globals: the ECMAScript built-ins, `console` (to the service's log), `host` (Host methods return Promises),
  `fetch` and `secrets` (`secrets.get(name)`, see [Secrets](#secrets)). No `require`, `process`, `std` or `os`.
- `[]byte` is a `Uint8Array`, `time.Time` is `{seconds, nanos}` (a `Date` is accepted back), nil is `null`.

## Guests and blank imports

Each language's guest is a package embedding its WebAssembly module; a blank import links it and makes the
language available:

```go
import (
	_ "github.com/heynemann/go-plg/guest/golang"     // Go plugins (≈ 5 MB, compressed)
	_ "github.com/heynemann/go-plg/guest/lua"        // Lua plugins (≈ 0.3 MB)
	_ "github.com/heynemann/go-plg/guest/typescript" // TypeScript plugins (≈ 4.9 MB: QuickJS 1.2, esbuild 3.7 compressed)
)
```

Link only the languages you accept. A plugin whose guest is not linked fails Register with
`plg.ErrGuestNotLinked`, naming the import to add. To accept only some languages from a storage, pass
`plg.WithGolangPlugins()`, `plg.WithLuaPlugins()` and/or `plg.WithTypescriptPlugins()` to Register: plugins in
other languages are then skipped (logged and counted), not errors. Without any of them every language is enabled.

## Declaring the interface: `implements`

Every `plugin.yaml` names the interface its plugin implements, and the version of the contract module it is
written against:

```yaml
implements: example.com/contracts/rewards.Cohort@v1.4.0
```

The value is `<package path>.<Interface>@<version>`: the import path of the interface's package (a valid import
path), the interface's name (a Go identifier) and a canonical semantic version with its leading `v` (`v1.4.0`,
`v2.0.0-rc.1` or a pseudo-version; not `1.4.0`, `v1.4` or `v1.4.0+meta`). It is required: a manifest without it,
or with a malformed value, is `plg.ErrInvalidManifest`, and a `Descriptor` without it is
`plg.ErrInvalidDescriptor`. `Descriptor.Implements` carries it; changing it is a new descriptor, so a reload
loads the plugin again.

**Which plugins a Register takes.** A Register loads only the plugins declaring its own interface: the package
path and name of `T`, compared exactly (for a `T` in an external test package, plugins name that package,
`foo_test`, not `foo`). The others are skipped, not errors, so one storage can hold the plugins of several
interfaces and serve several registrations. The interface is checked first: a plugin of another interface is
skipped with `plg.ErrOtherInterface`, one of this interface in a language not enabled with
`plg.ErrUnsupportedLanguage`. Either way the skip is logged (each reason its own message, at Info at Register and
Debug at a reload), counted in `plg.plugin.skipped` with a `reason` attribute (`interface` or `language`), emitted
as a Skipped event and shown as status `skipped` whose `Rejection` report's `Err` is the reason.

**Version rule.** The service's contract version is the version of the module providing `T`'s package in the
program's build info (`runtime/debug.ReadBuildInfo`; a module replaced by another version counts as that version).
A plugin loads when its pinned version has the same major version and a minor version not newer: a service built
with `v1.4.0` loads plugins pinned to `v1.0.0` … `v1.4.x`, and rejects `v1.5.0` (it may rely on methods or fields
the service lacks) and `v2.0.0` with a `plg.contract.version` error issue (`plg.CodeContractVersion`) naming both
versions, at Register, at a reload and in `plg.Check`. When the version cannot be told, because the contract module
is built from a local checkout (a `replace` pointing at a directory, tests and `go run`, or the service's own
module unless it is built at a tag with no uncommitted changes: the go command stamps an untagged commit with a
pseudo-version and changes with `+dirty`) or is missing from the build info, the plugin loads with a
`plg.contract.devel` warning issue (`plg.CodeContractDevel`).

## Checking a plugin before registering it: `plg validate`

`plg validate <plugin dir>` checks a plugin on the author's machine, before any service sees it, with plg's own
loading code and the real guests (the command links all three); `plg validate <repository>` checks every plugin of
an [author repository](#author-repositories-plgyaml):

```sh
go run github.com/heynemann/go-plg/cmd/plg validate plugins/gold
```

```
STEP       RESULT  DETAIL
manifest   ok      go plugin gold implementing example.com/rewards/cohort.Provider@v1.2.0
contract   ok      module example.com/rewards v1.2.0 (/home/ana/go/pkg/mod/example.com/rewards@v1.2.0)
interface  ok      cohort.Provider: IsMember, List; Host cohort.Users: Tier (a stub: every call fails)
limits     ok      memory=64MiB timeout=2s pool=1..8 (//plg:limits)
go.mod     ok      /home/ana/plugins/gold/go.mod requires example.com/rewards v1.2.0
sandbox    ok      no go statements, and imports only what the Go guest provides
types      ok      the plugin's package type-checks against cohort.Provider
guest      ok      a custom Go guest with encoding/xml, found in the guest cache (/home/ana/.cache/plg/guests/3f9c….wasm)
load       ok      Create ran in the go guest (version sha256:a753…)
test       ok      tests/members.yaml: gold members are in the cohort
test       ok      tests/members.yaml: the directory's error is the call's
tests      ok      2 passed
resources  ok      tests/members.yaml: gold members are in the cohort: memory 9.4 MiB of 64.0 MiB (14%), time 2ms of 2s (0%)
resources  ok      tests/members.yaml: the directory's error is the call's: memory 9.4 MiB of 64.0 MiB (14%), time 1ms of 2s (0%)
```

The steps run in order (plg.yaml, manifest, shared, files, contract, interface, limits, then the language checks:
go.mod, sandbox, types and guest for Go plugins, typings and types for TypeScript, typings and syntax for Lua; then
load, Validate's examples, the test cases and the resource report) and stop at the first that fails, except that
once the plugin loads the examples, the test cases and the resource report all run. The command exits 1 when any
step failed (2 for a usage error) and 0 when there are only warnings, with `plg validate: <dir> is not a valid
plugin` on stderr, where plg's log (the plugin's own output, compiles, issues) also goes.

- **manifest:** `plugin.yaml`, a regular file (not a symbolic link: the plugin's files leave links out) of at most
  `plg.MaxManifestSize` bytes, is read with `plg.ParseManifest`; the plugin's key is its directory's name. Its
  ownership is checked here: a missing owner is a warning issue, or fails the step when the author repository sets
  `requireOwnership`.
- **files:** the plugin's files are copied once into memory, as a [storage](#storages) loads them (regular files,
  symbolic links inside the plugin left out, within plg's limits: `plg.MaxPluginFileSize` and the others), from
  the composed copy when the author repository has shared directories; the plugin's directory itself may be a
  symbolic link, or be reached through one. Every later step (sandbox, types, syntax, typings, load, test cases)
  reads that copy and never the directory, so they all see the same files, and a plugin over the limits fails here
  with `plg.ErrPluginTooLarge`, as its load would, before any file is read whole. The row shows only when the step
  fails (`plg pack` reports it as its `pack` step).
- **contract:** the contract module is resolved at the version `implements` pins, through the local `go` command
  (`go mod download -json`), so `GOPROXY`, `GOPRIVATE`, `GONOSUMDB`, `.netrc` credentials and the Go module cache
  work as for any Go build: once the module is in the cache, validating needs no network (`GOPROXY=off` works). The
  module providing the package is the one of longest path among the package path and its prefixes, as the go
  command picks it. Without a `go` command on the `PATH`, the step says so.
- **interface:** the contract package is type-checked with go/types as `plg gen` type-checks it (only the guest's
  allowlisted imports), and the interface and its Host are described for the Go dispatcher and the script guests'
  schema, exactly as a service describes them by reflection: the dispatcher is the same, byte for byte. The method
  types are those the Go interpreter (yaegi, with the Go guest's symbol tables) gives the contract's source, so a
  contract whose types the interpreter cannot build fails here, as a Go plugin of it would fail to load. The
  interface's [directives](#contract-directives) are read here too; a malformed one fails the step.
- **limits:** the limits the plugin runs under: an [author repository](#author-repositories-plgyaml)'s `limits`
  over the contract's `//plg:limits`, and plg's defaults for what neither sets (the plugin language's memory limit,
  a pool of 1 to GOMAXPROCS instances, no timeout); the row says where they come from (`memory=64MiB timeout=300ms
  pool=1..2 (plg.yaml memory=64MiB over //plg:limits memory=16MiB timeout=300ms pool=1..2)`). The memory limit is
  the check's `WithMemoryLimit`, and the timeout bounds `Create` (not the guest compile or bundling before it) and
  each [test case](#test-cases-testsyaml)'s `Create` and call (10s for test cases when no timeout is set). The
  pool is checked as `WithPool` checks it and printed, but changes nothing here: the check runs one instance.
- **go.mod** (Go plugins): the `go.mod` of the plugin's module (its directory's or the nearest above) must require
  the contract module at the pinned version, after its `replace` directives; a `replace` by a local directory is
  a warning, since its version cannot be told. When they differ, the step names the one to change, the older of
  the two (the code may use what the newer version adds, and the pin is the oldest contract a service may serve
  the plugin with): `raise the pin in …/plugin.yaml to …@v1.3.0` or `raise the requirement in …/go.mod to
  example.com/rewards v1.3.0 (go get example.com/rewards@v1.3.0)`.
- **types** (Go plugins): the plugin's package is type-checked against the contract, as the Go compiler would, so
  a method of the wrong signature fails with its file:line:column (the Go interpreter would accept the plugin and
  fail only once the method is called). Its vendored packages are type-checked with it; a plugin importing a
  package beyond the contract, its vendored packages and the standard library is not type-checked (a warning).
- **typings** (Lua and TypeScript plugins): the plugin's committed [typings](#typings-plg-types) must be exactly
  what `plg types` writes for the pinned contract: missing, edited or written for another version, the step fails
  naming the stale files and the command to run (`run plg types -host Users plugins/gold`).
- **types** (TypeScript plugins): the plugin is type-checked against its typings, with their `tsconfig.json`, by
  Microsoft's TypeScript compiler (typescript-go, compiled to WebAssembly and run in plg's engine, so validating
  needs neither Node.js nor npm): one row per error, at its file:line:column, e.g.
  `plugin.ts:4:51: Property 'Rank' does not exist on type 'Users'. (TS2339)`. What `Create` returns is checked
  against the interface even when nothing in the plugin says so: a method returning a string where the contract
  wants a bool fails with `plugin.ts:13:17: Create: Argument of type '() => Tiers' is not assignable to …` at the
  export of `Create` (annotating `Create(): cohort.Provider`, or a class with `implements cohort.Provider`, puts
  the error on the method itself). The compiler reads the plugin's files from the `files` step's copy, so it
  follows no symbolic link.
- **guest** (Go plugins of a contract with `//plg:guest-packages`): the custom Go guest with those packages, built
  as `plg build-guest` builds one, from the plugin's module (its `go.mod` and `go.sum`, local `replace`s made
  absolute; the module must provide the non-standard packages). The guest's plg is the version the `plg` command was
  built at (a command built from a local checkout has none: the module's own plg then, which its build list must
  have). The guest is kept in the guest cache (`-guest-cache`, default `<user cache dir>/plg/guests`) under a key of
  everything it is built from: the go version, the module's `go.mod`, the packages, and each linked package's module
  version, or the content of its files for a module replaced by a directory. The next run with the same inputs
  finds it there and builds nothing; being the same bytes, its compiled form is a compilation-cache hit too.
- **load:** the plugin is checked as [`plg.Check`](#checking-a-candidate-plgcheck) checks a candidate, against a
  registration of the resolved contract (its module's version is the one the pin is checked against, by the same
  rule as at Register): the snapshot limits, the guest's own checks, then `Create` in one throwaway instance of the
  real guest (ownership is the manifest step's). The plugins get a stub Host whose
  every method fails with `plg validate runs plugins without the service's Host: <Host>.<Method> was called`.
  A Go plugin's `Create` names its Host. Lua and TypeScript plugins name none: their Host is the one their typings
  were written for (the `Host:` line of their header); without typings, when the contract package declares exactly
  one other interface that passes plg's Host checks, it is their Host; with several, a failed `load` row is
  followed by the hint `pass -host <A|B>`, and `-host <Interface>` (an interface of the contract package) picks one.
  Issues follow the `load` row, one row each.
- **sandbox** (Go plugins, before `types`): the plugin may not start goroutines (a `go` statement, which Register
  rejects too) nor import what the Go guest cannot provide: only the guest's allowlisted
  standard library, the contract package, its `vendor/` packages and the contract's `//plg:guest-packages`. Each
  offending statement or import is named at its file:line:column, with a hint.
- **syntax** (Lua plugins): every `.lua` file of the plugin is compiled, without running it, by the real Lua
  guest, so a module only `require`d later, inside a call, is checked too (`lib/tiers.lua:4: 'end' expected …`).
- **validate / example:** each `examples/validation/*.yaml` file is an input of the plugin's `Validate`, decoded
  strictly (as test case inputs, `{$ref: …}` reading test fixtures) into the contract's `//plg:validation` type,
  one `example` row each: an issue of error severity fails it, a warning issue is a warning. Examples of a
  contract with no validation input are warnings, not used; the `validate` row sums them up, counting those not
  used apart (`0 example inputs pass, 2 not used`; `no example inputs` without).
- **resources:** after the [test cases](#test-cases-testsyaml), one row per case: the peak memory of its instance
  (Create's included; WebAssembly memory only grows) against the memory limit, and the call's time against its
  timeout. A case at or above `-near-limit` of either (a share, default 0.8) is a warning:
  `resources  warning  tests/memory.yaml: a big string: memory 14.1 MiB of 16.0 MiB (88%), time 3ms of 300ms (1%);
  at or above 80% of its memory limit`.

**Output.** The table is coloured (`-color auto|always|never`; `auto` colours only a terminal, and never with
`NO_COLOR` set or `TERM=dumb`). `-format json` prints, instead of the tables, a JSON report whose schema is
stable: fields are only added within a `version`; removing or changing one raises it.

```jsonc
{
  "version": 1,
  "valid": false,                      // every plugin valid
  "repository": "/repo/plg.yaml",      // only for an author repository
  "plugins": [{
    "plugin": "rules/gold", "dir": "/repo/rules/gold",
    "valid": false, "failedStep": "test", "warnings": 2,
    "steps": [                         // in the order they ran; a test case or an example is one step each
      {"step": "manifest", "result": "warning", "detail": "lua plugin gold implementing …",
       "issues": [{"severity": "warning", "code": "plg.ownership.missing", "message": "no owner", "field": "owner"}]},
      {"step": "test", "result": "failed", "subject": "tests/a.yaml: silver members",
       "detail": "IsMember returned other results", "lines": ["result: expected true, got false"]},
      {"step": "load", "result": "failed", "detail": "…", "hint": "if Create takes a Host, pass -host <A|B>"}
    ],
    "resources": [{"call": "tests/a.yaml: gold members", "nearLimit": ["memory"], "memoryBytes": 15728640,
                   "memoryLimitBytes": 16777216, "durationMs": 1.5, "timeoutMs": 300}]
  }]
}
```

`result` is `ok`, `failed` or `warning`; `subject`, `hint`, `lines`, `issues`, `failedStep`, `nearLimit` and
`resources` are left out when empty; `warnings` counts the warning issues and the warning steps holding none.
`-junit <file>` also writes the report as JUnit XML for CI: a `testsuite` per plugin and a `testcase` per step
(named `<step>` or `<step>: <subject>`), failed when the step failed, with its detail as the message.

`-cache-dir` sets the guests' (and the TypeScript compiler's) compilation cache, as for
[`plg warm`](#deployment-plg-warm-and-plg-build-guest); `plg warm -validate` compiles the TypeScript compiler
into it ahead of time (about 10 s cold, under a second from the cache).
`-guest-cache` the directory of the custom Go guests.

### Test cases: `tests/*.yaml`

Once the plugin loads, `plg validate` runs the language-neutral test cases in its `tests/*.yaml` files, each in a
fresh instance of the real guest (its `Create` run with the stubbed Host, then the one call), under the limits of
the `limits` row and in the custom Go guest of the `guest` row. The same files test a Go, a Lua and a TypeScript
plugin of the contract alike:

```yaml
httpPolicy:                      # optional: the plugins' HTTP policy (plg.HTTPPolicy); without it every request is refused
  allowedHosts: [coupons.example.com]
cases:
  - name: gold customers get 10% off
    call: Quote                  # a method of the contract, or Validate
    input:                       # every parameter, by name (Validate's is `in`, the contract's //plg:validation type)
      order: {$ref: fixtures/order.yaml}
    host:                        # Host stubs: the arguments given must match; result (a list for several) or error
      - {method: Tier, args: {customer: alice}, result: gold, times: 1}
    expect:
      result: {total: 900, discount: 100, reasons: [gold]}
  - name: an unknown coupon
    call: Coupon
    input: {code: NOPE}
    http:                        # HTTP stubs: method (GET by default) and URL; status, headers, body; times
      - {url: "https://coupons.example.com/codes/NOPE", status: 404}
    secrets: {coupon_token: t0k3n} # fakes of plugin.yaml's secrets; null makes the read fail
    expect:
      error: unknown coupon NOPE # a substring of the error the plugin returned or threw
```

- **input** values are YAML, decoded strictly against the contract's types: an unknown field, a value of the wrong
  kind, an overflowing number, bad base64 (`[]byte`) or a timestamp that is not RFC 3339 fails the case with its
  `file:line:col`, rather than becoming a silent zero. `{$ref: <path>}` reads a value from a fixture file, relative
  to the plugin's directory; fixtures may hold `$ref`s too (cycles and paths out of the directory are errors); the
  fixtures a case's `$ref`s take add up to at most 16 MiB, a fixture counting each time one takes it (each file at
  most 1 MiB).
- **host:** a Host call no stub matches fails the case (and returns an error to the plugin); `times` counts the
  calls a stub must answer (`times: 0`: never). **http:** a request no stub matches fails the case; the policy is
  enforced first, so a host it refuses never reaches a stub. Stubs never dial, so the private-network protection
  has nothing to check.
- **secrets:** each secret the call (or `Create`) reads must be faked; a read the case does not fake, or a fake of
  a name `plugin.yaml` does not declare, fails the case. Every read gets the case's value (no cache across cases).
- **expect:** `result` (a list when the method has several results), `error` (panics, traps and timeouts never
  match it), or `issues` (Validate's; fields left out match anything). Floats may be written `.nan`, `.inf` and
  `-.inf`; an expected `.nan` matches NaN.

Each case gets a row; a failed one is followed by what was expected and what came back, a line per differing field
of structs, lists and maps, and a `tests` row sums up, followed by the [`resources`](#checking-a-plugin-before-registering-it-plg-validate)
rows. Any failed case (or file that does not parse) makes the command exit 1:

```
test       ok      tests/pass.yaml: gold customers get 10% off
test       FAIL    tests/fail.yaml: another total: Quote returned other results than expect.result
                   result.total: expected 1000, got 900
                   result.reasons: expected ["gold", "gift"], got ["gold"]
test       FAIL    tests/fail.yaml: a timeout: Spin failed: it did not return within 3s: …
tests      FAIL    test cases failed: 2 failed, 1 passed
```

When every case passes the `tests` row is `tests ok 10 passed`, before the `resources` rows; without test cases it
is `tests ok no test cases (tests/*.yaml)`.

### Author repositories: `plg.yaml`

Teams keep several plugins in one repository. A `plg.yaml` at its root lists them and what `plg validate` and
`plg pack` apply to them all:

```yaml
plugins:                 # the plugins' directories (at least one); a plugin's key is its directory's name
  - gold
  - rules/silver
  - coupon
shared:                  # directories copied into every plugin's files
  - lib
limits:                  # override the contract's //plg:limits for every plugin
  memory: 64MiB          # bytes, or KiB, MiB, GiB; from 64KiB to 4GiB
  timeout: 2s            # bounds Create and each test case's Create and call
  pool: 1..8             # N or MIN..MAX, from 1 to 4096; plg validate runs one instance, so only printed
requireOwnership: true   # a plugin without a complete owner fails instead of warning
replace:                 # contract modules taken from local checkouts, like go.mod's replace
  example.com/rewards: ../rewards
```

It is read in plugin.yaml's strict YAML subset (unknown or duplicate keys, anchors, aliases and tags are errors).
Every directory must exist inside the repository, none listed twice nor inside another, and plugins' keys must
differ; a replace's directory must hold a go.mod declaring that module. An invalid plg.yaml fails before any
plugin is validated: the report (table, JSON and JUnit) holds the repository alone, failed at its `plg.yaml` step.

```sh
go run github.com/heynemann/go-plg/cmd/plg validate .
```

```
gold
STEP       RESULT  DETAIL
plg.yaml   ok      /home/ana/plugins/plg.yaml; shared lib
manifest   ok      go plugin gold implementing example.com/rewards/cohort.Provider@v1.2.0
shared     ok      copied lib at vendor/example.com/plugins/lib
…

rules/silver
STEP       RESULT  DETAIL
…

PLUGIN        RESULT  DETAIL
gold          ok
rules/silver  ok      1 warning
coupon        FAIL    failed at load
```

Each listed plugin gets its section, in plg.yaml's order, then a summary with the step each failed at or its
warnings; the command exits 1 when any plugin fails (`plg validate: 1 of the 3 plugins …/plg.yaml lists is not
valid: coupon`). Validating one plugin directory of a repository (`plg validate rules/silver`) applies the
settings of the nearest plg.yaml above it (up to the directory holding `.git`) when that file lists it, and warns
otherwise. `-host` applies to every plugin.

- **Shared directories** keep the runtime's rule that a plugin is one directory: validate copies the plugin and
  each shared directory into a temporary directory and validates that copy (a plugin holding a file where a
  shared directory goes fails the `shared` step). Lua and TypeScript plugins find a shared directory at its path
  in the repository, from the plugin's root: `require("lib.tiers")`, `import { tier } from "./lib/tiers"` (in the
  repository, the generated `tsconfig.json`'s `rootDirs` and `.luarc.json`'s `workspace.library` make editors resolve
  the same names). Go
  plugins import shared packages by their real import path (the module path of the go.mod above the shared
  directory, then its path below it), so `go build ./...` and editors work in the repository; validate copies the
  shared directories holding Go files into the plugin's [`vendor/`](#go-plugins) at that path. A shared Go
  directory must be in a Go module.
- **limits** override, key by key, the limits the contract recommends (its `//plg:limits`), raising or lowering
  them; the `limits` row shows those in force and where each comes from.
  The memory limit is each instance's (`plg.WithMemoryLimit`), the timeout bounds `Create` and each test case's
  `Create` and call; the pool has no effect
  on `plg validate`, which runs one instance.
- **requireOwnership** checks ownership as `plg.WithRequiredOwnership` does: a missing owner is an error that fails
  the `manifest` step and stops the run there.
- **replace** takes a contract module from a local checkout instead of `go mod download` (no network or module
  cache needed): the contract step and the load warn that the pin is not checked against it, as a service built
  with a directory replace does. `plg pack` refuses to pack while one is set.

### Typings: `plg types`

Lua and TypeScript authors get editor support from the contract. `plg types <plugin dir>` resolves the contract
`plugin.yaml` pins (as `plg validate` does) and writes the plugin's typings, which are committed with it:

- **TypeScript:** `plg.d.ts`, declarations of the interface (what `Create` returns; each method may return its
  result or a Promise of it, several results as an array, plus the optional `Validate` and `Close` plg looks up by
  name), every type its methods take and return (struct fields under their `json` names; nil-able values, that is
  slices, maps, pointers and `[]byte`, may be `null`; `time.Time` as `plg.Time`, `{seconds, nanos}`, and a `Date`
  wherever the plugin hands a time back), the Host as the `host` global (its methods return Promises), the
  validation input, `plg.Issue`, and the guest's other globals (`console`, `fetch`, `Headers`, `Response`,
  `secrets`, text
  imports). Types live in a namespace per Go package (`cohort.Provider`, `cohort.Page`; a package named like a
  global gets a `_`), so plugins use them without importing anything:

  ```ts
  export function Create(): cohort.Provider {
    return {
      IsMember: async (user) => (await host.Tier(user)) === "gold",
      List: (cursor) => ({ users: [], next: "" }),
    };
  }
  ```

  And `tsconfig.json`, the compiler options `plg validate` type-checks with (ECMAScript 2023 and plg's globals,
  no DOM or Node.js, strict), which editors then share.
- **Lua:** `plg.d.lua`, [Lua Language Server](https://luals.github.io) annotations of the same (a `---@meta` file,
  never run): classes for the interface, the Host (`host`), the types and plg's `Issue`, `Time`, `http`, `json`
  and `secrets`. Annotate the plugin's table with `---@class Gold: cohort.Provider` to have its methods checked.

`plg types` never overwrites a file it did not write (its first lines carry
`Code generated by plg types. DO NOT EDIT.`) and leaves an up-to-date file untouched. `-host <Interface>` names
the Host the plugin uses, recorded in the files' header; without it, `plg types` keeps the one the existing
typings name, else takes the contract's only possible Host, as `plg validate` does. Run it again whenever
`implements` changes: `plg validate` fails until the typings match the pinned contract. Go plugins get none: they
import the contract package itself.

### Starting and iterating: `plg init` and `plg dev`

`plg init` starts a plugin from the contract it will implement:

```sh
go run github.com/heynemann/go-plg/cmd/plg init -implements example.com/shop/pricing.Rule@v1.0.0 -lang ts rules/gold
```

It resolves the contract as `plg validate` does (an author repository's `replace` included) and writes a new
plugin directory (default: the interface's name, lowered; an existing one must be empty): `plugin.yaml`
(`implements`, `language`, `entrypoint` and an `owner` block to fill in), the typings of a Lua or TypeScript
plugin, a skeleton implementing every method of the interface with its results' zero values (and `Validate`,
returning no issues, when the contract declares `//plg:validation`) whose `Create` takes the Host (Go) or reaches
it through `host` (Lua, TypeScript), and `tests/example.yaml`, a case per method calling it with zero values and
expecting what the skeleton returns. The Host is `-host`'s, else the contract's only possible Host. A Go plugin
becomes a module of its own requiring the pinned contract module, unless its author repository has a `go.mod`
above it, which gains that requirement instead (`go mod tidy` afterwards writes `go.sum`); a `go.mod` requiring
another version of the contract module stops `plg init` before it writes anything. A plugin inside an author
repository is added to its `plg.yaml`, after the plugins it lists, its comments kept. The new plugin passes
`plg validate` as written, with a warning until its owner block is filled in (an error under
`requireOwnership: true`); a Go plugin on a contract with `//plg:validation` also gets a `types` warning, since
validate type-checks only imports of the contract, vendored packages and the standard library, and its
`Validate` imports plg's `plugin` package: the warning stays whatever its `go.mod` requires. In an author
repository with `shared:` directories, the typings of a Lua or TypeScript plugin (from
`plg init` and `plg types`, checked by validate) include the editor configuration resolving them at their
repository paths: `tsconfig.json`'s `rootDirs` merges the plugin with the repository's root, and a `.luarc.json`
lists them in the Lua Language Server's `workspace.library`.

`plg dev [plugin dir]` (default: the current directory, or an author repository's only plugin) validates the plugin
as `plg validate` does, then again whenever its files, its tests, the repository's `plg.yaml` or its shared
directories change (it polls them every quarter of a second), printing each run's table. A directory with no
`plugin.yaml` is refused (exit 2: `<dir> is not a plugin directory: it has no plugin.yaml or plg.yaml and no author
repository lists it`) unless it is an author repository (its only plugin) or one lists it. Watching stops with
`plg.ErrPluginTooLarge`, naming the entry, once the watched files (the plugin, the shared directories and
`plg.yaml` together) hold more than `plg.MaxPluginEntries` (16384) entries, as a plugin's own files may. The
contract module is resolved once (later runs reuse it without the go command) and the guests are warm in the sense
of the compilation cache: the guests and the TypeScript compiler compiled on the first run are loaded from the
on-disk cache (`-cache-dir`) on every run after a save, which compiles nothing new, so it costs the load and the
test cases. Each run still starts a fresh container, as `plg validate` does. It reads commands from stdin:

```text
plg dev> call Quote order.yaml
Quote: result {total: 300, discount: 0, reasons: null}
plg dev> call Split
Split failed: call Split: input gives no "cents" (parameters: cents, parts)
```

`call <Method> [input file]` calls one method (or `Validate`) in a fresh instance under the plugin's limits, with
the parameters the YAML input file gives by name, as a test case's `input` (`$ref` relative to the file); the
Host, HTTP requests and secret reads fail, as in `plg validate`'s `load`. `run` validates again, and `quit` or
the end of the input stops it.

### Packing a plugin: `plg pack`

Once a plugin validates, `plg pack` turns it into one file a platform can keep anywhere (a bucket, a database
row) and load through any storage:

```sh
go run github.com/heynemann/go-plg/cmd/plg pack -o dist rules     # an author repository: dist/<key>.plg each
go run github.com/heynemann/go-plg/cmd/plg pack -o dist rules/gold
```

It validates exactly as `plg validate` does (same flags, but `-near-limit`; same table, `-format json` and
`-junit`) and writes nothing unless every plugin is valid: a failed step, an author repository whose `plg.yaml`
replaces the contract module, or a Go plugin whose `go.mod` replaces it by a directory, stops it with exit code 1
(validate only warns about those replaces; an artifact must be checked against the module its `plugin.yaml` pins).
It then writes `<key>.plg` per plugin into `-o` (default: the working directory, which may not be inside the
files it packs, or the next pack would pack the artifacts) and says so on stderr: `plg pack: wrote
dist/gold.plg (sha256:…)`.

An artifact is a gzip-compressed tar archive:

- `provenance.json`, first: `format` (1), `contract` (`path` and `version` of the contract module the plugin was
  validated against), `digest` (the content digest of the files below: `sha256:` and the SHA-256 over each file's
  path and SHA-256, the version Register gives a plugin whose `plugin.yaml` names none), `commit` (the git commit
  `HEAD` of the plugin's repository; empty outside git), `dirty` (whether the files packed — the plugin's, the
  repository's shared directories and `plg.yaml` — differ from that commit: changed, added, untracked or
  ignored), `plgVersion` (the plg that packed it, `(devel)` built from a checkout) and `report` (validate's JSON
  report of this plugin, as below).
- `plugin/…`: the plugin's files as a storage loads them, the repository's shared directories copied in where
  validate copies them (Lua and TypeScript at their repository paths, Go under `vendor/`): regular files only
  (links inside the plugin are left out, as every storage leaves them out).

The same input packs to the same bytes: entries in walk order, every one a regular file of mode 0644 dated the
Unix epoch without owner, a gzip header without name or time, and a report without what changes from one run to
the next: the resource report (each case's time and peak memory) is left out, and a custom Go guest's step says it
came "from" the guest cache whether or not it was built in this run. Nor does it name the machine: its paths are
relative to the author repository (to the plugin's directory without one), the contract module is its path and
version without the module cache's directory, the guest cache is not named, and a pool plg's defaults set reads
`pool=1..GOMAXPROCS` rather than this machine's CPU count. So the same input packs to the same bytes from any
checkout on any machine; the digest depends on the files alone. Pack copies the plugin's files once, before
validating them, and validates and packs that copy (git's state is read at the same moment), so a file changed
during the run is neither validated nor packed.

Services read artifacts with `plg.OpenArtifact` (see [Storages](#storages)); `plg.Info`, the admin handler and
`plg status <key>` show the provenance of a plugin loaded from one. Nothing about it is enforced at load.

## Storages

```go
type Storage interface {
	Name() string                                           // for logs and errors
	List(ctx context.Context) ([]plg.Descriptor, error)     // the plugins, each with a Key, Implements, Language, Entrypoint, Version, MemoryLimit, Owner, Secrets
	Open(ctx context.Context, d *plg.Descriptor) (fs.FS, error) // a plugin's files; d is one List returned, not to be modified
}
```

A storage may list plugins of several interfaces: each Register takes the ones declaring its own (see
[Declaring the interface](#declaring-the-interface-implements)). Register may take several storages
(`plg.WithStorage` repeated); a key listed twice fails with `plg.ErrDuplicateKey`.

Register reads an opened plugin's files once, into an in-memory copy that it hashes (for an empty `Version`),
bundles and loads, so files changing during a load are never half seen. Symbolic links are never followed, in
any storage: a link that stays inside the plugin's files is skipped as if it were not there (a plugin cannot
import or `require` it), and a link whose target is absolute or leads out of the plugin's files (or cannot be
read) fails the plugin's load with an error naming it.

The copy is bounded, in every storage: a regular file over `plg.MaxPluginFileSize` (16 MiB), or regular files
over `plg.MaxPluginSize` (64 MiB) together, fail the load with `plg.ErrPluginTooLarge`, naming the plugin and
the file, before they are read (every file counts, used by the plugin's language or not); a file growing while
it is read is read up to what is left of the limits only. The listing is bounded too, so empty files,
directories or long names cannot fill memory: more than `plg.MaxPluginEntries` (16,384) directory entries of any
kind, or entry paths over `plg.MaxPluginPathBytes` (2 MiB) together, fail the load with `plg.ErrPluginTooLarge`
as soon as a directory read brings the plugin over, before the rest is listed. Directories are read in batches
through `fs.ReadDirFile`, which every directory of an `os.DirFS`, `embed.FS`, `fstest.MapFS` and `NewFSStorage`
is; an `fs.FS` whose opened directories are not can only be listed a directory at a time, whole, through its
`fs.ReadDirFS`. Ending Register's context stops the copy at the next directory, file or read. `MemoryStorage.Put`
and the FS storage's `Open` copy by the same rules, and the S3 example stops paging through a listing that brings
a plugin over the entry or path limit.

Built in:

- `plg.NewFSStorage(dir)` and `plg.NewFSStorageFromFS(name, fsys)` (any `fs.FS`: an `embed.FS`, `fs.Sub` of one,
  …). Each top-level directory is a plugin, its name the key, with a `plugin.yaml`:

  ```yaml
  implements: example.com/contracts/rewards.Cohort@v1.4.0
  language: go
  entrypoint: plugin.go
  version: 1.4.0
  memoryLimit: 67108864
  ```

  An optional `owner` block says who owns the plugin (see [Ownership](#ownership)), and an optional `secrets` list
  names the secrets it reads (see [Secrets](#secrets)).

  `implements` (see [Declaring the interface](#declaring-the-interface-implements)), `language` (`go`, `lua` or
  `typescript`) and `entrypoint` are required; `version` is optional (without it,
  `List` leaves it empty and Register uses a SHA-256 of the files it loads); `memoryLimit` (bytes, from 65536 — one wasm page — to 4 GiB) may only lower
  the Register's limit. Unknown or misspelt keys are errors (`plg.ErrInvalidManifest`), so a typo never silently
  drops a limit. The file is one YAML document in a strict subset: anchors, aliases, merge keys (`<<`), tags
  other than YAML's core types, a second document and duplicate keys are errors too, and string values take the
  scalar verbatim (`version: 1.10` is `"1.10"`). A `plugin.json` is never read: a directory with one but no
  `plugin.yaml` fails the listing with a hint to convert it. A `plugin.yaml` must be a regular file of at most `plg.MaxManifestSize` (64 KiB) bytes, else
  it is `plg.ErrInvalidManifest` too (a FIFO or a link to a device is never read). Top-level files, links and dot-directories are ignored. `NewFSStorage` reads `dir` through an
  `os.Root`, so no path or link leads it out of `dir`, and it copies a plugin directory entry by entry, each opened
  relative to its parent and checked to be that very entry: a file or directory swapped for a link mid-copy is
  never followed, out of the plugin or inside it. Its opens never wait: a `plugin.yaml`, file or directory swapped
  for a FIFO (whose ordinary open waits for a writer) fails the listing or the copy at once. `NewFSStorageFromFS`,
  `MemoryStorage.Put` and your own storages are copied through the `fs.FS` interface, which opens a file by name:
  a tree that follows links (an `os.DirFS`, anywhere on the host) can be led out of the plugin by a link swapped in
  mid-copy, and its open of an entry swapped for a FIFO can wait for a writer, so serve directories others can
  write to with `NewFSStorage`.
- `plg.NewMemoryStorage(name)` with `Put(desc, files)` and `Delete(key)`: plugins a program builds itself, and
  tests.

Your own: implement the three methods. `plg.ParseManifest` parses a `plugin.yaml` exactly like the FS storage, for
storages that keep the same layout elsewhere; read at most `plg.MaxManifestSize`+1 bytes of it, as it rejects more. A `Version` that changes exactly when the plugin changes lets
reloads skip unchanged plugins (an empty one makes plg hash the files on every reload). Two complete storages,
kept out of plg's own dependencies, live in [`examples/`](#examples): **S3** (aws-sdk-go-v2, the plugin.yaml
layout under a key prefix, versions from object ETags) and **PostgreSQL** (pgx, the plugin.yaml stored as one of
the plugin's file rows, only enabled plugins listed, versions maintained by triggers, migrations included).

A storage may also keep one blob per plugin: the `.plg` artifact `plg pack` writes (see
[Packing a plugin](#packing-a-plugin-plg-pack)). `plg.OpenArtifact(r io.ReaderAt, size int64)` reads one into
memory and returns a `*plg.Artifact`: an `fs.FS` (also `fs.ReadFileFS`, `fs.ReadDirFS`, `fs.StatFS`) of the
plugin's files and its `Provenance()`. List each artifact by its `plugin.yaml` and return the `*plg.Artifact` itself
from `Open`: Register loads its files as they are, without another copy, and `plg.Info` and the admin handler show
its provenance (a wrapper or a copy loads the same files without it):

```go
func (s *blobStorage) List(ctx context.Context) ([]plg.Descriptor, error) {
	var descs []plg.Descriptor
	for key, blob := range s.blobs { // blob is a .plg file's bytes, from wherever the platform keeps them
		art, err := plg.OpenArtifact(bytes.NewReader(blob), int64(len(blob)))
		if err != nil {
			return nil, err
		}
		manifest, err := art.ReadFile("plugin.yaml")
		if err != nil {
			return nil, err
		}
		desc, err := plg.ParseManifest(manifest)
		if err != nil {
			return nil, err
		}
		desc.Key = key
		if desc.Version == "" { // the digest Register would give it, so a poll skips an unchanged artifact
			desc.Version = art.Provenance().Digest
		}
		descs = append(descs, desc)
	}
	return descs, nil
}

func (s *blobStorage) Open(ctx context.Context, d *plg.Descriptor) (fs.FS, error) {
	blob := s.blobs[d.Key]
	return plg.OpenArtifact(bytes.NewReader(blob), int64(len(blob)))
}
```

`OpenArtifact` holds an artifact to the limits above (a file stating more than `MaxPluginFileSize`, files over
`MaxPluginSize` together, more than `MaxPluginEntries` entries or paths over `MaxPluginPathBytes`, counting each
directory a file's path implies once with its path, as a storage's listing does: `ErrPluginTooLarge`, before they are
read; decompressing stops past what those limits allow, so a compression bomb is not inflated),
and fails with `plg.ErrInvalidArtifact` on anything `plg pack` does not write: an archive cut short or corrupted
(gzip's checksum, or files whose content digest is not the provenance's), an entry outside `plugin/` (`..`,
absolute or unclean paths), anything but a regular file (links, directories, devices), a file twice, a missing
provenance record or `plugin.yaml`, or a format it does not read. It enforces nothing about the provenance itself:
a plugin loads whatever its commit, dirty flag or report say; the platform decides what to accept.


## Ownership

A plugin's `plugin.yaml` may say who owns it, so whoever meets a problem with the plugin knows who to talk to:

```yaml
owner:
  maintainers:                      # the first is the primary one
    - {name: Ana Lima, email: ana@example.com, handle: "@ana"}
  team: {name: payments, department: finance}
  org: commerce
  company: Acme Inc.
  contact: {oncall: payments-oncall, channel: "#payments", email: payments@example.com}
  links: {repository: https://git.example.com/rules, docs: ..., runbook: ..., issues: ...}
  labels: {tier: "1"}
```

Every field is optional. It is parsed into a `*plg.Owner` on `Descriptor.Owner`, nil without an `owner` block
(custom storages set it themselves,
or get it from `plg.ParseManifest`), with limits that Register checks for every storage: strings of at most
`plg.MaxOwnerStringBytes` (256) bytes, at most `plg.MaxOwnerMaintainers` (16) maintainers, each with a name, email
or handle, and at most `plg.MaxOwnerLabels` (32) labels with non-empty keys. The owner then shows up:

- on the plugin's `*plg.PanicError` and `*plg.PluginError` values, as their `Owner` field (messages unchanged);
- on every log record plg writes about the plugin (loads, failures, its guest output, its HTTP requests, its
  pool), as an `owner` group of the fields that are set: `team`, `department`, `org`, `company`, the primary
  `maintainer`, `maintainer_email` and `maintainer_handle`, `oncall`, `channel`, `email`, `repository`, `docs`,
  `runbook`, `issues` and a `labels` group;
- on every plugin metric, as the `team`, `org` and `company` attributes (empty when not set). Names, addresses and
  labels never become metric attributes, so they cannot blow up cardinality or leak into dashboards;
- on every span of the plugin's calls, as the same `team`, `org` and `company` attributes (see
  [Logs, metrics and traces](#logs-metrics-and-traces)).

An owner change is a descriptor change: the next reload loads the plugin again even when its version is the same.

### Required ownership and load reports

Every load (at Register and on each reload) checks that the plugin names its owning team (`owner.team.name`) and a
way to reach it (at least one of `owner.contact.oncall`, `owner.contact.channel`, `owner.contact.email`). A plugin
lacking either still loads, with a `plg.ownership.missing` warning (`plg.CodeOwnershipMissing`) naming the missing
fields. To make ownership a condition of going live:

```go
plg.Register[pricing.Rule](ctx, c, "pricing", plg.WithStorage(store), plg.WithRequiredOwnership())
```

With it the issue is an error that blocks the load like any load failure: Register fails with `plg.ErrLoad`
wrapping `plg.ErrBlockingIssue`, the message naming the plugin and the missing fields, and a reload keeps serving
the plugin's last good version (a new plugin stays absent).

Each load attempt produces a `plg.Report{Key, Version, Owner, Issues, Err, At, Duration}`, whose
`plg.Issue{Severity, Code, Message, Field}` values have a severity of `plg.SeverityError` (blocks the load; so does
any severity plg does not know), `plg.SeverityWarning` or `plg.SeverityInfo`. plg keeps the report with the version
it loaded, or, when a reload rejects an update or a new plugin, as that key's rejection until a later reload loads
the plugin or finds it unchanged. Every issue is logged as a `plg: plugin issue` record at its severity's level
(Error, Warn, Info) with the plugin's `key`, `version` and `owner` group and the issue's `severity`, `code`,
`field` and `issue` (its message). `plg.Info` and `plg.Plugins` return the reports
([inspecting plugins](#inspecting-plugins-plginfo-and-plgplugins)).

## Validation

A registration can make its plugins validate themselves before they go live. Declare the validation input as a
data type in the contract package (the rules of [the interface](#the-interface)), and say how each plugin gets
its input:

```go
// in package pricing
type Limits struct {
	MaxDiscount float64
}

plg.Register[pricing.Rule](ctx, c, "pricing", plg.WithStorage(store),
	plg.WithValidation(func(ctx context.Context, info plg.PluginInfo) (pricing.Limits, error) {
		return limitsFor(info.Owner.Team.Name), nil // info: Key, Version, Language, Storage, Owner
	}),
	plg.WithValidationTimeout(5*time.Second), // default 10 s
)
```

Register fails with `plg.ErrInvalidOption` when the input type is not a named data type declared in the contract
package. Every plugin's instance (what `Create` returns) must then have a `Validate` method beside the
interface's, looked up by name like `Close`. In Go it returns `plugin.Issue` values from
`github.com/heynemann/go-plg/plugin`, a package compiled into the Go guest (custom guests from
`plg build-guest` included):

```go
import "github.com/heynemann/go-plg/plugin"

func (r *rule) Validate(ctx context.Context, in pricing.Limits) ([]plugin.Issue, error) {
	if r.discount > in.MaxDiscount {
		return []plugin.Issue{{Severity: plugin.Error, Code: "rule.discount", Message: "over the limit", Field: "discount"}}, nil
	}

	return nil, nil
}
```

Lua and TypeScript plugins receive the input as any argument (a table, an object, keyed like the contract's
fields) and return a list of issues with the fields `severity` (`"error"`, `"warning"` or `"info"`), `code`,
`message` and `field`; a missing field is empty and `nil` or `undefined` means no issue. TypeScript's `Validate`
may also return a Promise of the array:

```lua
function Rule:Validate(limits)
  if self.discount > limits.MaxDiscount then
    return { { severity = "error", code = "rule.discount", message = "over the limit", field = "discount" } }
  end
  return nil -- or nil, "message" to fail; error(...) fails too
end
```

```ts
async Validate(limits: { MaxDiscount: number }) {
  if (this.discount > limits.MaxDiscount) {
    return [{ severity: "error", code: "rule.discount", message: "over the limit", field: "discount" }];
  }
  return []; // throwing (or rejecting) fails
}
```

On every load (at Register and on each reload), before any of the plugin's pool instances starts, plg starts a
separate, throwaway instance, runs its `Create`, calls `Validate` with the function's input under the validation
timeout (never past the load's `ctx`) and closes it, so the serving instances' state never sees validation. It
has the Register's Host and HTTP policy; Host methods called from that `Create` or `Validate` can tell with
`plg.IsValidation(ctx)`, and its HTTP requests are logged with `validation=true`. Its issues go into the load
report and the log like the ownership check's. The load is blocked (Register fails with `plg.ErrLoad`; a reload
keeps the last good version) by:

- an issue of severity error, or of a severity plg does not know (wrapping `plg.ErrBlockingIssue`); warnings
  and info are only recorded;
- the function returning an error or panicking (a `plg.validation.input` error issue, `plg.CodeValidationInput`);
- `Validate` failing (a Go error, Lua's `nil, msg` or `error(...)`, a TypeScript throw or rejected Promise),
  panicking or trapping (for scripts: running out of memory), running past the timeout (a TypeScript Promise
  still unsettled then included; one that nothing is left to settle fails at once), issues that do not fit
  `plg.Issue`, and an instance without a `Validate` (in Go: of exactly that signature, the message showing the
  one the plugin declares), all wrapping `plg.ErrValidation`.

The validation instance is charged to the memory budget within the plugin version's own reservation
(`max × limit`): it is closed before the pool's instances start, so a budget sized exactly to the plugins' worst
case still validates them.

### Validating on demand: `plg.Validate` and `plg.ValidateAll`

What a plugin validates against can change after it loaded (limits, a remote dependency). Admin tools and health
checks can validate the versions the container serves at any time:

```go
report, err := plg.Validate(ctx, c, "pricing/discounts")
switch {
case err != nil: // could not run: ErrNotFound, ErrNoValidation (no WithValidation), ErrClosed, ErrMemoryBudget, ctx
case report.Err != nil: // it failed: ErrBlockingIssue (error issues) or ErrValidation (Validate failed)
default: // it passed; report.Issues holds its warnings and info
}

for key, report := range plg.ValidateAll(ctx, c) { // every plugin of every registration with WithValidation
	_ = key // one at a time, in key order; report.Err also holds why one could not run
}
```

Each validation asks the Register's function for the plugin's input and runs `Validate` in a fresh, throwaway
instance, as at load, then closes it. Its memory limit is charged to the budget as extra room, beyond every
plugin's reservation (the version's own pool keeps its room), never waiting: a budget the reservations fill fails
it at once with `plg.ErrMemoryBudget`, and nothing changes. Its issues are logged like a load's, and its report is
kept with the version as its latest validation. What a failure does is set per Register:

```go
plg.WithOnDemandValidationPolicy(plg.Degrade)   // the default: keep serving, marked degraded until a later validation passes
plg.WithOnDemandValidationPolicy(plg.Unpublish) // remove it: Get fails with ErrNotFound, calls in flight finish there
```

An unpublished version drains like one a reload removes, and the failed report becomes the key's rejection
(`plg.Info` shows it `retired`); the next reload loads and validates the plugin again and, if that passes,
publishes it again. Every validation (at load, on demand and by `plg.Check`) is counted in the
`plg.plugin.validations` metric by trigger and outcome (`passed`, `failed`, `unavailable`), and its issues in
`plg.plugin.validation.issues` by severity.

### Checking a candidate: `plg.Check`

Before enabling a new plugin or version (say, in an admin endpoint that receives an upload), dry-run it against
an existing registration:

```go
report, err := plg.Check(ctx, c, "pricing", &plg.Descriptor{
	Key: "discounts", Implements: "example.com/shop/pricing.Rule@v1.0.0", Language: plg.LanguageGo,
	Entrypoint: ".", Owner: &owner,
}, uploadedFiles) // any fs.FS
switch {
case err != nil: // could not check: unknown name (ErrNotFound), ErrClosed, ErrMemoryBudget, ctx ended
case report.Err != nil: // the candidate would not load; report.Err and report.Issues say why
default: // it would load; report.Issues holds its warnings and info
}
```

`Check` loads the candidate as a reload of that registration would (its contract, Host, HTTP policy, secret
resolver, enabled languages, memory limits, ownership rules and validation); its files go through the same snapshot limits as a
storage's, and a descriptor without a `Version` gets their content hash. It runs `Create` and, with
`WithValidation`, `Validate` in one throwaway instance whose Host calls and HTTP requests see
`plg.IsValidation(ctx)`, then closes it. Nothing is published: no catalogue entry, no report kept for the
registration, no events (the candidate's issues are logged like a load's). The instance's memory limit is
charged to the budget as extra room, beyond every plugin's reservation, never waiting for it: a budget the
reservations fill fails `Check` at once with `plg.ErrMemoryBudget`.

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
  at Register's `ctx`, and at `Container.Close` when the pool was starting it. When `Container.Close`'s `ctx` ends
  before the calls in flight do, it stops them, wherever they wait (running, sleeping, in an HTTP request, whose
  context ends, or in a Host method, whose `ctx` ends): each fails with `plg.ErrClosed`, as does a `Create` it
  stops, and none counts as a plugin failure. Always call plugins with a deadline.
- **Pool:** `WithPool(min, max)` (default `1, GOMAXPROCS`; `1 ≤ min ≤ max ≤ 4096`). Register starts `min`
  instances of each plugin (so a broken `Create` fails Register), a background goroutine keeps `min` warm and
  replaces discarded ones (retrying a failing `Create` with backoff from 100 ms to 30 s), and a call takes an idle
  instance, starts one while under `max`, or waits on its `ctx`. A call that starts an instance does so under its
  own `ctx`, and starting one takes far longer than a call (see [Performance](#performance)), more so on a busy
  machine; a service whose calls have short deadlines therefore sets `min = max`, as `examples/service` does:
  Register starts every instance, and their memory is taken from the start.
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
Go request's `Header`, `fetch`'s `headers`, `http.request`'s `headers` table) are left as they were, except
`traceparent` and `tracestate`: whenever the call's context holds a span, plg sets them to the request's span, or
with tracing off to the caller's (see [Logs, metrics and traces](#logs-metrics-and-traces)), replacing the
plugin's (its `tracestate` is dropped even when plg's is empty); only a call whose context holds no span keeps the
plugin's own. Each request is a client span `plg.http <method>`, a child of the call's span.

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

## Secrets

Plugins calling third-party APIs get their credentials from the service instead of their code. A plugin declares
the names it reads in its `plugin.yaml`:

```yaml
secrets: [coupon_token, stripe_key]   # 1 to 64 letters, digits, _ - . each, starting with a letter; at most 64
```

and the registration resolves them, from a vault, the environment or anything else:

```go
plg.Register[pricing.Rule](ctx, c, "pricing", plg.WithStorage(s),
	plg.WithSecrets(func(ctx context.Context, info plg.PluginInfo, name string) (string, error) {
		return vault.Read(ctx, "plg/"+info.Owner.Team.Name+"/"+name)
	}))
```

```go
token, err := plugin.Secret(ctx, "coupon_token") // Go: github.com/heynemann/go-plg/plugin
```
```lua
local token = secrets.get("coupon_token")        -- Lua: raises an error when the read fails
```
```ts
const token = secrets.get("coupon_token");       // TypeScript: throws an Error when the read fails
```

- **Only declared names.** Reading a name `plugin.yaml` does not declare fails without calling the resolver;
  changing the list reloads the plugin even at the same version.
- **Resolved on access, cached briefly.** The resolver runs on the plugin's read, with the plugin call's `ctx`
  (Create's, or validation's) and the version's `PluginInfo`. A value serves that version's reads for 30 seconds,
  then the next read resolves it again: rotated secrets reach plugins without a reload.
- **Clear failures, nothing leaked.** Without `WithSecrets`, when the resolver fails or panics (a `PanicError` in
  `PhaseCallback`, method `WithSecrets`), or returns more than `plg.MaxSecretSize` (64 KiB), the read fails. The
  plugin is told the secret could not be resolved; why goes to the service's log only.
- **Scrubbed.** plg replaces every value it resolved for a version (its newest 8 per secret) with
  `[secret <name>]` in what it reports of the plugin: guest output, returned errors, panic values and stderr,
  Validate issues, and HTTP log records and spans. Long values are caught too, up to `plg.MaxSecretSize`: an
  output line is logged in pieces of 4 KiB, or of twice the longest value when that is longer, cut where no value
  is split; the stderr a panic reports (its last 32 KiB) comes from a buffer of 32 KiB or twice the longest value,
  and never starts with the end of a value. It matches exact values: an encoded, sliced or otherwise transformed
  secret is not caught. What a successful call returns is the service's and is not scrubbed.

`plg validate` reads fail outside test cases; test cases fake them with `secrets:` (see
[Test cases](#test-cases-testsyaml)).

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
	Owner    plg.Owner // the failing plugin's owner; zero when no plugin is involved or it has none
}
```

- **Inside the guest:** a panic, out of memory, a stack overflow, a runtime abort (`select {}`), a call stopped at
  its deadline, a broken result: the call fails with a `PanicError`, that instance is discarded, the pool replaces
  it. `errors.Is(err, plg.ErrCall)` holds for every failed call, and `errors.Is(err, context.DeadlineExceeded)`
  for a deadline stop.
- **On the host:** every boundary where plg runs code it does not control is behind `recover`: storages, the
  codec, Host methods, the HTTP transport, pollers and refillers, your callbacks, your logger's handler (on every
  record plg logs: loads, skipped plugins, compiles, reloads, closes, HTTP requests and the plugins' own output)
  and your meter and tracer providers (a panic there is a `PanicError` in phase `callback`, the record,
  measurement or span is dropped, and `New`, `Register`, the reload, the call, the pool and its instances carry
  on, their state intact).
  esbuild does not run on the host: it bundles TypeScript plugins in a WebAssembly sandbox with its own time and
  memory limits.
- **Reporting:** each `PanicError` is logged at Error level, counted (`plg.plugin.panics`), passed to
  `plg.WithOnPanic(fn)` and kept in `c.RecentPanics()` (the last 64, `plg.WithPanicHistory(n)`).

```go
_, err := rule.Discount(ctx, order)

var pe *plg.PanicError
if errors.As(err, &pe) {
	slog.Error("rule crashed", "key", pe.Key, "phase", pe.Phase, "guest", pe.Guest)
}
```

## Logs, metrics and traces

- **Logs:** `plg.New(plg.WithLogger(logger))` (a `*slog.Logger`; discarded by default) gets loads, reloads,
  skips, failures, panics, pool backoffs, HTTP requests and the plugins' own output (stdout at Info, stderr at
  Warn, with the plugin key, and `trace_id`/`span_id` whenever the call's context holds a span: the call's span,
  or with tracing off the caller's).
- **Metrics:** OpenTelemetry, through `plg.WithMeterProvider(mp)` (default: the global provider): calls, latency
  and errors by key, method and outcome; panics by phase; pool instances, instances in use (serving at least one
  call), calls in flight (more than instances in use only with `WithConcurrentCalls`), waits and refills; instance
  memory; loads, skips and reload failures; validations by trigger and outcome, and their issues by severity;
  HTTP requests, latency and sizes; lifecycle events dropped (`plg.events.dropped`). Names and attributes are
  listed on `plg.WithMeterProvider`. Recording on the call path does not allocate.
- **Traces:** OpenTelemetry, through `plg.WithTracerProvider(tp)` (default: the global provider, picked up once
  an SDK is installed). Each plugin call is a span `plg.call <key>.<method>`, a child of the span in the call's
  context, with the key, version, language, method, outcome and the owner's team, org and company; failed calls
  have the error status. Host methods (`plg.host <Host>.<method>`) and HTTP requests (`plg.http <method>`, client
  spans) are its children, and every request carries the `traceparent` of its span, so the trace continues in
  the services the plugins call:

  ```go
  ctr := plg.New(plg.WithTracerProvider(tp))
  ctx, span := tracer.Start(r.Context(), "price order") // or the span your HTTP middleware started
  defer span.End()
  total, err := rule.Price(ctx, order) // plg.call pricing/loyalty.Price, its Host calls and requests below it
  ```

  With a no-op provider (or none installed) tracing costs a call nothing. Span names and attributes are listed
  on `plg.WithTracerProvider`.

### Lifecycle events: `WithOnEvent`

To route plugin problems to the people who own them, give the container one function that receives every
lifecycle event:

```go
c := plg.New(plg.WithOnEvent(func(ctx context.Context, e plg.Event) {
	switch e.Kind {
	case plg.EventRejected: // e.Report.Err says why; e.Owner says who to tell
		page(e.Owner.Contact.Oncall, e.Key, e.Version, e.Report.Err)
	case plg.EventPanicked: // e.Panic is the PanicError
		notify(e.Owner.Contact.Channel, e.Panic)
	}
}))
```

| Kind | When | Carries |
|---|---|---|
| `EventLoaded` | Register or a reload published a plugin version | its load `Report` |
| `EventRejected` | a load failed (Register, which then fails, or a reload, which keeps the last good version), an issue blocked it (ownership, validation), a reload's storages list a key it cannot load (invalid descriptor, a key listed twice), or an on-demand validation unpublished the version | the failed load's or validation's `Report` |
| `EventRetired` | a reload replaced or removed a version, `Unpublish` removed it, or `Close` closed the container | |
| `EventPanicked` | a `PanicError` was reported (what `WithOnPanic` gets) | `Panic` |
| `EventValidated` | an on-demand validation ran (`plg.Validate`, `plg.ValidateAll`) | its `Report` |
| `EventSkipped` | a listed plugin implements another interface, or its language is not enabled, at Register and each reload | its `Report`, whose `Err` is `plg.ErrOtherInterface` or `plg.ErrUnsupportedLanguage` |

Every event carries the plugin's `Key`, `Version`, `Language`, `Storage` and `Owner` (empty where not known: a
plugin rejected before its files were read has only its descriptor's version, if any, and an owner a listing
problem found invalid is left out). Delivery never slows anything down: one goroutine per container calls the
function, in order, from a queue of 1024 events; an event that finds the queue full is dropped, counted in
`plg.events.dropped` and logged at Warn. A panic in the function is contained like every callback's (a
`PanicError` in phase `callback`, kept in `RecentPanics`) and delivery goes on. `Close` delivers what is queued
(the versions it retires included) for as long as its ctx allows, ends the function's ctx with its own, and
stops. `plg.Check` emits nothing.

Whether it has a `WithOnEvent` function or not (and whether its queue dropped an event or not), the container
keeps each plugin's newest 100 events, from the listing that first names the plugin until its storages stop
listing it:

```go
events, err := plg.Events(c, "pricing/discounts") // oldest first; ErrNotFound and ErrClosed as plg.Info
```

The admin handler serves them too (`GET /plugins/{key}/events`), and so does `plg events` from a terminal.

### Inspecting plugins: `plg.Info` and `plg.Plugins`

Admin tools and health pages can see every plugin the registrations' storages list, whether it loaded or not:

```go
for _, p := range plg.Plugins(c) { // every registration's plugins, in key order
	log.Printf("%s %s v%s team=%s panics=%d", p.Key, p.Status, p.Version, p.Owner.Team.Name, p.Panics)
}

p, err := plg.Info(c, "pricing/discounts") // ErrNotFound for a key the storages do not list, ErrClosed
```

| Status | Meaning |
|---|---|
| `serving` | the version the storages list serves; its latest on-demand validation, if any, passed |
| `degraded` | the version served failed its latest on-demand validation (`plg.Degrade`) and serves on |
| `serving_last_good` | the storages list a version (or descriptor) a reload could not load; the last good version serves on |
| `rejected` | no version serves: what the storages list failed to load, or listing it did (an invalid descriptor, a key listed twice) |
| `retired` | an on-demand validation unpublished it (`plg.Unpublish`), until a reload loads it again |
| `skipped` | it implements another interface than its registration's, or its language is not enabled for it (`Rejection.Err` says which) |

A `plg.PluginState` holds the plugin's `Key`, `Registration`, `Storage`, `Language` and `Owner`; the `Version`
served, when its load ended (`LoadedAt`) and that load's `Report` (`Load`); the `Report` of its latest on-demand
validation (`Validation`); the `Report` of what keeps the listing from being served (`Rejection`: the failed load
or listing, or the validation that unpublished it) and its version (`RejectedVersion`); the `PanicError`s reported
under the key since the storages listed it (`Panics`, a `plg.Check`'s excepted) with the newest (`LastPanic`); and
its pool (`plg.PoolStats`: bounds, instances, those in use, calls in flight, idle call slots, waiting calls,
refills, grows, memory limit and memory). A version loaded from an artifact also carries its `Provenance` (see
[Packing a plugin](#packing-a-plugin-plg-pack)); nil otherwise. A degraded version beside a rejected update shows
`degraded`, with both reports. A plugin is known once its first load settled, until the storages stop listing it; a
failed Register keeps nothing. Both read what the container keeps, never waiting for a load or a call and running
no plugin code;
`Get` and `All` are unchanged (still no allocation). The reports and `PanicError`s are the container's own: treat
them as read-only.

### The admin handler: `plg.AdminHandler`

Dashboards can read the same over HTTP. `plg.AdminHandler(c)` is an `http.Handler` serving read-only JSON:

```go
// requireAdmin is your own authentication and authorization middleware.
mux.Handle("/admin/plg/", requireAdmin(http.StripPrefix("/admin/plg", plg.AdminHandler(c))))
```

> **Mount it behind your service's own auth, never on a public listener.** The answers hold every plugin's owner
> (maintainers' names and emails, on-call rotations and channels included), its errors and its panics' guest
> output and host stacks. plg does no authentication of its own.

| Request | Answer |
|---|---|
| `GET /plugins` | `200` `{"plugins": [<plugin>, …]}`: what `plg.Plugins` says, in key order |
| `GET /plugins/{key}` | `200` `<plugin>`: what `plg.Info` says of the key, `<name>/<descriptor key>` (its slash may stay as is; escape anything else a URL path cannot hold); `404` for a key it does not know |
| `GET /plugins/{key}/events` | `200` `{"events": [<event>, …]}`: what `plg.Events` says of the key, oldest first; `404` for a key it does not know |
| any other method on those paths (`HEAD` included) | `405`, with `Allow: GET` |
| any other path | `404` |
| any path, once the container is closed | `503` |

Only a literal `/events` ends the events path: a key whose own last part is `events` (a custom storage whose keys
hold slashes) reads its `<plugin>` with that slash escaped, `/plugins/ping/x%2Fevents`.

Errors answer `{"error": "<message>"}`. Every answer is `application/json` with `Cache-Control: no-store`. The
handler reads what the container keeps, like `plg.Info`, never waiting for a load or a call; it runs no plugin
code, and it changes nothing: validating, reloading and `plg.Check` stay Go calls behind your own endpoints.

A `<plugin>` (here, one serving its last good version beside an update its validation rejected):

```json
{
  "key": "pricing/discounts",
  "registration": "pricing",
  "storage": "rules",
  "language": "lua",
  "status": "serving_last_good",
  "version": "1",
  "rejectedVersion": "2",
  "loadedAt": "2026-11-06T09:14:03.48823171Z",
  "owner": {
    "team": {"name": "payments", "department": "finance"},
    "org": "commerce",
    "company": "Acme Inc.",
    "contact": {"oncall": "payments-oncall", "channel": "#payments", "email": "payments@example.com"},
    "links": {"repository": "https://git.example.com/payments/rules", "docs": "", "runbook": "", "issues": ""},
    "maintainers": [{"name": "Ana Lima", "email": "ana@example.com", "handle": "@ana"}],
    "labels": {"tier": "1"}
  },
  "pool": {
    "min": 1, "max": 4, "instances": 2, "inUse": 1, "callsInFlight": 1, "idleSlots": 1, "waiting": 0,
    "refills": 0, "grows": 1, "memoryLimitBytes": 67108864, "memoryBytes": 2228224
  },
  "panics": 1,
  "lastPanic": {
    "time": "2026-11-06T09:20:11.002301Z",
    "phase": "call",
    "method": "Discount",
    "message": "plg: pricing/discounts: call Discount failed: not enough memory",
    "value": "not enough memory",
    "guest": "",
    "stack": ""
  },
  "provenance": null,
  "load": {
    "key": "pricing/discounts", "version": "1", "at": "2026-11-06T09:14:03.30154402Z", "durationNs": 186778190,
    "error": null, "owner": {"…": "as above"}, "issues": []
  },
  "validation": null,
  "rejection": {
    "key": "pricing/discounts", "version": "2", "at": "2026-11-06T09:31:40.1207749Z", "durationNs": 201338471,
    "error": "plg: plugin failed to load: storage rules: plg: load blocked by an error issue: rule.max: the discount exceeds 50%",
    "owner": {"…": "as above"},
    "issues": [{"severity": "error", "code": "rule.max", "message": "the discount exceeds 50%", "field": "max"}]
  }
}
```

- **Every key is always present.** `loadedAt` and `pool` are `null` when no version serves; `load`, `validation`,
  `rejection` and `lastPanic` are `null` when the state has none (the fields of `plg.PluginState` of the same
  names); a report's `error` is `null` when it has none. Lists and `labels` are empty, never `null`.
- **`provenance`** is `null` unless the version served was loaded from an artifact (see
  [Packing a plugin](#packing-a-plugin-plg-pack)); then it is the artifact's record: `contract` (`path`,
  `version`), `digest`, `commit`, `dirty`, `plgVersion` and `report`, plg validate's JSON report of the plugin as
  plg pack ran it:

  ```json
  "provenance": {
    "contract": {"path": "example.com/shop", "version": "v1.0.0"},
    "digest": "sha256:5f0c…", "commit": "8e2a91c…", "dirty": false, "plgVersion": "v0.9.0",
    "report": {"version": 1, "repository": "plg.yaml", "valid": true, "plugins": [{"plugin": "discounts", "…": "…"}]}
  }
  ```
- **Errors are their messages** (`err.Error()`), never Go values: a report's `error`, a panic's `message` (the
  `PanicError`'s) and `value` (the recovered value, the guest's panic message or the trap, printed).
- **Times** are RFC 3339 strings with nanoseconds, in UTC; **durations** (`durationNs`) are integer nanoseconds;
  **memory** is in bytes. `status`, `language`, `severity` and `phase` are the values of `plg.Status`,
  `plg.Language`, `plg.Severity` and `plg.Phase`.
- Key order and whitespace are not part of the format.

An `<event>` is a `plg.Event`: `at`, `kind` (a `plg.EventKind`), `key`, `version`, `language`, `storage`, `owner`
(as above), `report` (a report, or `null`) and `panic` (as `lastPanic`, or `null`), under the same rules:

```json
{"events": [
  {"at": "2026-11-06T09:14:03.48823171Z", "kind": "loaded", "key": "pricing/discounts", "version": "1",
   "language": "lua", "storage": "rules", "owner": {"…": "as above"}, "report": {"…": "the load's"}, "panic": null},
  {"at": "2026-11-06T09:31:40.1207749Z", "kind": "rejected", "key": "pricing/discounts", "version": "2",
   "language": "lua", "storage": "rules", "owner": {"…": "as above"}, "report": {"…": "the failed load's"},
   "panic": null}
]}
```

### From a terminal: `plg status` and `plg events`

Plugin authors check their plugins in an environment through its admin handler. Name the environments in
`~/.config/plg/profiles.yaml` (`$XDG_CONFIG_HOME/plg/profiles.yaml` when that is an absolute path):

```yaml
default: staging                                 # used without -profile (else the only profile)
profiles:
  - name: staging
    url: https://staging.example.com/admin/plg   # where the service mounts plg.AdminHandler
    token: {env: PLG_STAGING_TOKEN}              # sent as "Authorization: Bearer <token>"
  - name: prod
    url: https://prod.example.com/admin/plg
    token: {command: [vault, read, -field=token, secret/plg/prod]}  # what it prints, trimmed
```

A token comes from an environment variable or from what a command prints (run without a shell, in the working
directory, and given 30 seconds, as each admin request is); a profile without one sends none. `plg status` and
`plg events` follow no redirect (a 3xx answer is reported as an error), so the token never goes anywhere but the
profile's URL. Errors show a profile's URL without its password. The file is read as strictly as `plugin.yaml`
(unknown and duplicate keys, anchors and aliases are errors). Then:

```sh
go run github.com/heynemann/go-plg/cmd/plg status                  # every plugin, one row each
go run github.com/heynemann/go-plg/cmd/plg status -team payments   # the team's plugins only
go run github.com/heynemann/go-plg/cmd/plg status pricing/discounts -profile prod   # one plugin in detail
go run github.com/heynemann/go-plg/cmd/plg events pricing/discounts                 # its newest events
```

```text
KEY                STATUS             VERSION  LANGUAGE  TEAM      PANICS  PROBLEM
pricing/bulk       serving            3        go        pricing   0       -
pricing/discounts  serving_last_good  1        lua       payments  1       plg: plugin failed to load: …
```

```text
AT                    KIND      VERSION  DETAIL
2026-11-06T09:14:03Z  loaded    1        -
2026-11-06T09:20:11Z  panicked  1        plg: pricing/discounts: call Discount failed: not enough memory
2026-11-06T09:31:40Z  rejected  2        plg: plugin failed to load: …
```

`-format json` prints the handler's answer instead (`status -team` keeps the team's plugins in it). Flags may come
before or after the key. Errors (an unknown key, a refused token, an unreachable service) go to stderr with exit
code 1; misuse exits 2. Control characters in what plugins wrote (their messages, panics) are replaced in tables,
so a plugin cannot drive your terminal.

## Putting it together: fit to serve, and who to talk to

The [pricing service example](examples/service) wires every piece above for its three rules (Go, Lua and
TypeScript), so plugin authors, users and administrators each have one place to look.

**Plugin authors** declare who owns the rule in its `plugin.yaml` and make it check itself against the service's
limits:

```yaml
# examples/plugins/loyalty/plugin.yaml
implements: github.com/heynemann/go-plg/examples/pricing.Rule@v1.0.0
language: lua
entrypoint: rule.lua
owner:
  maintainers:
    - {name: Bo Chen, email: bo@example.com, handle: "@bo"}
  team: {name: loyalty, department: marketing}
  contact: {oncall: loyalty-oncall, channel: "#loyalty", email: loyalty@example.com}
```

```lua
-- examples/plugins/loyalty/rule.lua: limits is a pricing.Limits, keyed by its JSON names
function Rule:Validate(limits)
  local issues = {}
  for tier, off in pairs(percent) do
    if off > limits.maxPercent then
      issues[#issues + 1] = { severity = "error", code = "loyalty.percent", field = "tiers." .. tier,
        message = tier .. " grants " .. off .. "% off, over the " .. limits.maxPercent .. "% limit" }
    end
  end
  return issues
end
```

The Go rule (`bulk`) returns `[]plugin.Issue` from `Validate(ctx, pricing.Limits)`, and the TypeScript one
(`coupon`) an array of issues.

**The service** makes both conditions of going live, sends every lifecycle event to its log with the owning team,
and serves the admin endpoints behind its own token:

```go
c := plg.New(plg.WithLogger(logger), plg.WithOnEvent(logEvent(logger)))

plg.Register[pricing.Rule](ctx, c, "pricing", plg.WithStorage(store),
	plg.WithHost[pricing.Customers](customers),
	plg.WithValidation(func(context.Context, plg.PluginInfo) (pricing.Limits, error) {
		return pricing.Limits{MaxPercent: 10, MaxCents: 1000}, nil
	}),
	plg.WithRequiredOwnership(),
)

mux.Handle("/admin/plg/", requireToken(token, http.StripPrefix("/admin/plg", plg.AdminHandler(c))))
mux.Handle("POST /admin/check/{key}", requireToken(token, checkHandler)) // plg.Check of an uploaded zip
```

A rule over the limits, or without a team and a contact, never serves: at startup Register fails (the service
exits, its log naming the issue: `loyalty.percent: gold grants 50% off, over the 10% limit`); on a reload the last
good version serves on. Either way the event sink logs
`level=WARN msg="plugin event" kind=rejected key=pricing/loyalty … team=loyalty oncall=loyalty-oncall channel=#loyalty error=…`.

**Administrators** see every rule, its status, its reports and who owns it, and dry-run a candidate before
enabling it:

```sh
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/admin/plg/plugins/pricing/loyalty
(cd new-rule && zip -r - .) | curl -s -H "Authorization: Bearer $TOKEN" \
  localhost:8080/admin/check/new-rule --data-binary @-
# {"error":"…","key":"pricing/new-rule","version":"…","issues":[…],"loads":false}
```

The check runs the candidate's `Create` and `Validate` under the same rules as a load and publishes nothing.
The service's integration tests (`examples/service`) exercise each of these end to end.

## Author guide: from a contract to a served plugin

For a team writing plugins for a service that publishes its contract: everything from the first file to watching
the plugin serve, with the [pricing contract](examples/pricing) and the example author repository
[`examples/rules`](examples/rules), which holds a rule in each language (`bundle` in Go, `vip` in Lua, `seasonal`
in TypeScript), their test cases and committed typings, and the CI script that validates and packs them. Each
step links to the section that tells it in full. The commands run from a checkout of plg; a test
([`examples/authorguide`](examples/authorguide)) runs them as written, with the contract module served by a local
proxy instead of fetched.

**What the guide needs, and what works before it exists.** As written, the guide needs a release of plg that has
these commands (`plg init`, `dev`, `pack`, `status`, `events`), for `go install …/cmd/plg@latest`, and the tag
`examples/v1.0.0` of this repository, the contract version every plugin here pins. Neither is published yet:
`@latest` installs a plg without these commands, and resolving the contract fails with `unknown revision
examples/v1.0.0`. Until they are, work from a clone of plg:

- `go install ./cmd/plg` from the checkout installs this plg.
- Resolve the contract from the checkout with a `replace` in the author repository's `plg.yaml` (see
  [Author repositories](#author-repositories-plgyaml)): append `replace: {github.com/heynemann/go-plg/examples:
  ..}` to `examples/rules/plg.yaml`. Inside `examples/rules`, `plg init` (which adds the new plugin to that
  `plg.yaml`), `plg types`, `plg validate` and `plg dev` then work, each warning that the pin is not checked
  against the checkout. A plugin outside an author repository has no `plg.yaml`, so step 1 as written needs the
  tag.
- `plg pack`, and so `ci.sh`, refuses to pack while that `replace` is set: an artifact must be validated against
  the module its `plugin.yaml` pins. Steps 5 to 7 therefore need the tag; until then
  `cd examples && go test -tags integration ./authorguide` runs the whole guide, pack included, with the
  contract served by a local GOPROXY.

**1. Start a plugin.** Install the command, then let [`plg init`](#starting-and-iterating-plg-init-and-plg-dev)
write a plugin of the contract (manifest, typings, a skeleton of every method, a test case) and check it:

<!-- authorguide:start -->
```sh
go install github.com/heynemann/go-plg/cmd/plg@latest
plg init -implements github.com/heynemann/go-plg/examples/pricing.Rule@v1.0.0 -lang lua promo
plg validate promo
```

The contract module is fetched by the go command, so a private one needs only what any Go build needs
(`GOPRIVATE`, `.netrc`). The new plugin validates, with a warning until its `owner` block names the team (see
[Ownership](#ownership)); the service of the example requires one, as does the repository's `requireOwnership`.

**2. Iterate.** `plg dev promo` validates the plugin on every save and reads commands from stdin (see
[`plg dev`](#starting-and-iterating-plg-init-and-plg-dev)): `call Discount order.yaml` calls `Discount` in a fresh
instance with the parameters the file gives, keyed by parameter name (`order`, as in a test case's `input`), not
a bare order:

<!-- authorguide:order -->
```yaml
order: {customer: ada, coupon: SUMMER, items: [{sku: tea, quantity: 2, unitCents: 450}]}
```

<!-- authorguide:dev -->
```sh
plg dev promo
```

```text
plg dev> call Discount order.yaml
Discount: result {reason: "", cents: 0}
plg dev> quit
```

The skeleton grants nothing until you write `Discount`; each save validates it again.

**3. Test.** Each `tests/*.yaml` file holds language-neutral cases: inputs, stubbed Host calls, HTTP requests and
secrets, and the expected result, error or `Validate` issues (see [Test cases](#test-cases-testsyaml)).
[`vip`'s](examples/rules/vip/tests/vip.yaml) stub the service's `Tier`; [`bundle`'s](examples/rules/bundle/tests/bundle.yaml)
read an order from a fixture and check that `Validate` rejects the rule under a stricter service.

**4. Typings.** Lua and TypeScript plugins commit the [typings](#typings-plg-types) `plg types` writes from the
contract; validate fails, naming the command to run, when `implements` moves to a version they were not written
for. Go plugins import the contract package itself, from a module requiring the pinned version
([`bundle/go.mod`](examples/rules/bundle/go.mod)).

**5. Validate in CI, then pack.** The repository's [`plg.yaml`](examples/rules/plg.yaml) lists its plugins (see
[Author repositories](#author-repositories-plgyaml)); its [`ci.sh`](examples/rules/ci.sh) is the whole CI job:
`plg validate -format json -junit reports/validate.xml .` keeps the [JSON and JUnit
reports](#checking-a-plugin-before-registering-it-plg-validate) and fails the job on any failed plugin, then
[`plg pack -o dist .`](#packing-a-plugin-plg-pack) writes one `.plg` artifact per plugin:

<!-- authorguide:repo -->
```sh
cd examples/rules
plg types seasonal
plg validate seasonal
./ci.sh
```

`plg validate` also warns, for `bundle`, that its `types` step did not type-check the plugin: a Go `Validate`
imports plg's `plugin` package, and only the contract, vendored packages and the standard library are
type-checked (see [`plg init`](#starting-and-iterating-plg-init-and-plg-dev)). The warning does not fail the job.

**6. Serve the artifacts.** The platform publishes the artifacts to its storage and the service loads them with
`plg.OpenArtifact` (see [Storages](#storages) and [Publishing artifacts](#publishing-artifacts-to-a-storage)
below). The example service serves a directory of them with `-artifacts`
([`artifacts.go`](examples/service/artifacts.go)); it runs in the foreground until Ctrl-C, so leave it there and
run step 7 in a second terminal:

<!-- authorguide:service -->
```sh
cd .. && PRICING_ADMIN_TOKEN=s3cret go run ./service -artifacts rules/dist
```

**7. Watch it serve.** Name the environment in `~/.config/plg/profiles.yaml` and ask it, with
[`plg status` and `plg events`](#from-a-terminal-plg-status-and-plg-events), what it serves, from which commit and
contract, and what happened to it:

<!-- authorguide:profiles -->
```yaml
profiles:
  - name: local
    url: http://localhost:8080/admin/plg
    token: {env: PRICING_ADMIN_TOKEN}
```

<!-- authorguide:serving -->
```sh
export PRICING_ADMIN_TOKEN=s3cret
curl -s localhost:8080/quote -d '{"customer":"ada","coupon":"SUMMER","items":[{"sku":"tea","quantity":10,"unitCents":450}]}'
plg status
plg status pricing/vip
plg events pricing/vip
```

```text
KEY               STATUS   VERSION        LANGUAGE    TEAM        PANICS  PROBLEM
pricing/bundle    serving  sha256:633e…   go          assortment  0       -
pricing/seasonal  serving  sha256:940f…   typescript  promotions  0       -
pricing/vip       serving  sha256:1d62…   lua         loyalty     0       -
```

`plg status pricing/vip` adds, among the owner, pool and memory, the artifact's `provenance` (`commit 3b1f…,
contract github.com/heynemann/go-plg/examples v1.0.0, packed by plg v…`), and `plg events` its loads, reloads,
rejections and panics.

**8. Secrets and traces.** A plugin calling a third-party API declares the secrets it reads in `plugin.yaml` and
reads them at run time; the service resolves them (`plg.WithSecrets`), and test cases fake them (see
[Secrets](#secrets)). Each call is a span `plg.call <key>.<method>` in the service's trace, with the plugin's Host
calls and HTTP requests below it and its log lines carrying `trace_id` (see
[Logs, metrics and traces](#logs-metrics-and-traces)).

### Platform guidance

#### Publishing artifacts to a storage

A `.plg` artifact is one immutable blob named by its key, which is how object stores and databases like to keep
things. Publish from the CI job that packed it (after `plg pack` succeeded, so only validated plugins reach the
storage), under a path the platform owns (`plugins/<key>.plg`), and let the service's storage list those blobs
and return `plg.OpenArtifact` of each from `Open` (see [Storages](#storages)); the [S3](examples/s3storage) and
[PostgreSQL](examples/pgstorage) examples show listing and versioning, and `examples/service/artifacts.go` a
directory of artifacts. With [polling](#reloading-and-polling) a published artifact goes live without a restart,
and a bad one keeps the last good version serving. Decide from the provenance what to accept (a `dirty` artifact,
an old contract, a commit not on the main branch): plg loads whatever it is given.

#### Designing contracts for large data: bounded predicates and pages

Every call crosses into WebAssembly and back, its arguments and results serialized each way, under the
plugin's memory limit and timeout. Keep both sides of a call bounded:

- **Ask predicates, not sets.** "Is this user in the cohort?" (`IsMember(ctx, user) (bool, error)`) scales with
  one user; "return every user of the cohort" scales with the cohort and eventually exceeds the memory limit.
- **Page what must be listed.** When the service does need the members, take a cursor and a page size and
  return one page and the next cursor (`List(ctx, cursor string, limit int) (Page, error)`), with the service
  enforcing the limit; the plugin's memory then holds one page, whatever the data's size.
- **Let the plugin pull, through the Host.** Data the service holds reaches plugins through
  [Host](#calling-back-into-the-service-the-host-interface) methods of the same shape (a lookup by key, a page),
  never as one argument carrying everything.

#### Side effects: idempotency keys and at-least-once delivery

A plugin call can fail after its effect happened: a timeout cuts it after the HTTP request reached the provider,
an instance traps after a write, the service retries or restarts. Contracts with side effects (delivering a
reward, sending a message) are therefore at least once, and must make repeating harmless:

- **The service passes an idempotency key** in the call (`Deliver(ctx, delivery Delivery)` where `Delivery`
  carries a key derived from what is being delivered, such as the reward and user ids, never a fresh random value
  per attempt), and plugins hand it on (an `Idempotency-Key` header, a unique constraint) so the provider applies
  the effect once.
- **The service retries until it knows the outcome**, recording delivery as done only after a successful call,
  and treats a plugin's error as "may have happened": the key makes the retry safe.
- **Results say what happened**, so a retry answering "already delivered" is a success, not an error.

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
`plg warm -validate` also compiles the TypeScript compiler
[`plg validate`](#checking-a-plugin-before-registering-it-plg-validate) runs, for CI images that validate plugins;
services never need it.

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
| Allocations per call (host side) | 9 | 10 | 9 |
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

Tracing (`BenchmarkCallLuaTraced`, Lua, one string in and out): no provider installed, or a no-op one, adds no
allocation and no measurable time to a call; an SDK provider recording the call's span adds ≈ 1.4 µs and 6
allocations (2.5 KB) per call, before any export.

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
  joined into one, as in `fetch`. Bundling in WebAssembly is slow for large plugins (about 3 s per MB of
  source), and a plugin whose bundling outlasts 10 s plus 5 s per MiB of its files fails to load.
- **`WithConcurrentCalls`:** interleaved calls share the plugin's global state and memory limit; a call abandoned
  at its deadline (or finished with requests still in flight) still runs its remaining code, within 250 ms in all
  from its end however many steps that takes: a call still running then, say retrying a failing request in a loop,
  discards the instance (its other calls fail with `plg.ErrInstanceLost`); an abandoned TypeScript call with no
  `fetch` in flight is dropped at once, its result discarded, though code awaiting what another call settles later
  still runs then; a Lua request that falls back to blocking counts against `MaxRequestsPerCall` per step rather
  than per call; in TypeScript a request belongs to the call whose step made it, so a promise shared between calls
  through global state is attributed to the call that started it.
- **Rebuilds:** the guest artifacts must be rebuilt and re-verified (`make verify-guests`) whenever wazero,
  wasi-sdk, Lua, QuickJS-ng, esbuild, yaegi, typescript-go or Go are upgraded.

## Examples

[`examples/`](examples) is a separate Go module, so plg's own `go.mod` never gains aws-sdk, pgx or testcontainers:

- [`examples/pricing`](examples/pricing): a contract (`Rule`, the `Customers` host, the `Limits` validation
  input) and its generated adapter.
- [`examples/plugins`](examples/plugins): three rules, in Go (`bulk`), Lua (`loyalty`, calls the host) and
  TypeScript (`coupon`, throws on an unknown code), each naming its owning team in its `plugin.yaml` and
  validating itself against the service's limits.
- [`examples/service`](examples/service): an HTTP pricing service serving those rules, with a host, a memory
  budget, every rule instance started before serving, per-call deadlines, validation and required ownership, a
  lifecycle event log, the admin handler and a `plg.Check` upload endpoint behind a token
  ([walkthrough](#putting-it-together-fit-to-serve-and-who-to-talk-to)), optional polling of a plugin directory
  or of a directory of packed artifacts (`-artifacts`), and a clean shutdown: `cd examples && go run ./service`.
- [`examples/rules`](examples/rules): an author repository of three more rules of the same contract (Go, Lua,
  TypeScript) with its `plg.yaml`, test cases, committed typings and the CI script validating and packing them,
  which the [author guide](#author-guide-from-a-contract-to-a-served-plugin) walks through.
- [`examples/s3storage`](examples/s3storage): a `plg.Storage` over S3, tested against MinIO.
- [`examples/pgstorage`](examples/pgstorage): a `plg.Storage` over PostgreSQL listing only enabled plugins, with
  its migration, tested against PostgreSQL.
- [`examples/quickstart`](examples/quickstart): runs this README's quick start, and
  [`examples/authorguide`](examples/authorguide) its author guide, with the service on the artifacts it packs.

Their tests start MinIO and PostgreSQL with [testcontainers](https://golang.testcontainers.org/), so they need
Docker, and the author repository's tests run its CI script in a git repository and the author guide's commands, so
they need `git`, `sh` and `curl` too:
`make examples`. Each top-level test starts its own server and terminates it through `t.Cleanup`
(RULES.md §7), so a run leaves no container behind whether it passes or fails.

## Development

- `make check` runs every gate CI runs: formatting, golangci-lint (host and wasip1), the unit and integration
  tests with `-race` (after `make warm` compiles the guests and the TypeScript compiler into the compilation
  cache without it; each test run
  has a `-timeout` to catch hangs), the 95% per-package coverage gate (on that run's profile; `scripts/gate-test.sh`
  tests it and checks that every test container is terminated through its test's cleanup), allocation and
  benchmark budgets, a `-count=10` repeat of the unit tests, and `make examples` (lint and tests of the examples
  module, Docker, `git`, `sh` and `curl` required).
- `make guests` rebuilds the guest artifacts with pinned toolchains, and the TypeScript compiler `plg validate`
  runs (`internal/tscheck/tsgo.wasm.gz`: typescript-go's `tsgo`, pinned by the module in `guestsrc/tscheck`, built
  for wasip1); `make verify-guests` checks that the committed ones are reproducible.
- [`RULES.md`](RULES.md) holds the engineering rules every change follows; [`PLAN.md`](PLAN.md) the design and
  the log of decisions and measurements.
