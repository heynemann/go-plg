package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
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
// preemptively when the context of its current call ends. Its guests may import the plg host module
// (http_roundtrip, host_call, take_result). It is safe for concurrent use.
type Runtime struct {
	rt     wazero.Runtime
	logger *slog.Logger
	hosts  *xsync.Map[string, *hostState] // by instance module name
	names  atomic.Uint64                  // instance name sequence
}

// NewRuntime returns a runtime sharing the engine's compilation cache, with WASI preview 1 and the plg host
// module available.
//
// Its guests may use wasm exception handling, wazero's experimental feature: the Lua guest compiles Lua's errors
// (setjmp/longjmp) to it. wazero v1.12 reads the feature only when decoding and validating a module, and neither
// the compiled code of modules without exception instructions nor the compilation cache key depends on it, so
// every runtime enables it and one runtime per memory limit still serves every guest.
func (e *Engine) NewRuntime(ctx context.Context, opts RuntimeOptions) *Runtime {
	cfg := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
		WithCompilationCache(e.cache).
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(limitPages(opts.MemoryLimit))

	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	instantiateWASI(ctx, rt)

	created := &Runtime{rt: rt, logger: e.logger, hosts: xsync.NewMap[string, *hostState]()}
	created.instantiateHostModule(ctx, rt)

	return created
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
	// HTTP serves the instance's HTTP requests; nil refuses them all.
	HTTP HTTPFunc
	// HTTPRefuses reports whether HTTP refuses a call's n-th request of size bytes whatever it holds (HTTP
	// disabled, a limit), without making it; nil means it refuses none so. During a multiplexed step, such a
	// request is refused at once (by HTTP, as during a call), without being copied: see [Request].
	HTTPRefuses func(n, size int) bool
	// Host serves the instance's calls to its Host; nil means the plugin has no Host.
	Host HostFunc
	// Mounts are the instance's only file system access.
	Mounts []Mount
}

// Instantiate creates an instance of the module and runs the guest's reactor initialization. Instances have
// clocks, sleeps that end with the context of the call in progress (see [pollOneoff]), a cryptographic random
// source, the given mounts, the argument list ["plugin"] and HTTP only through opts.HTTP; no environment and no
// sockets.
func (m *Module) Instantiate(ctx context.Context, opts *InstanceOptions) (*Instance, error) {
	fsCfg := wazero.NewFSConfig()
	for _, mnt := range opts.Mounts {
		fsCfg = fsCfg.WithFSMount(mnt.FS, mnt.GuestPath)
	}

	// The host imports find the instance's state by its module name, unique within the runtime.
	name := "plg-instance-" + strconv.FormatUint(m.rt.names.Add(1), 10)
	state := &hostState{http: opts.HTTP, refuses: opts.HTTPRefuses, host: opts.Host}
	m.rt.hosts.Store(name, state)

	cfg := wazero.NewModuleConfig().
		WithName(name).
		WithStartFunctions("_initialize").
		WithArgs("plugin").
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(rand.Reader).
		WithFSConfig(fsCfg).
		WithStdout(orDiscard(opts.Stdout)).
		WithStderr(orDiscard(opts.Stderr))

	mod, err := m.rt.rt.InstantiateModule(ctx, m.cm, cfg)
	if err != nil {
		m.rt.hosts.Delete(name)

		return nil, fmt.Errorf("instantiating guest %s: %w", m.name, err)
	}

	inst, err := newInstance(ctx, mod, state, m.rt.hosts)
	if err != nil {
		m.rt.hosts.Delete(name)

		return nil, err
	}

	return inst, nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}

	return w
}
