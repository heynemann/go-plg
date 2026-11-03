package plg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"time"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
)

// Option configures a [Container]; pass it to [New].
type Option func(*containerConfig)

type containerConfig struct {
	logger       *slog.Logger
	onPanic      func(context.Context, *PanicError)
	cacheDir     string
	memoryBudget uint64
	panicHistory int
}

// defaultPanicHistory is how many PanicErrors [Container.RecentPanics] keeps by default.
const defaultPanicHistory = 64

// WithLogger sets the logger for loads, cache warnings and plugin failures. The default discards everything.
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

type registerConfig struct {
	http         *HTTPPolicy     // nil: HTTP refused
	goGuest      *guestreg.Guest // the WithGoGuest option's custom guest; nil runs the linked one
	storages     []Storage
	languages    []Language    // enabled languages; empty enables every language
	hosts        []hostSpec    // WithHost options; at most one is valid
	goGuests     [][]byte      // WithGoGuest options; at most one is valid
	memoryLimit  uint64        // per-instance limit, whole pages
	pollInterval time.Duration // zero: no polling
	poolMin      int
	poolMax      int
	poolSet      bool
	limitSet     bool
}

// enabled reports whether the Register runs plugins in lang.
func (c *registerConfig) enabled(lang Language) bool {
	return len(c.languages) == 0 || slices.Contains(c.languages, lang)
}

// registerOptions applies a Register call's options and checks them: at least one non-nil storage, valid pool
// and memory options, whose defaults it fills in, and a poll interval that is not negative.
func registerOptions(name string, opts []RegisterOption) (*registerConfig, error) {
	cfg := &registerConfig{}

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

// settle validates the pool and memory options and fills in their defaults: a pool of 1 to GOMAXPROCS
// instances and the Go guest's memory limit (every guest linked so far is Go's; the Lua and TypeScript guests
// bring their own 64 MiB default with them).
func (c *registerConfig) settle() error {
	switch {
	case !c.poolSet:
		c.poolMin, c.poolMax = 1, min(runtime.GOMAXPROCS(0), maxPoolSize)
	case c.poolMin < 1 || c.poolMin > c.poolMax || c.poolMax > maxPoolSize:
		return fmt.Errorf("%w: WithPool(%d, %d): want 1 <= min <= max <= %d", ErrInvalidOption, c.poolMin,
			c.poolMax, maxPoolSize)
	default: // the pool given is valid
	}

	switch {
	case !c.limitSet:
		c.memoryLimit = goMemoryLimit
	case c.memoryLimit < engine.PageSize || c.memoryLimit > maxMemoryLimit:
		return fmt.Errorf("%w: WithMemoryLimit(%d): want from one %d-byte page to 4 GiB", ErrInvalidOption,
			c.memoryLimit, engine.PageSize)
	default:
		c.memoryLimit = wholePages(c.memoryLimit)
	}

	return nil
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

// WithMemoryLimit caps the linear memory of each instance of the Register's plugins, in bytes, rounded down to
// whole 64 KiB wasm pages. A call that needs more fails and its instance is discarded; other instances are
// unaffected. A plugin's [Descriptor.MemoryLimit] (memoryLimit in plugin.json) may lower the limit, never
// raise it. It must be from 64 KiB to 4 GiB, otherwise Register fails with [ErrInvalidOption]. The default is
// 128 MiB for Go plugins.
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

// guestFor returns the guest that runs the Register's lang plugins: its custom Go guest, or the linked guest.
func (c *registerConfig) guestFor(lang Language) (guestreg.Guest, error) {
	if lang == LanguageGo && c.goGuest != nil {
		return *c.goGuest, nil
	}

	return guestFor(lang)
}
