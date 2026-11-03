package plg

import (
	"fmt"
	"log/slog"
	"runtime"
	"slices"

	"github.com/heynemann/go-plg/internal/engine"
)

// Option configures a [Container]; pass it to [New].
type Option func(*containerConfig)

type containerConfig struct {
	logger       *slog.Logger
	cacheDir     string
	memoryBudget uint64
}

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
// container compiles. The default is os.UserCacheDir()/plg/<wazero version>. A directory that cannot be
// created or written falls back to an in-memory cache, with a warning.
func WithCompilationCacheDir(dir string) Option {
	return func(c *containerConfig) {
		c.cacheDir = dir
	}
}

// RegisterOption configures one Register call.
type RegisterOption func(*registerConfig)

// maxPoolSize is the largest pool maximum [WithPool] accepts. Each pool keeps room for that many idle
// instances, so the bound keeps a mistyped maximum from allocating gigabytes.
const maxPoolSize = 4096

type registerConfig struct {
	http        *HTTPPolicy // nil: HTTP refused
	storages    []Storage
	languages   []Language // enabled languages; empty enables every language
	memoryLimit uint64     // per-instance limit, whole pages
	poolMin     int
	poolMax     int
	poolSet     bool
	limitSet    bool
}

// enabled reports whether the Register runs plugins in lang.
func (c *registerConfig) enabled(lang Language) bool {
	return len(c.languages) == 0 || slices.Contains(c.languages, lang)
}

// registerOptions applies a Register call's options and checks them: at least one non-nil storage, and valid
// pool and memory options, whose defaults it fills in.
func registerOptions(name string, opts []RegisterOption) (*registerConfig, error) {
	cfg := &registerConfig{}

	for _, opt := range opts {
		opt(cfg)
	}

	if len(cfg.storages) == 0 || slices.Contains(cfg.storages, nil) {
		return nil, fmt.Errorf("%w: register %s with at least one non-nil WithStorage", ErrNoStorage, name)
	}

	err := cfg.settle()
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
