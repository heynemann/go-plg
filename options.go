package plg

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
)

// Option configures a [Container]; pass it to [New].
type Option func(*containerConfig)

type containerConfig struct {
	logger        *slog.Logger
	meterProvider metric.MeterProvider // nil: the global provider
	onPanic       func(context.Context, *PanicError)
	onEvent       func(context.Context, Event)
	cacheDir      string
	memoryBudget  uint64
	panicHistory  int
}

// defaultPanicHistory is how many PanicErrors [Container.RecentPanics] keeps by default.
const defaultPanicHistory = 64

// WithLogger sets the logger for loads, reloads, cache warnings, HTTP requests, plugin failures and the plugins'
// own output (stdout at Info, stderr at Warn). Every call plg makes into its handler is guarded: a panic there is
// a [PanicError] in [PhaseCallback], the record is dropped and what was logging carries on, its state intact. The
// default discards everything.
func WithLogger(logger *slog.Logger) Option {
	return func(c *containerConfig) {
		c.logger = logger
	}
}

// WithMemoryBudget caps, in bytes, the memory all plugin instances of the container may use together. Register
// counts the worst case of every plugin it loads, pool maximum × per-instance memory limit, and fails with
// [ErrMemoryBudget] when the plugins registered so far and the new ones would exceed the budget together; pools
// never grow past it at run time either. Zero, the default, means no budget.
func WithMemoryBudget(total uint64) Option {
	return func(c *containerConfig) {
		c.memoryBudget = total
	}
}

// WithCompilationCacheDir sets the directory of the on-disk compilation cache shared by every guest the
// container compiles. The default is the PLG_CACHE_DIR environment variable when it is set, otherwise
// os.UserCacheDir()/plg/<wazero version>; [Warm] and `plg warm` resolve it the same way, so a cache they fill
// is the one a container reads. A directory that cannot be created or written falls back to an in-memory
// cache, with a warning.
func WithCompilationCacheDir(dir string) Option {
	return func(c *containerConfig) {
		c.cacheDir = dir
	}
}

// WithOnPanic calls fn with every [PanicError] the container reports, synchronously, on the goroutine that
// met the failure (the caller's for a call). fn must be safe for concurrent use and must not block for long. A
// panic in fn is recovered, logged and kept in [Container.RecentPanics] with [PhaseCallback]; it is not passed
// to fn again.
func WithOnPanic(fn func(ctx context.Context, pe *PanicError)) Option {
	return func(c *containerConfig) {
		c.onPanic = fn
	}
}

// WithPanicHistory sets how many of the newest PanicErrors [Container.RecentPanics] returns; zero or less
// keeps none. The default is 64.
func WithPanicHistory(n int) Option {
	return func(c *containerConfig) {
		c.panicHistory = n
	}
}

// RegisterOption configures one Register call.
type RegisterOption func(*registerConfig)

// maxPoolSize is the largest pool maximum [WithPool] accepts. Each pool keeps room for that many idle
// instances, so the bound keeps a mistyped maximum from allocating gigabytes.
const maxPoolSize = 4096

// maxConcurrentCalls bounds [WithConcurrentCalls], and maxLanes the calls a plugin's whole pool may serve at once
// (pool maximum × concurrent calls): the pool keeps room for that many idle slots, 8 bytes each.
const (
	maxConcurrentCalls = 4096
	maxLanes           = 1 << 20
)

type registerConfig struct {
	http              *HTTPPolicy     // nil: HTTP refused
	httpRoots         *x509.CertPool  // the https roots trusted; nil: the system's (set only by tests, see export_test.go)
	goGuest           *guestreg.Guest // the WithGoGuest option's custom guest; nil runs the linked one
	ownership         Severity        // of a missing-ownership issue: a warning, an error with WithRequiredOwnership
	storages          []Storage
	languages         []Language         // enabled languages; empty enables every language
	hosts             []hostSpec         // WithHost options; at most one is valid
	goGuests          [][]byte           // WithGoGuest options; at most one is valid
	validations       []validationSpec   // WithValidation options; at most one is valid
	policies          []ValidationPolicy // WithOnDemandValidationPolicy options; at most one is valid
	memoryLimit       uint64             // per-instance limit, whole pages
	pollInterval      time.Duration      // zero: no polling
	validationTimeout time.Duration      // WithValidationTimeout
	poolMin           int
	poolMax           int
	concurrent        int // WithConcurrentCalls
	poolSet           bool
	limitSet          bool
	concurrentSet     bool
}

