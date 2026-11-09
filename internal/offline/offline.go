// Package offline is how `plg validate` loads a plugin with plg's own loading code, without the service that
// compiles the contract in: the command describes the contract from its source ([Contract]) and package plg, which
// sets [Check] and [Open] when it is linked, loads the plugin against it as plg.Check loads a candidate plugin
// version, and runs its test cases in instances of their own.
package offline

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/directive"
	"github.com/heynemann/go-plg/internal/goabi"
)

// Contract is a contract as plg validate resolved it: the interface a plugin implements, described from the
// contract's source.
type Contract struct {
	// Interface is the interface as the Go dispatcher writes it.
	Interface *goabi.Interface
	// Host is the Host the plugin's Create takes; nil when it takes none.
	Host *Host
	// Validation is the contract's validation input; nil when it declares none.
	Validation *Validation
	// Source holds the Go source files of the interface's package at its root.
	Source fs.FS
	// Module is the contract module plg validate resolved at the plugin's pinned version.
	Module contractref.Module
	// Methods are the interface's methods, in Interface's order, of the types the Go interpreter gives the
	// contract's types: the types plg plans the script guests' schema from.
	Methods []reflect.Method
	// Params are the names of each method's parameters after the context, in Methods' order (see [ParamName]).
	Params [][]string
}

// Host is a Host interface of the contract.
type Host struct {
	// Interface is the Host as the Go dispatcher writes it.
	Interface *goabi.Interface
	// Methods are the Host's methods, as Contract's.
	Methods []reflect.Method
	// Params are the names of each method's parameters after the context, as Contract's.
	Params [][]string
	// Funcs implement the methods, one func of the method's type each, in Methods' order. Nil: plg validate runs
	// the plugin without the service's Host, and every call of one of its methods fails.
	Funcs []reflect.Value
}

// Validation is the validation input the contract declares: the type plg.WithValidation registers and a
// plugin's Validate takes.
type Validation struct {
	// Type is the validation input as the Go dispatcher writes it: a named type of the contract package.
	Type *goabi.Type
	// Input is the type the Go interpreter gives it.
	Input reflect.Type
}

// ParamName names the parameter at index i of a method (the context being 0) that its declaration leaves unnamed
// or names _: p<i>, as plg gen names the parameters of the adapters it generates.
func ParamName(i int) string {
	return "p" + strconv.Itoa(i)
}

// Plugin is the plugin plg validate checks: its directory.
type Plugin struct {
	// Files are the plugin's files, plugin.yaml among them.
	Files fs.FS
	// Key is the plugin's key: its directory's name, as a storage of plugin directories keys it.
	Key string
	// Manifest is the content of its plugin.yaml.
	Manifest []byte
}

// Options configure [Check] and [Open].
type Options struct {
	// Logger gets what the plugin writes and what plg logs while loading it.
	Logger *slog.Logger
	// Secrets answers the plugin's secret reads (test cases' fakes), with the context of the call or Create making
	// the read, every read resolving afresh; nil fails every read (plg validate has no secrets outside test cases).
	Secrets func(ctx context.Context, name string) (string, error)
	// CacheDir is the guests' compilation cache directory; empty means plg's default.
	CacheDir string
	// GoGuest is the custom Go guest (plg.WithGoGuest) a Go plugin runs in; nil runs the linked one.
	GoGuest []byte
	// Limits are the limits the plugin runs under: its memory limit (plg.WithMemoryLimit) when set, and its Timeout
	// bounds Create and each call a [Runner] runs (zero: Check sets no bound, a Runner [DefaultTimeout]). Its pool
	// is checked as plg.WithPool checks one, and changes nothing: each runs one instance. Unset ones are plg's
	// defaults ([Defaults]).
	Limits directive.Limits
	// RequireOwnership makes missing ownership an error, as plg.WithRequiredOwnership does.
	RequireOwnership bool
}

// DefaultTimeout bounds the plugin's Create and each call a [Runner] runs when [Options] set no Timeout.
const DefaultTimeout = 10 * time.Second

// Check loads plugin against contract and runs its Create, with the stub Host when contract has one, in a throwaway
// instance, as plg.Check runs a candidate. It returns the load's report, whose Error is why the plugin would not
// load; it fails when the manifest is invalid, the contract is not one plg can serve, or the check could not run.
// Package plg sets it.
//
//nolint:gochecknoglobals // the one seam from the command to plg's unexported loading code, set from plg's init
var Check func(ctx context.Context, contract *Contract, plugin *Plugin, opts *Options) (*adminjson.Report, error)

// CheckHost checks host, of the contract package at import path pkgPath, as [Check] checks a contract's Host (as
// plg.WithHost checks one): plg validate offers the contract's interfaces that pass as the default Host. Package plg
// sets it.
//
//nolint:gochecknoglobals // the seam's second entry, set with Check from plg's init
var CheckHost func(pkgPath string, host *Host) error

