package plg

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"time"

	"github.com/heynemann/go-plg/internal/artifact"
	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/plgimpl"
)

// MuxGrace is how long, in all, a multiplexed call that already ended may run on.
const MuxGrace = muxGrace

// BundleMemoryLimit is the bundler's memory limit.
const BundleMemoryLimit = bundleMemoryLimit

// GlobalTracingChild is the environment variable set in the process TestTracingFollowsTheGlobalProvider runs
// itself in: TestMain then runs the tests without preparing the guests, which that test does not use.
const GlobalTracingChild = "PLG_TEST_GLOBAL_TRACING_CHILD"

// WithHTTPRoots makes the plugins' https requests trust roots instead of the system's certificate authorities, so
// tests can reach their own https servers.
func WithHTTPRoots(roots *x509.CertPool) RegisterOption {
	return func(c *registerConfig) {
		c.httpRoots = roots
	}
}

// RegisterBuiltWith is Register[T] in a program built with T's contract module at version: its plugins' pinned
// versions are checked against version, as a program whose build info records it would check them.
func RegisterBuiltWith[T any](ctx context.Context, ctr *Container, name, version string, opts ...RegisterOption) error {
	iface := reflect.TypeFor[T]()

	reg, err := ctr.prepare(iface, name, opts)
	if err != nil {
		return err
	}

	reg.module.Version, reg.module.Dir = version, ""

	return ctr.registerPrepared(ctx, iface, reg)
}

// Hold takes an instance of the plugin with the given key, as a call in flight would, until the returned
// release function is called. It lets tests make a plugin busy without a slow plugin method.
func Hold(ctx context.Context, c *Container, key string) (func(), error) {
	held, err := HoldInstance(ctx, c, key)

	return held.Release, err
}

// Held is an instance taken by [HoldInstance]. Call exactly one of Release and Discard.
type Held struct {
	Release func() // gives the instance back
	Discard func() // closes the instance as a broken one; the pool's refiller replaces it
	ID      uint64 // instances of a plugin are numbered from 1 in the order they started
}

// HoldInstance is [Hold], also returning the identity of the instance taken. Like a call, it counts as in
// flight on the generation of the plugin it holds until it is released or discarded.
func HoldInstance(ctx context.Context, c *Container, key string) (Held, error) {
	gen, err := c.serving(key)
	if err != nil {
		return Held{}, err
	}

	held, err := gen.pool.acquire(ctx)
	if err != nil {
		gen.leave()

		return Held{}, err
	}

	return Held{
		ID: held.id,
		Release: func() {
			gen.pool.release(context.WithoutCancel(ctx), held)
			gen.leave()
		},
		Discard: func() {
			gen.pool.discard(context.WithoutCancel(ctx), held)
			gen.leave()
		},
	}, nil
}

// CallRaw calls the method with the given index of an instance of the plugin with the given key, the arguments
// already encoded as args, then discards the instance: a test sends what no adapter would.
func CallRaw(ctx context.Context, c *Container, key string, method uint32, args []byte) (engine.Result, error) {
	gen, err := c.serving(key)
	if err != nil {
		return engine.Result{}, err
	}

	held, err := gen.pool.acquire(ctx)
	if err != nil {
		gen.leave()

		return engine.Result{}, err
	}

	defer func() {
		gen.pool.discard(context.WithoutCancel(ctx), held)
		gen.leave()
	}()

	return held.inst.Call(ctx, method, args) //nolint:wrapcheck // the test reads the engine's own error
}

// MemoryInUse returns the memory limits of every instance alive in the container, added up.
func MemoryInUse(c *Container) uint64 {
	return c.budget.used.Load()
}

// MemoryReserved returns the worst cases of every plugin version the container holds, added up.
func MemoryReserved(c *Container) uint64 {
	return c.budget.reserved.Load()
}

// Instances returns how many plugin instances the container's runtimes hold open: instantiated and their
// module not closed, whoever holds them (a pool, a start in progress, or nobody).
func Instances(c *Container) int {
	open := 0

	for _, rt := range c.runtimes.All() {
		open += rt.Instances()
	}

	return open
}

