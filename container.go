// Package plg runs plugins written as source code, inside WebAssembly sandboxes, behind ordinary Go
// interfaces.
//
// A service declares an interface, generates its adapter with `plg gen`, links the guest of every plugin
// language it accepts and registers the interface with storages listing the plugins:
//
//	import (
//		"github.com/heynemann/go-plg"
//		_ "github.com/heynemann/go-plg/guest/golang"
//	)
//
//	c := plg.New()
//	defer c.Close(ctx)
//
//	err := plg.Register[contract.Fooer](ctx, c, "fooer", plg.WithStorage(storage))
//	f, err := plg.Get[contract.Fooer](ctx, c, "fooer/my-fooer")
//	bar, err := f.Foo(ctx, "x")
//
// Every exported function and method is safe for concurrent use.
package plg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"go.opentelemetry.io/otel/metric"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
)

// closeTimeout bounds closing a generation's instances, in which each runs the plugin's optional Close: a Close
// that hangs cannot keep a retired generation's memory forever.
const closeTimeout = 10 * time.Second

// Container holds registered plugins and the WebAssembly runtimes that run them: one per distinct per-instance
// memory limit, all sharing the container's compilation cache. Create it with [New] and release it with
// [Container.Close].
type Container struct {
	engine       *engine.Engine
	logger       *slog.Logger
	panics       *panicReporter
	meters       *instruments
	observation  metric.Registration                 // the callback observing the pools' gauges
	runtimes     *xsync.Map[uint64, *engine.Runtime] // by memory limit
	modules      *xsync.Map[moduleKey, *compiledGuest]
	names        *xsync.Map[string, bool]          // registered (or registering) names
	regs         *xsync.Map[string, *registration] // published registrations, by name
	retired      *xsync.Map[*plugin, context.CancelFunc]
	catalogue    atomic.Pointer[snapshot]
	budget       memoryBudget
	closeTimeout time.Duration // closeTimeout; tests shorten it
	closed       atomic.Bool
	// genericBinder makes bind ignore the adapters plg gen generated (Contract.Fast), so tests and benchmarks
	// can hold the generic binder to the same suite.
	genericBinder atomic.Bool
}

// New returns an empty container.
func New(opts ...Option) *Container {
	cfg := containerOptions(opts)

	ctr := &Container{
		engine:       engine.New(context.Background(), engine.Options{Logger: cfg.logger, CacheDir: cfg.cacheDir}),
		logger:       cfg.logger,
		runtimes:     xsync.NewMap[uint64, *engine.Runtime](),
		modules:      xsync.NewMap[moduleKey, *compiledGuest](),
		names:        xsync.NewMap[string, bool](),
		regs:         xsync.NewMap[string, *registration](),
		retired:      xsync.NewMap[*plugin, context.CancelFunc](),
		budget:       memoryBudget{total: cfg.memoryBudget},
		closeTimeout: closeTimeout,
	}
	ctr.catalogue.Store(&snapshot{})
	ctr.instrument(&cfg)

	ctr.panics = newPanicReporter(cfg.logger, cfg.onPanic, cfg.panicHistory, ctr.meters)

	return ctr
}

// containerOptions applies the container options over their defaults.
func containerOptions(opts []Option) containerConfig {
	cfg := containerConfig{logger: slog.New(slog.DiscardHandler), panicHistory: defaultPanicHistory}
	for _, opt := range opts {
		opt(&cfg)
	}

	return cfg
}

