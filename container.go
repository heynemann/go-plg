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
	engine *engine.Engine
	// logger is the user's logger behind the callback boundary ([panicReporter.guardedLogger]), for the records
	// the container logs itself; a plugin version's instances and pool log through guarded loggers of their own.
	logger       *slog.Logger
	panics       *panicReporter
	events       *eventQueue // the WithOnEvent function's; nil without one
	meters       *instruments
	observation  metric.Registration                 // the callback observing the pools' gauges
	runtimes     *xsync.Map[uint64, *engine.Runtime] // by memory limit
	modules      *xsync.Map[moduleKey, *compiledGuest]
	names        *xsync.Map[string, *registration] // registered (or registering) registrations, by name
	regs         *xsync.Map[string, *registration] // published registrations, by name
	retired      *xsync.Map[*plugin, context.CancelFunc]
	catalogue    atomic.Pointer[snapshot]
	budget       memoryBudget
	closeTimeout time.Duration // closeTimeout; tests shorten it
	// bundleTimeBase is the base of a bundling's time limit (bundleTimeBase) and bundleMemory the bundler's
	// memory limit (bundleMemoryLimit); tests change them.
	bundleTimeBase time.Duration
	bundleMemory   uint64
	closed         atomic.Bool
	// genericBinder makes bind ignore the adapters plg gen generated (Contract.Fast), so tests and benchmarks
	// can hold the generic binder to the same suite.
	genericBinder atomic.Bool
}

// New returns an empty container.
func New(opts ...Option) *Container {
	cfg := containerOptions(opts)

	ctr := &Container{
		panics: newPanicReporter(
			cfg.logger,
			cfg.onPanic,
			cfg.panicHistory,
			nil,
		), // instrument gives it the meters
		runtimes:     xsync.NewMap[uint64, *engine.Runtime](),
		modules:      xsync.NewMap[moduleKey, *compiledGuest](),
		names:        xsync.NewMap[string, *registration](),
		regs:         xsync.NewMap[string, *registration](),
		retired:      xsync.NewMap[*plugin, context.CancelFunc](),
		budget:       memoryBudget{total: cfg.memoryBudget},
		closeTimeout: closeTimeout,
		// The bundler's limits, see typescript.go.
		bundleTimeBase: bundleTimeBase,
		bundleMemory:   bundleMemoryLimit,
	}
	ctr.catalogue.Store(&snapshot{})

	ctr.logger = ctr.panics.guardedLogger("", "")
	ctr.panics.tally = ctr.countPanic
	ctr.instrument(&cfg)

	// After the instruments: a panic in the logger's handler on the engine's warning is counted in them.
	ctr.engine = engine.New(context.Background(), engine.Options{Logger: ctr.logger, CacheDir: cfg.cacheDir})

	ctr.events = newEventQueue(cfg.onEvent, ctr.panics, ctr.logger)
	ctr.panics.events = ctr.events

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
// runtimes and the compiled guests, so a closed container that stays referenced keeps almost no memory. Calls
// still running when ctx ends are stopped, wherever they wait: in the guest, a sleep, an HTTP request or a Host
// method (whose context ends); each fails with [ErrClosed], and so does a Create in progress. A runtime with such
// a call is released once the last of them has left the guest, which may be after Close returns. It returns the
// failures, those of closing the plugins included. Last, it delivers the queued [WithOnEvent] events, the
// retired versions' included, while ctx lasts, and stops delivering.
// Afterwards Get, reloads and calls through plugins obtained earlier fail with [ErrClosed]. Closing a closed
// container does nothing.
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

	// Forgetting the compiled guests frees them, decoded modules included (about 30 MiB for the Go guest), even
	// while the closed container stays referenced; closing the compilation cache frees their machine code.
	c.modules.Clear()

	errs = append(errs, c.engine.Close(ctx), c.unobserve(ctx), c.events.close(ctx))

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

// errCompileUnwound is a compilation a panic unwound before it had a module or an error.
var errCompileUnwound = errors.New("the guest's compilation was interrupted")

// compile compiles the guest for the runtime of key into entry, the waiting callers' to read once done is closed.
// entry is complete however compile ends: a compilation a panic unwinds (the engine logs the compile) is a
// failure, forgotten like any other, so that a later call compiles the guest anew.
func (c *Container) compile(ctx context.Context, key moduleKey, guest guestreg.Guest, entry *compiledGuest) (
	*engine.Module, error,
) {
	defer func() {
		if entry.mod == nil && entry.err == nil {
			entry.err = errCompileUnwound
		}

		if entry.err != nil {
			c.modules.Delete(key)
		}

		close(entry.done)
	}()

	rt, err := c.runtime(ctx, key.limit)
	if err != nil {
		entry.err = err
	} else {
		entry.mod, entry.err = compileGuest(ctx, rt, key.lang, guest)
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