// Defaults returns the limits plg runs plugins of language under without options: its memory limit and pool
// (no timeout: a service bounds calls with their contexts). Package plg sets it.
//
//nolint:gochecknoglobals // plg's defaults, which the command cannot import, set from plg's init with Check
var Defaults func(language string) directive.Limits

// Open prepares plugin, which loads against contract (see [Check]), for its test cases: the returned [Runner]
// runs each call in a fresh instance, with contract's Host Funcs (and Validation, if any). It fails when the
// manifest is invalid or the contract is not one plg can serve. Package plg sets it.
//
//nolint:gochecknoglobals // the seam from the command to plg's unexported loading code, set from plg's init
var Open func(ctx context.Context, contract *Contract, plugin *Plugin, opts *Options) (Runner, error)

// Runner runs calls of one plugin, each in a fresh instance. It is used by one goroutine at a time.
type Runner interface {
	// Run starts a fresh instance, running the plugin's Create, calls one method in it and closes it. Create
	// and the call are each bounded by the Options' Timeout; the Host Funcs and call.HTTP's Transport get the
	// context Run was given, through the contexts the instance's Create and call run under.
	Run(ctx context.Context, call *Call) Outcome
	// Secrets returns the names of the secrets the plugin's manifest declares.
	Secrets() []string
	// Close frees what the runner holds.
	Close(ctx context.Context) error
}

// Call is a call a [Runner] runs.
type Call struct {
	// HTTP is the policy the plugin's requests are made under, to its Transport; nil refuses every request. A
	// runner prepares the plugin once per HTTP value it is given.
	HTTP *HTTP
	// Args are the arguments after the context, of the method's types; Validate's is the validation input.
	Args []reflect.Value
	// Method is the index of the method in the contract's Methods; len(Methods) calls the plugin's Validate.
	Method int
}

// HTTP is an HTTP policy, as plg.HTTPPolicy's fields, whose requests go to Transport instead of the network:
// Transport is never dialed, so the policy's private-network protection has nothing to check.
type HTTP struct {
	Transport          http.RoundTripper
	AllowedHosts       []string
	Timeout            time.Duration
	MaxRequestBytes    int
	MaxResponseBytes   int
	MaxRequestsPerCall int
}

// Outcome is what a [Runner] call returned.
type Outcome struct {
	// Err is why the call failed: the instance could not start (its Create failed or panicked), the call
	// failed, panicked, trapped or ran past the timeout, or its arguments or results could not cross.
	Err error
	// Returned is the message of the error the plugin returned (or threw) from the method; empty when it did
	// not return one, the call failing otherwise or succeeding.
	Returned string
	// Results are the method's results before its error, when it succeeded.
	Results []reflect.Value
	// Issues are the issues Validate returned, when it succeeded.
	Issues []adminjson.Issue
	// Resources are what the call took in its instance; zero when no instance started (its Create failed).
	Resources Resources
}

// Resources are what a [Runner] call took, against the limits it ran under.
type Resources struct {
	// Memory is the size of the instance's linear memory once the call returned: its peak, since wasm memory
	// only grows (what Create took included).
	Memory uint64
	// MemoryLimit is the instance's memory limit.
	MemoryLimit uint64
	// Duration is how long the call ran, Create not included.
	Duration time.Duration
	// Timeout bounds the call.
	Timeout time.Duration
}

// CheckContract checks contract's interface and Host as [Check] checks them before loading a plugin, failing with
// the error Check would fail with: plg validate names what breaks a contract it cannot describe for the Lua and
// TypeScript typings. Package plg sets it.
//
//nolint:gochecknoglobals // the seam's third entry, set with Check from plg's init
var CheckContract func(contract *Contract) error

// Ownership checks the owner block of the plugin.yaml manifest as a load checks it: it returns the missing-
// ownership issue (an error when required, as plg.WithRequiredOwnership makes it, else a warning), none when the
// owner is complete; it fails when the manifest is invalid. Package plg sets it.
//
//nolint:gochecknoglobals // the seam's fourth entry, set with Check from plg's init
var Ownership func(manifest []byte, required bool) ([]adminjson.Issue, error)

// LuaSyntax compiles every .lua file of a Lua plugin's files with the Lua guest's compiler, without running any,
// and returns the syntax error of each that does not compile (file:line: message), in name order: plg validate
// checks the modules a plugin requires only when a call needs them too. It fails when the guest cannot run. Package
// plg sets it.
//
//nolint:gochecknoglobals // the seam's fifth entry, set with Check from plg's init
var LuaSyntax func(ctx context.Context, files fs.FS, opts *Options) ([]string, error)