// enabled reports whether the Register runs plugins in lang.
func (c *registerConfig) enabled(lang Language) bool {
	return len(c.languages) == 0 || slices.Contains(c.languages, lang)
}

// registerOptions applies a Register call's options and checks them: at least one non-nil storage, valid pool
// and memory options, whose defaults it fills in, and a poll interval that is not negative.
func registerOptions(name string, opts []RegisterOption) (*registerConfig, error) {
	cfg := &registerConfig{ownership: SeverityWarning, validationTimeout: defaultValidationTimeout}

	for _, opt := range opts {
		opt(cfg)
	}

	if len(cfg.storages) == 0 || slices.Contains(cfg.storages, nil) {
		return nil, fmt.Errorf("%w: register %s with at least one non-nil WithStorage", ErrNoStorage, name)
	}

	if cfg.pollInterval < 0 {
		return nil, fmt.Errorf("%w: WithPollInterval(%s): want a positive interval, or zero not to poll",
			ErrInvalidOption, cfg.pollInterval)
	}

	err := cfg.customGoGuest()
	if err != nil {
		return nil, err
	}

	err = cfg.settle()
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// settle validates the pool and memory options and fills in the pool's default, 1 to GOMAXPROCS instances, and
// one call per instance. Without WithMemoryLimit, each plugin gets its language's default limit
// ([registerConfig.limit]).
func (c *registerConfig) settle() error {
	switch {
	case !c.poolSet:
		c.poolMin, c.poolMax = 1, min(runtime.GOMAXPROCS(0), maxPoolSize)
	case c.poolMin < 1 || c.poolMin > c.poolMax || c.poolMax > maxPoolSize:
		return fmt.Errorf("%w: WithPool(%d, %d): want 1 <= min <= max <= %d", ErrInvalidOption, c.poolMin,
			c.poolMax, maxPoolSize)
	default: // the pool given is valid
	}

	err := c.settleConcurrentCalls()
	if err != nil {
		return err
	}

	if c.limitSet {
		if !validMemoryLimit(c.memoryLimit) {
			return fmt.Errorf("%w: WithMemoryLimit(%d): want from one %d-byte page to 4 GiB", ErrInvalidOption,
				c.memoryLimit, minMemoryLimit)
		}

		c.memoryLimit = wholePages(c.memoryLimit)
	}

	return nil
}

// settleConcurrentCalls validates WithConcurrentCalls against the pool's maximum, by default one call per
// instance.
func (c *registerConfig) settleConcurrentCalls() error {
	switch {
	case !c.concurrentSet:
		c.concurrent = 1
	case c.concurrent < 1 || c.concurrent > maxConcurrentCalls:
		return fmt.Errorf("%w: WithConcurrentCalls(%d): want 1 to %d", ErrInvalidOption, c.concurrent,
			maxConcurrentCalls)
	case c.concurrent*c.poolMax > maxLanes:
		return fmt.Errorf("%w: WithConcurrentCalls(%d) with a pool of up to %d instances: want at most %d calls "+
			"at once in all", ErrInvalidOption, c.concurrent, c.poolMax, maxLanes)
	default: // the number of calls given is valid
	}

	return nil
}

// limit returns the Register's per-instance memory limit for plugins of a language: WithMemoryLimit's, or the
// language's default.
func (c *registerConfig) limit(ld loader) uint64 {
	if c.limitSet {
		return c.memoryLimit
	}

	return ld.memoryLimit()
}

// wholePages rounds a memory limit down to whole wasm pages.
func wholePages(limit uint64) uint64 {
	return limit / engine.PageSize * engine.PageSize
}

// WithStorage adds a storage whose plugins implement the registered interface. Register needs at least one;
// repeat the option to load from several.
func WithStorage(s Storage) RegisterOption {
	return func(c *registerConfig) {
		c.storages = append(c.storages, s)
	}
}

// WithPollInterval makes the Register's plugins follow their storages: every interval, a background goroutine
// reloads them as [ReloadInterface] does, so changes are served without an explicit reload. Polling lists the
// storages each time but reloads only the plugins whose descriptor or version changed. It must not be negative,
// otherwise Register fails with [ErrInvalidOption]; zero, the default, does not poll.
func WithPollInterval(interval time.Duration) RegisterOption {
	return func(c *registerConfig) {
		c.pollInterval = interval
	}
}

// WithRequiredOwnership makes ownership a condition of loading the Register's plugins. Every load checks that a
// plugin's plugin.yaml names its owning team (owner.team.name) and a way to reach it (one of owner.contact.oncall,
// owner.contact.channel and owner.contact.email); without the option a plugin lacking either loads with a
// [CodeOwnershipMissing] warning in its load [Report] and in the log. With it, that issue is an error that blocks
// the load like any load failure: Register fails with [ErrLoad] wrapping [ErrBlockingIssue], naming the plugin
// and the missing fields, and a reload keeps serving the plugin's last good version.
func WithRequiredOwnership() RegisterOption {
	return func(c *registerConfig) {
		c.ownership = SeverityError
	}
}

// WithPool sets how many instances serve each plugin of the Register; each instance runs its own Create and
// serves one call at a time. Register starts minimum instances, and a background goroutine per plugin keeps
// that many warm, replacing discarded ones (it retries a failing Create with exponential backoff, logging each
// failure at Warn). A call takes an idle instance, starts a new one while the plugin has fewer than maximum
// and the container's [WithMemoryBudget] allows it, and otherwise waits for one on its context.
//
// It needs 1 <= minimum <= maximum <= 4096, otherwise Register fails with [ErrInvalidOption]: one warm instance
// at least means Register proves each plugin's Create works and no call to an idle plugin waits for a cold
// start. The default is WithPool(1, GOMAXPROCS).
func WithPool(minimum, maximum int) RegisterOption {
	return func(c *registerConfig) {
		c.poolMin, c.poolMax, c.poolSet = minimum, maximum, true
	}
}

// WithConcurrentCalls lets each instance of the Register's Lua and TypeScript plugins serve up to n calls at
// once. While a call waits for an HTTP request (an awaited fetch in TypeScript, http.request in Lua), its instance
// runs other calls instead of idling, so a few instances serve many I/O-bound calls: a Lua or TypeScript instance
// costs about 3 MiB of host memory (its linear memory starts with a 2 MiB stack), an extra call in flight on it a
// few KiB. Go plugins always serve one call per instance; the option does not apply to them. With the option, an
// instance counts as one in [WithPool] and in the memory budget however many calls it serves.
//
// The calls on one instance share it as concurrent requests share a Node.js process: they never see each other's
// arguments or results, but they do see the plugin's global state, which may change while a call waits (in Lua,
// while http.request yields), and they share the instance's memory limit. Each call keeps its own context: when it
// ends while the call waits for its requests, the call fails with the context's error and the instance serves on
// (the plugin sees the requests fail, and the code it runs afterwards gets 250 ms in all, else the instance is
// discarded); a TypeScript call with no fetch in flight, awaiting what only another call (or nothing) settles, is
// dropped from the instance at once instead. A call that traps, runs out of memory, panics, breaks the guest ABI or
// is stopped at its deadline while running discards the instance: it fails with a [PanicError] as usual, and every
// other call on the instance with [ErrInstanceLost].
// A Lua plugin's request made where its call cannot yield (inside a coroutine of the plugin's own, or a callback
// of a C function such as table.sort's comparator) blocks the instance, as without the option.
//
// n must be from 1 (the default: one call per instance) to 4096, and n times the pool's maximum at most 1048576
// (the pool keeps room for that many calls), otherwise Register fails with [ErrInvalidOption].
func WithConcurrentCalls(n int) RegisterOption {
	return func(c *registerConfig) {
		c.concurrent, c.concurrentSet = n, true
	}
}

// WithMemoryLimit caps the linear memory of each instance of the Register's plugins, in bytes, rounded down to
// whole 64 KiB wasm pages. A call that needs more fails and its instance is discarded; other instances are
// unaffected. A plugin's [Descriptor.MemoryLimit] (memoryLimit in plugin.yaml) may lower the limit, never
// raise it. It must be from 64 KiB to 4 GiB, otherwise Register fails with [ErrInvalidOption]. The default
// depends on the plugin's language: 128 MiB for Go plugins, 64 MiB for Lua and TypeScript plugins.
func WithMemoryLimit(limit uint64) RegisterOption {
	return func(c *registerConfig) {
		c.memoryLimit, c.limitSet = limit, true
	}
}

// WithGolangPlugins enables Go plugins. The language options form an allowlist: without any, every language is
// enabled; with some, plugins in the other languages are skipped (and logged) rather than failing Register.
func WithGolangPlugins() RegisterOption {
	return withLanguage(LanguageGo)
}

// WithLuaPlugins enables Lua plugins; see [WithGolangPlugins] for how the language options combine.
func WithLuaPlugins() RegisterOption {
	return withLanguage(LanguageLua)
}

// WithTypescriptPlugins enables TypeScript plugins; see [WithGolangPlugins] for how the language options combine.
func WithTypescriptPlugins() RegisterOption {
	return withLanguage(LanguageTypeScript)
}

func withLanguage(lang Language) RegisterOption {
	return func(c *registerConfig) {
		c.languages = append(c.languages, lang)
	}
}

// WithGoGuest runs the Register's Go plugins in a custom Go guest instead of the one guest/golang links: wasm is
// a module `plg build-guest` built, which adds packages to the standard library allowlist, so the plugins may
// import them (they may import exactly the default allowlist plus those packages; anything else fails to load,
// as with the default guest). The program need not link guest/golang then. The custom guest runs under the same
// rules as the default one: the same plugin checks, memory limits, CPU cap, pool, panic containment, Host and
// HTTP.
//
// Register keeps wasm without copying it (an embedded file, typically), so it must not be modified afterwards.
// Registers given the same module share one compiled copy per container; compiling it is the service's cold
// start (`plg warm` warms the linked guests only). wasm must not be empty and the option may be given once,
// otherwise Register fails with [ErrInvalidOption]; a module that is not a Go guest fails Register with
// [ErrLoad].
func WithGoGuest(wasm []byte) RegisterOption {
	return func(c *registerConfig) {
		c.goGuests = append(c.goGuests, wasm)
	}
}

// customGoGuest checks the WithGoGuest options and sets the custom guest, identified by its SHA-256.
func (c *registerConfig) customGoGuest() error {
	switch {
	case len(c.goGuests) == 0:
		return nil
	case len(c.goGuests) > 1:
		return fmt.Errorf("%w: WithGoGuest given %d times; a Register runs one Go guest", ErrInvalidOption,
			len(c.goGuests))
	case len(c.goGuests[0]) == 0:
		return fmt.Errorf("%w: WithGoGuest: the module is empty", ErrInvalidOption)
	}

	wasm := c.goGuests[0]
	digest := sha256.Sum256(wasm)
	c.goGuest = &guestreg.Guest{
		Language: string(LanguageGo),
		Wasm:     func() ([]byte, error) { return wasm, nil },
		ID:       "sha256:" + hex.EncodeToString(digest[:]),
	}

	return nil
}

// guestFor returns the loader of lang and the guest that runs the Register's lang plugins: its custom Go guest,
// or the linked guest.
//
//nolint:ireturn // the loader is the per-language seam
func (c *registerConfig) guestFor(lang Language) (loader, guestreg.Guest, error) {
	if lang == LanguageGo && c.goGuest != nil {
		return goLoader{}, *c.goGuest, nil
	}

	return guestFor(lang)
}
