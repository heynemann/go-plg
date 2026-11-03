package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// PageSize is the size of a wasm memory page.
const PageSize = 64 << 10

// maxPages is the wasm32 memory maximum (4 GiB).
const maxPages = 1 << 16

// RuntimeOptions configure a [Runtime].
type RuntimeOptions struct {
	// MemoryLimit caps each instance's linear memory, in bytes, rounded down to whole pages (at least one).
	// Zero means the wasm32 maximum of 4 GiB.
	MemoryLimit uint64
}

// Runtime is a wazero runtime: every instance it creates has the same memory limit and is stopped
// preemptively when the context of its current call ends. It is safe for concurrent use.
type Runtime struct {
	rt     wazero.Runtime
	logger *slog.Logger
}

// NewRuntime returns a runtime sharing the engine's compilation cache, with WASI preview 1 available.
func (e *Engine) NewRuntime(ctx context.Context, opts RuntimeOptions) *Runtime {
	cfg := wazero.NewRuntimeConfig().
		WithCompilationCache(e.cache).
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(limitPages(opts.MemoryLimit))

	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt) // cannot fail on a fresh runtime

	return &Runtime{rt: rt, logger: e.logger}
}

func limitPages(limit uint64) uint32 {
	if limit == 0 || limit/PageSize >= maxPages {
		return maxPages
	}

	return uint32(max(limit/PageSize, 1))
}

// Close closes the runtime and every instance it created.
func (r *Runtime) Close(ctx context.Context) error {
	err := r.rt.Close(ctx)
	if err != nil {
		return fmt.Errorf("closing runtime: %w", err)
	}

	return nil
}

// Module is a compiled guest module.
type Module struct {
	rt   *Runtime
	cm   wazero.CompiledModule
	name string
}

// Compile compiles wasm (through the compilation cache) and logs how long it took.
func (r *Runtime) Compile(ctx context.Context, name string, wasm []byte) (*Module, error) {
	start := time.Now()

	cm, err := r.rt.CompileModule(ctx, wasm)
	if err != nil {
		return nil, fmt.Errorf("compiling guest %s: %w", name, err)
	}

	r.logger.LogAttrs(ctx, slog.LevelInfo, "plg: guest compiled",
		slog.String("guest", name), slog.Duration("duration", time.Since(start)))

	return &Module{rt: r, cm: cm, name: name}, nil
}

// Mount is a read-only file tree visible to an instance at GuestPath.
type Mount struct {
	FS        fs.FS
	GuestPath string
}

// InstanceOptions configure one instance.
type InstanceOptions struct {
	// Stdout and Stderr receive the guest's output; nil discards it.
	Stdout, Stderr io.Writer
	// Mounts are the instance's only file system access.
	Mounts []Mount
}

// Instantiate creates an instance of the module and runs the guest's reactor initialization. Instances have
// clocks, sleep, a cryptographic random source, the given mounts and the argument list ["plugin"]; no
// environment and no sockets.
func (m *Module) Instantiate(ctx context.Context, opts InstanceOptions) (*Instance, error) {
	fsCfg := wazero.NewFSConfig()
	for _, mnt := range opts.Mounts {
		fsCfg = fsCfg.WithFSMount(mnt.FS, mnt.GuestPath)
	}

	cfg := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize").
		WithArgs("plugin").
		WithSysWalltime().
		WithSysNanotime().
		WithSysNanosleep().
		WithRandSource(rand.Reader).
		WithFSConfig(fsCfg).
		WithStdout(orDiscard(opts.Stdout)).
		WithStderr(orDiscard(opts.Stderr))

	mod, err := m.rt.rt.InstantiateModule(ctx, m.cm, cfg)
	if err != nil {
		return nil, fmt.Errorf("instantiating guest %s: %w", m.name, err)
	}

	return newInstance(ctx, mod)
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}

	return w
}