// Generation identifies the version of the plugin c serves under key; nil when there is none.
func Generation(c *Container, key string) any {
	gen, ok := c.catalogue.Load().plugins[key]
	if !ok {
		return nil
	}

	return gen
}

// LoadedVersion opens the plugin desc of store and returns the version Register gives it when desc has none:
// the content hash of the snapshot it loads.
func LoadedVersion(ctx context.Context, store Storage, desc *Descriptor) (string, error) {
	files, err := store.Open(ctx, desc)
	if err != nil {
		return "", fmt.Errorf("opening: %w", err)
	}

	frozen, err := snapshotFiles(ctx, files)
	if err != nil {
		return "", err
	}

	return artifact.Digest(frozen), nil
}

// OpenSwapping opens the plugin directory desc names as s.Open does, calling beforeOpen with the
// plugin-relative name of each directory and file right before the copy opens it, once the walk listed them.
func OpenSwapping(ctx context.Context, s *FSStorage, desc *Descriptor, beforeOpen func(name string)) (fs.FS, error) {
	return s.copyPlugin(ctx, desc, openHooks{beforeCopy: beforeOpen})
}

// WalkSwapping opens the plugin directory desc names as s.Open does, calling beforeOpen with the plugin-relative
// name of each directory right before the walk of its files opens it, once its parent's walk listed it.
func WalkSwapping(ctx context.Context, s *FSStorage, desc *Descriptor, beforeOpen func(name string)) (fs.FS, error) {
	return s.copyPlugin(ctx, desc, openHooks{beforeWalk: beforeOpen})
}

// ListSwapping lists s as s.List does, calling beforeOpen with each name of the storage's directory right before
// List opens it (a plugin.yaml once List stated it).
func ListSwapping(ctx context.Context, s *FSStorage, beforeOpen func(name string)) ([]Descriptor, error) {
	return s.listCalling(ctx, beforeOpen)
}

// Retired returns how many replaced or removed plugin versions are not closed yet.
func Retired(c *Container) int {
	return c.retired.Size()
}

// AwaitRetired waits on ctx until every replaced or removed plugin version is closed.
func AwaitRetired(ctx context.Context, c *Container) error {
	for gen := range c.retired.All() {
		select {
		case <-gen.closed:
		case <-ctx.Done():
			return fmt.Errorf("awaiting retired versions: %w", context.Cause(ctx))
		}
	}

	return nil
}

// SetCloseTimeout replaces the time a retired version's instances get to run their Close.
func SetCloseTimeout(c *Container, d time.Duration) {
	c.closeTimeout = d
}

// EventsStopped returns a channel closed once the goroutine delivering c's WithOnEvent events has returned.
func EventsStopped(c *Container) <-chan struct{} {
	return c.events.done
}

// SetBundleLimits replaces the base of a bundling's time limit and the bundler's memory limit. Call it while no
// load or reload of the container runs: they read the limits without synchronization.
func SetBundleLimits(c *Container, timeBase time.Duration, memory uint64) {
	c.bundleTimeBase, c.bundleMemory = timeBase, memory
}

// Bundle bundles the TypeScript plugin whose entrypoint is entrypoint inside files as a load does, in the
// container's bundler.
func Bundle(ctx context.Context, ctr *Container, files fs.FS, entrypoint string) (string, error) {
	guest, ok := guestreg.Lookup(string(LanguageTypeScript))
	if !ok {
		return "", ErrGuestNotLinked
	}

	snapshot, err := snapshotFiles(ctx, files)
	if err != nil {
		return "", err
	}

	return ctr.bundle(ctx, guest, snapshot, entrypoint)
}