// Close stops the pollers and serves no more calls: it waits on ctx for the calls in flight to finish, closes
// every instance (each running the plugin's optional Close), stops the refill goroutines and releases the
// runtimes. Calls still running when ctx ends are stopped. It returns the failures, those of closing the
// plugins included. Afterwards Get, reloads and calls through plugins obtained earlier fail with [ErrClosed].
// Closing a closed container does nothing.
func (c *Container) Close(ctx context.Context) error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	errs := []error{c.stopReloaders(ctx)}

	last := c.catalogue.Swap(&snapshot{closed: true})
	for _, gen := range last.plugins {
		c.retire(ctx, gen)
	}

	errs = append(errs, c.awaitRetired(ctx))

	for _, gen := range last.plugins {
		<-gen.closed // closed by now; receiving orders the read of closeErr after its write

		errs = append(errs, gen.closeErr)
	}

	for _, reg := range c.regs.All() {
		reg.http.Close()
	}

	for _, rt := range c.runtimes.All() {
		errs = append(errs, rt.Close(ctx))
	}

	errs = append(errs, c.engine.Close(ctx), c.observation.Unregister())

	err := errors.Join(errs...)
	if err != nil {
		return fmt.Errorf("plg: closing the container: %w", err)
	}

	return nil
}

// runtime returns the container's runtime for instances limited to limit bytes, creating it on first use.
func (c *Container) runtime(ctx context.Context, limit uint64) (*engine.Runtime, error) {
	rt, _ := c.runtimes.LoadOrCompute(limit, func() (*engine.Runtime, bool) {
		return c.engine.NewRuntime(ctx, engine.RuntimeOptions{MemoryLimit: limit}), false
	})

	// Close sets closed before closing the runtimes it finds: one created after that is closed here (closing
	// a runtime twice is harmless).
	if c.closed.Load() {
		return nil, errors.Join(ErrClosed, rt.Close(ctx))
	}

	return rt, nil
}

// moduleKey identifies a compiled guest module: one per guest (the language's linked guest, or a custom one by
// its digest) and runtime.
type moduleKey struct {
	lang  Language
	guest string // guestreg.Guest.ID
	limit uint64
}

// compiledGuest is a guest module compiled once per runtime, by the first Register that needs it. done is
// closed once mod or err is set.
type compiledGuest struct {
	mod  *engine.Module
	err  error
	done chan struct{}
}

// module returns the guest's module compiled for the runtime of instances limited to limit bytes, compiling it
// on first use. Concurrent callers wait for one compilation; a failed one is forgotten, so a later call
// retries. Every runtime compiles through the container's compilation cache, which holds the guest's machine
// code once: compiling it for another runtime only decodes the module again.
func (c *Container) module(ctx context.Context, lang Language, guest guestreg.Guest, limit uint64) (
	*engine.Module, error,
) {
	key := moduleKey{lang: lang, guest: guest.ID, limit: limit}

	for {
		mine := &compiledGuest{done: make(chan struct{})}

		entry, loaded := c.modules.LoadOrStore(key, mine)
		if !loaded {
			return c.compile(ctx, key, guest, mine)
		}

		select {
		case <-entry.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for the %s guest to compile: %w", lang, context.Cause(ctx))
		}

		if entry.err == nil {
			return entry.mod, nil
		}
	}
}

func (c *Container) compile(ctx context.Context, key moduleKey, guest guestreg.Guest, entry *compiledGuest) (
	*engine.Module, error,
) {
	defer close(entry.done)

	rt, err := c.runtime(ctx, key.limit)
	if err != nil {
		entry.err = err
	} else {
		entry.mod, entry.err = compileGuest(ctx, rt, key.lang, guest)
	}

	if entry.err != nil {
		c.modules.Delete(key)
	}

	return entry.mod, entry.err
}

// compileGuest compiles a guest for rt. It is named by its language, and a custom guest by its ID too, in the
// compile log and errors.
func compileGuest(ctx context.Context, rt *engine.Runtime, lang Language, guest guestreg.Guest) (
	*engine.Module, error,
) {
	name := string(lang)
	if guest.ID != "" {
		name += " " + guest.ID
	}

	wasm, err := guest.Wasm()
	if err == nil {
		var mod *engine.Module

		mod, err = rt.Compile(ctx, name, wasm)
		if err == nil {
			return mod, nil
		}
	}

	return nil, fmt.Errorf("loading the %s guest: %w", name, err)
}
