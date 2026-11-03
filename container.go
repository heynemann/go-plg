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

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
)

// goMemoryLimit is the default memory limit of each Go plugin instance; yaegi alone takes about 29 MiB.
const goMemoryLimit = 128 << 20

// Container holds registered plugins and the WebAssembly runtimes that run them: one per distinct per-instance
// memory limit, all sharing the container's compilation cache. Create it with [New] and release it with
// [Container.Close].
type Container struct {
	engine    *engine.Engine
	logger    *slog.Logger
	panics    *panicReporter
	runtimes  *xsync.Map[uint64, *engine.Runtime] // by memory limit
	modules   *xsync.Map[moduleKey, *compiledGuest]
	names     *xsync.Map[string, bool] // registered (or registering) names
	catalogue atomic.Pointer[snapshot]
	budget    memoryBudget
	closed    atomic.Bool
}

// New returns an empty container.
func New(opts ...Option) *Container {
	cfg := containerConfig{logger: slog.New(slog.DiscardHandler), panicHistory: defaultPanicHistory}
	for _, opt := range opts {
		opt(&cfg)
	}

	ctr := &Container{
		engine:   engine.New(context.Background(), engine.Options{Logger: cfg.logger, CacheDir: cfg.cacheDir}),
		logger:   cfg.logger,
		panics:   newPanicReporter(cfg.logger, cfg.onPanic, cfg.panicHistory),
		runtimes: xsync.NewMap[uint64, *engine.Runtime](),
		modules:  xsync.NewMap[moduleKey, *compiledGuest](),
		names:    xsync.NewMap[string, bool](),
		budget:   memoryBudget{total: cfg.memoryBudget},
	}
	ctr.catalogue.Store(&snapshot{})

	return ctr
}

// Close stops serving plugins and their refill goroutines, and releases the runtimes. It waits on ctx for calls
// in flight to finish; calls still running when ctx ends are stopped. Afterwards Get fails with [ErrClosed],
// and so do calls through plugins obtained earlier. Closing a closed container does nothing.
func (c *Container) Close(ctx context.Context) error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	last := c.catalogue.Swap(&snapshot{closed: true})

	var errs []error

	for _, plug := range last.plugins {
		errs = append(errs, plug.pool.close(ctx))
		plug.http.Close()
	}

	for _, rt := range c.runtimes.All() {
		errs = append(errs, rt.Close(ctx))
	}

	errs = append(errs, c.engine.Close(ctx))

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

// moduleKey identifies a compiled guest module: one per language and runtime.
type moduleKey struct {
	lang  Language
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
	key := moduleKey{lang: lang, limit: limit}

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

func compileGuest(ctx context.Context, rt *engine.Runtime, lang Language, guest guestreg.Guest) (
	*engine.Module, error,
) {
	wasm, err := guest.Wasm()
	if err == nil {
		var mod *engine.Module

		mod, err = rt.Compile(ctx, string(lang), wasm)
		if err == nil {
			return mod, nil
		}
	}

	return nil, fmt.Errorf("loading the %s guest: %w", lang, err)
}