// CallsInFlight returns how many calls the multiplexed instance of the plugin with the given key has in flight,
// as its owner goroutine counts them once it has run every operation handed to it before. It takes one of the
// instance's lanes meanwhile.
func CallsInFlight(ctx context.Context, c *Container, key string) (int, error) {
	var n int

	err := onMux(ctx, c, key, func(m *muxInstance) { n = len(m.flight) })

	return n, err
}

// RestartCallNumbers makes the multiplexed instance of the plugin with the given key number its next calls from 1
// again (skipping those in flight), so that they take the numbers of calls it ended. It takes one of the
// instance's lanes meanwhile.
func RestartCallNumbers(ctx context.Context, c *Container, key string) error {
	return onMux(ctx, c, key, func(m *muxInstance) { m.last = 0 })
}

// errNotMultiplexed reports a plugin whose instances serve one call at a time.
var errNotMultiplexed = errors.New("its instances serve one call at a time")

// onMux runs operation on the owner goroutine of the multiplexed instance of the plugin with the given key,
// through one of its lanes.
func onMux(ctx context.Context, c *Container, key string, operation func(m *muxInstance)) error {
	gen, err := c.serving(key)
	if err != nil {
		return err
	}

	defer gen.leave()

	held, err := gen.pool.acquire(ctx)
	if err != nil {
		return err
	}

	defer gen.pool.release(context.WithoutCancel(ctx), held)

	lane, ok := held.inst.(*muxLane)
	if !ok {
		return fmt.Errorf("plugin %s: %w", key, errNotMultiplexed)
	}

	done := make(chan struct{})

	lane.m.ops <- func() {
		operation(lane.m)
		close(done)
	}

	<-done

	return nil
}

// HostHandler returns the host_call handler Register gives the instances of the Go plugin key when it registers
// iface with the WithHost option opt in the container ctr, which reports its panics.
func HostHandler(iface reflect.Type, opt RegisterOption, key string, ctr *Container) (engine.HostFunc, error) {
	contr, err := lookupContract(iface)
	if err != nil {
		return nil, err
	}

	cfg := &registerConfig{}
	opt(cfg)

	hst, err := newHost(contr, cfg.hosts)
	if err != nil {
		return nil, err
	}

	return hst.handler(pluginID{owner: noOwner, key: key, lang: LanguageGo}, ctr.panics, nil), nil
}

// UseGenericBinder makes c serve every contract with the generic binder, ignoring the fast paths plg gen
// generated, so tests and benchmarks can hold both binders to the same behavior. Call it before Register.
func UseGenericBinder(c *Container) {
	c.genericBinder.Store(true)
}

// UnguardLogger makes c log every record straight to its logger, without the callback boundary, or behind it
// again with unguarded false, so that a panicking handler escapes where c logs as it did before logging was
// guarded: tests use it to prove that such a panic leaves nothing behind.
func UnguardLogger(c *Container, unguarded bool) {
	c.panics.unguarded.Store(unguarded)
}

// FastAdapter returns the adapter plg gen generated for iface's registered contract, calling inv, or nil when
// the contract has none.
func FastAdapter(iface reflect.Type, inv plgimpl.Invoker) any {
	ct, err := lookupContract(iface)
	if err != nil || ct.fast == nil {
		return nil
	}

	return ct.fast(inv)
}

// GenericCall is the generic binder's call of iface's method index with args (ctx first).
//
//nolint:ireturn // binders hand calls around as plgimpl.Call
func GenericCall(iface reflect.Type, index int, args []reflect.Value) (plgimpl.Call, error) {
	ct, err := lookupContract(iface)
	if err != nil {
		return nil, err
	}

	return &reflectCall{meth: ct.methods[index], args: args}, nil
}

// GenericResults returns the results a call from GenericCall decoded, ending with a nil error.
func GenericResults(call plgimpl.Call) []reflect.Value {
	generic, ok := call.(*reflectCall)
	if !ok {
		return nil
	}

	return generic.out
}

// DecodeAnswer decodes the results of a plugin's successful answer body into call, as every binder does.
func DecodeAnswer(body []byte, call plgimpl.Call) error {
	return decodeResults(body, call)
}
