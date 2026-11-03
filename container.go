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

// goMemoryLimit caps the memory of each Go plugin instance; yaegi alone takes about 29 MiB.
const goMemoryLimit = 128 << 20

// Container holds registered plugins and the WebAssembly runtime that runs them. Create it with [New] and
// release it with [Container.Close].
type Container struct {
	engine    *engine.Engine
	runtime   *engine.Runtime
	logger    *slog.Logger
	modules   *xsync.Map[Language, *compiledGuest]
	names     *xsync.Map[string, bool] // registered (or registering) names
	catalogue atomic.Pointer[snapshot]
	closed    atomic.Bool
}

// New returns an empty container.
func New(opts ...Option) *Container {
	cfg := containerConfig{logger: slog.New(slog.DiscardHandler)}
	for _, opt := range opts {
		opt(&cfg)
	}

	ctx := context.Background()
	eng := engine.New(ctx, engine.Options{Logger: cfg.logger, CacheDir: cfg.cacheDir})

	ctr := &Container{
		engine:  eng,
		runtime: eng.NewRuntime(ctx, engine.RuntimeOptions{MemoryLimit: goMemoryLimit}),
		logger:  cfg.logger,
		modules: xsync.NewMap[Language, *compiledGuest](),
		names:   xsync.NewMap[string, bool](),
	}
	ctr.catalogue.Store(&snapshot{})

	return ctr
}

// Close stops serving plugins and releases the runtime. It waits on ctx for calls in flight to finish; calls
// still running when ctx ends are stopped. Afterwards Get fails with [ErrClosed], and so do calls through
// plugins obtained earlier. Closing a closed container does nothing.
func (c *Container) Close(ctx context.Context) error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	last := c.catalogue.Swap(&snapshot{closed: true})

	var errs []error

	for _, plug := range last.plugins {
		errs = append(errs, plug.pool.close(ctx))
	}

	errs = append(errs, c.runtime.Close(ctx), c.engine.Close(ctx))

	err := errors.Join(errs...)
	if err != nil {
		return fmt.Errorf("plg: closing the container: %w", err)
	}

	return nil
}

// compiledGuest is a guest module compiled once per container, by the first Register that needs it. done is
// closed once mod or err is set.
type compiledGuest struct {
	mod  *engine.Module
	err  error
	done chan struct{}
}

// module returns the container's compiled module of guest, compiling it on first use. Concurrent callers wait
// for one compilation; a failed one is forgotten, so a later call retries.
func (c *Container) module(ctx context.Context, lang Language, guest guestreg.Guest) (*engine.Module, error) {
	for {
		mine := &compiledGuest{done: make(chan struct{})}

		entry, loaded := c.modules.LoadOrStore(lang, mine)
		if !loaded {
			return c.compile(ctx, lang, guest, mine)
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

func (c *Container) compile(ctx context.Context, lang Language, guest guestreg.Guest, entry *compiledGuest) (
	*engine.Module, error,
) {
	defer close(entry.done)

	wasm, err := guest.Wasm()
	if err == nil {
		entry.mod, err = c.runtime.Compile(ctx, string(lang), wasm)
	}

	if err != nil {
		entry.err = fmt.Errorf("loading the %s guest: %w", lang, err)
		c.modules.Delete(lang)
	}

	return entry.mod, entry.err
}
