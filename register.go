package plg

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/internal/httpcap"
)

// Register loads, under name, every plugin its storages list for the interface T, and publishes them in one
// step: each plugin's key is "<name>/<descriptor key>". It loads synchronously and fails fast: on any error
// nothing is published. Plugins in a language the language options ([WithGolangPlugins], …) leave out are
// skipped and logged. Afterwards the registration follows its storages on [Container.Reload],
// [ReloadInterface] and, with [WithPollInterval], by itself.
//
// It fails if T breaks the interface contract (every method takes a context.Context first, returns an error
// last and uses only data types) with [ErrInvalidInterface], if T has no [Contract] with [ErrNoContract], if
// name is already registered with [ErrDuplicateName], if an option is invalid with [ErrInvalidOption], if a
// plugin's language guest is not linked into the program with [ErrGuestNotLinked], if the plugins' worst-case
// memory does not fit the container's budget with [ErrMemoryBudget], and if a plugin fails to load (the Create
// of any of its first instances included) with [ErrLoad].
func Register[T any](ctx context.Context, c *Container, name string, opts ...RegisterOption) error {
	return c.register(ctx, reflect.TypeFor[T](), name, opts)
}

func (c *Container) register(ctx context.Context, iface reflect.Type, name string, opts []RegisterOption) error {
	reg, err := c.prepare(iface, name, opts)
	if err != nil {
		return err
	}

	if _, dup := c.names.LoadOrStore(name, true); dup {
		reg.http.Close()

		return fmt.Errorf("%w: %s", ErrDuplicateName, name)
	}

	plugins, err := c.loadAll(ctx, reg)
	if err == nil {
		err = c.publish(iface, plugins, nil)
		if err != nil {
			c.closeUnpublished(ctx, plugins)
		}
	}

	if err != nil {
		c.names.Delete(name)
		reg.http.Close()

		return err
	}

	c.serve(ctx, reg)

	return nil
}

// registration is a checked Register call.
type registration struct {
	contr      *contract
	cfg        *registerConfig
	http       *httpcap.Client // enforces the call's HTTP policy; shared by its plugins
	host       *host           // the WithHost option's; nil without one
	reloads    *reloader       // set once the registration is published
	name       string
	dispatcher []byte // the Go dispatcher: the contract's, or the one rendered with the Host's stubs
	goPayload  []byte // the Go init payload, likewise
	schema     []byte // the script guests' wire.Schema: the interface's methods, then the Host's
}

// prepare checks a Register call's arguments and returns the interface's contract, the Register options (with
// their defaults), the HTTP client of its policy and its Host.
func (c *Container) prepare(iface reflect.Type, name string, opts []RegisterOption) (*registration, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}

	if name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("%w %q: names are non-empty and have no slash", ErrInvalidName, name)
	}

	err := validateInterface(iface)
	if err != nil {
		return nil, err
	}

	contr, err := lookupContract(iface)
	if err != nil {
		return nil, err
	}

	cfg, err := registerOptions(name, opts)
	if err != nil {
		return nil, err
	}

	hst, err := newHost(contr, cfg.hosts)
	if err != nil {
		return nil, err
	}

	client, err := httpClient(cfg.http, c.logger)
	if err != nil {
		return nil, err
	}

	reg := &registration{contr: contr, cfg: cfg, http: client, host: hst, name: name}
	reg.dispatcher, reg.goPayload = contr.dispatcher, contr.payload

	var hostMethods []*method

	if hst != nil {
		reg.dispatcher, reg.goPayload, hostMethods = hst.dispatcher, hst.payload, hst.methods
	}

	reg.schema = scriptSchema(contr.methods, hostMethods)

	return reg, nil
}

// source is a plugin a storage listed.
type source struct {
	storage   Storage
	loader    loader
	guest     guestreg.Guest
	storeName string // storage.Name(), asked once
	desc      Descriptor
	limit     uint64 // per-instance memory limit, whole pages
}

// problem is a listed plugin that cannot be loaded: its descriptor is invalid, its key is listed twice or its
// language's guest is missing. key is its descriptor key, empty when it has none.
type problem struct {
	err error
	key string
}

// listPlugins lists the storages for Register, which fails on any problem: a storage that cannot be listed, an
// invalid descriptor, a key listed twice, a language whose guest is not linked. Plugins in languages the
// Register did not enable are skipped and logged.
func (c *Container) listPlugins(ctx context.Context, name string, cfg *registerConfig) ([]source, error) {
	listed, problems, err := c.listStorages(ctx, name, cfg.storages)
	if err == nil && len(problems) > 0 {
		err = problems[0].err
	}

	if err != nil {
		return nil, err
	}

	srcs := listed[:0]

	for i := range listed {
		ok, err := c.admit(ctx, name, cfg, &listed[i], slog.LevelInfo)
		if err != nil {
			return nil, err
		}

		if ok {
			srcs = append(srcs, listed[i])
		}
	}

	return srcs, nil
}

// relistPlugins lists the storages for a reload: it returns the plugins to load and the problems of the others,
// and fails only when a storage cannot be listed. Skipped languages are logged at Debug level only, rather than
// on every poll.
func (c *Container) relistPlugins(ctx context.Context, name string, cfg *registerConfig) (
	[]source, []problem, error,
) {
	listed, problems, err := c.listStorages(ctx, name, cfg.storages)
	if err != nil {
		return nil, nil, err
	}

	srcs := listed[:0]

	for i := range listed {
		ok, err := c.admit(ctx, name, cfg, &listed[i], slog.LevelDebug)

		switch {
		case err != nil:
			problems = append(problems, problem{key: listed[i].desc.Key, err: err})
		case ok:
			srcs = append(srcs, listed[i])
		default: // in a language the Register did not enable
		}
	}

	return srcs, problems, nil
}

// admit prepares a listed plugin for loading. It reports false for a plugin in a language the Register did not
// enable, which it logs at level, and fails when the language's guest is not linked.
func (c *Container) admit(ctx context.Context, name string, cfg *registerConfig, src *source, level slog.Level) (
	bool, error,
) {
	if !cfg.enabled(src.desc.Language) {
		key := name + "/" + src.desc.Key
		c.logger.LogAttrs(ctx, level, "plg: plugin skipped: its language is not enabled",
			slog.String("key", key), slog.String("storage", src.storeName),
			slog.String("language", string(src.desc.Language)))
		c.countSkipped(ctx, key, src.desc.Language)

		return false, nil
	}

	ld, guest, err := cfg.guestFor(src.desc.Language)
	if err != nil {
		return false, fmt.Errorf("plugin %s (storage %s): %w", src.desc.Key, src.storeName, err)
	}

	src.loader, src.guest, src.limit = ld, guest, instanceLimit(cfg.limit(ld), &src.desc)

	return true, nil
}

// instanceLimit returns a plugin's per-instance memory limit: the Register's for its language, lowered by the
// descriptor's. A descriptor asking for more than the Register allows gets the Register's limit
// ([Container.launch] warns).
func instanceLimit(limit uint64, desc *Descriptor) uint64 {
	if desc.MemoryLimit == 0 || desc.MemoryLimit > limit {
		return limit
	}

	return wholePages(desc.MemoryLimit)
}

// listStorages lists every storage, checking that each descriptor has a key and an entrypoint and that no key
// is listed twice, whatever its language. A key with a problem is not loaded from any storage. A panicking
// storage fails with a reported [PanicError].
func (c *Container) listStorages(ctx context.Context, name string, storages []Storage) (
	[]source, []problem, error,
) {
	var (
		srcs     []source
		problems []problem
	)

	owner := map[string]string{} // key -> storage name

	for _, store := range storages {
		storeName, descs, err := c.listStorage(ctx, name, store)
		if err != nil {
			return nil, nil, err
		}

		for _, desc := range descs {
			err = checkDescriptor(storeName, &desc)
			if err == nil {
				if prev, dup := owner[desc.Key]; dup {
					err = fmt.Errorf("%w %q: listed by storages %s and %s", ErrDuplicateKey, desc.Key, prev,
						storeName)
				}
			}

			if err != nil {
				problems = append(problems, problem{key: desc.Key, err: err})

				continue
			}

			owner[desc.Key] = storeName
			srcs = append(srcs, source{storage: store, storeName: storeName, desc: desc})
		}
	}

	srcs = slices.DeleteFunc(srcs, func(src source) bool {
		return slices.ContainsFunc(problems, func(p problem) bool { return p.key == src.desc.Key })
	})

	return srcs, problems, nil
}

// listStorage asks a storage for its name and its descriptors; a panicking storage fails with a reported
// [PanicError].
func (c *Container) listStorage(ctx context.Context, name string, store Storage) (string, []Descriptor, error) {
	var (
		storeName string
		descs     []Descriptor
	)

	err := c.storageCall(ctx, name, "Name", func() error {
		storeName = store.Name()

		return nil
	})
	if err == nil {
		err = c.storageCall(ctx, name, "List", func() error {
			var listErr error

			descs, listErr = store.List(ctx)

			return listErr //nolint:wrapcheck // wrapped below, with the storage's name
		})
	}

	if err != nil {
		return "", nil, fmt.Errorf("%w: listing storage %s: %w", ErrLoad, storeName, err)
	}

	return storeName, descs, nil
}

// storageCall runs a method of a user's [Storage], turning a panic into a reported [PanicError] in
// [PhaseStorage] for the given key.
func (c *Container) storageCall(ctx context.Context, key, method string, call func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			failure := &PanicError{Key: key, Phase: PhaseStorage, Method: method}
			c.panics.recovered(ctx, failure, v)

			err = failure
		}
	}()

	return call()
}

// checkDescriptor checks that a descriptor has a key and an entrypoint, and a memory limit from one page to
// 4 GiB if any.
func checkDescriptor(storeName string, desc *Descriptor) error {
	if desc.Key == "" || desc.Entrypoint == "" {
		return fmt.Errorf("%w: storage %s lists %+v without a key or an entrypoint", ErrInvalidDescriptor,
			storeName, *desc)
	}

	if desc.MemoryLimit != 0 && !validMemoryLimit(desc.MemoryLimit) {
		return fmt.Errorf("%w: storage %s lists %q with a memory limit of %d bytes, not from one %d-byte page "+
			"to 4 GiB", ErrInvalidDescriptor, storeName, desc.Key, desc.MemoryLimit, minMemoryLimit)
	}

	return nil
}

// loadAll reserves the listed plugins' worst-case memory in the container's budget and loads them; on failure
// it closes the ones already loaded and gives the reservation back.
func (c *Container) loadAll(ctx context.Context, reg *registration) ([]*plugin, error) {
	srcs, err := c.listPlugins(ctx, reg.name, reg.cfg)
	if err != nil {
		return nil, err
	}

	reserved, err := c.reserveMemory(reg.name, reg.cfg, srcs)
	if err != nil {
		return nil, err
	}

	plugins := make([]*plugin, 0, len(srcs))

	for i := range srcs {
		key := reg.name + "/" + srcs[i].desc.Key

		plug, err := c.load(ctx, reg, key, &srcs[i])
		if err != nil {
			c.countLoad(ctx, key, srcs[i].desc.Language, err)
			c.closeUnpublished(ctx, plugins)
			c.budget.unreserve(reserved)

			return nil, fmt.Errorf("%w: %s (storage %s): %w", ErrLoad, key, srcs[i].storeName, err)
		}

		plug.value = reg.contr.bind(c, key) //nolint:contextcheck // each adapter call takes the context it is passed
		plug.reserved = worstCase(reg.cfg, &srcs[i])
		reserved -= plug.reserved
		plugins = append(plugins, plug) // the Register's to close from now on

		c.countLoad(ctx, key, srcs[i].desc.Language, nil)
	}

	return plugins, nil
}

// worstCase is the most memory a plugin's instances may take: its pool maximum × its memory limit.
func worstCase(cfg *registerConfig, src *source) uint64 {
	return uint64(cfg.poolMax) * src.limit //nolint:gosec // G115: settle validated 1 <= poolMax <= maxPoolSize
}

// reserveMemory reserves the worst case of every plugin in the container's budget, and returns the bytes
// reserved.
func (c *Container) reserveMemory(name string, cfg *registerConfig, srcs []source) (uint64, error) {
	var reserved, needed uint64

	for i := range srcs {
		needed += worstCase(cfg, &srcs[i])
	}

	for i := range srcs {
		worst := worstCase(cfg, &srcs[i])
		if !c.budget.reserve(worst) {
			c.budget.unreserve(reserved)

			return 0, fmt.Errorf("%w: %s needs up to %d bytes (a pool of up to %d instances per plugin), "+
				"and %d of the %d-byte budget are reserved by other plugins", ErrMemoryBudget, name, needed,
				cfg.poolMax, c.budget.reserved.Load(), c.budget.total)
		}

		reserved += worst
	}

	return reserved, nil
}

// load opens a listed plugin and launches it. A panic on the host meanwhile (the plugin's fs.FS is the
// storage's code) fails with a reported [PanicError] in [PhaseLoad].
func (c *Container) load(ctx context.Context, reg *registration, key string, src *source) (*plugin, error) {
	var plug *plugin

	err := c.loadStep(ctx, key, src, func() error {
		files, err := c.open(ctx, key, src)
		if err == nil {
			plug, err = c.launch(ctx, reg, key, src, files)
		}

		return err
	})

	return plug, err
}

// loadStep runs a step of loading the plugin key, turning a panic into a reported [PanicError] in [PhaseLoad].
func (c *Container) loadStep(ctx context.Context, key string, src *source, step func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			failure := &PanicError{Key: key, Language: src.desc.Language, Phase: PhaseLoad}
			c.panics.recovered(ctx, failure, v)

			err = failure
		}
	}()

	return step()
}

// open opens a listed plugin's files, copies them into the snapshot it returns (see [Storage]) and resolves its
// version: a content hash of the snapshot when the storage gives none. Hashing, bundling and loading all read
// that one snapshot.
func (c *Container) open(ctx context.Context, key string, src *source) (fs.FS, error) {
	var opened fs.FS

	err := c.storageCall(ctx, key, "Open", func() error {
		var openErr error

		opened, openErr = src.storage.Open(ctx, src.desc)

		return openErr //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	files, err := snapshotFiles(ctx, opened)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	if src.desc.Version == "" {
		src.desc.Version, err = contentVersion(files, ".")
		if err != nil {
			return nil, err
		}
	}

	return files, nil
}

// launch starts the minimum instances of an opened plugin's pool (running Create in each) and returns the
// generation they serve. Its adapter and its memory reservation are the caller's to set. A panic once the
// instances started (the user's logger) closes them before going on: nothing else owns them yet.
func (c *Container) launch(ctx context.Context, reg *registration, key string, src *source, files fs.FS) (
	*plugin, error,
) {
	start := time.Now()

	if limit := reg.cfg.limit(src.loader); src.desc.MemoryLimit > limit {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: plugin memory limit capped at the Register limit",
			slog.String("key", key), slog.Uint64("asked", src.desc.MemoryLimit), slog.Uint64("limit", limit))
	}

	spec, err := src.loader.prepare(reg, files, src.desc.Entrypoint)
	if err != nil {
		return nil, err
	}

	mod, err := c.module(ctx, src.desc.Language, src.guest, src.limit)
	if err != nil {
		return nil, err
	}

	meters := newPluginMeters(c.meters, key, src.desc.Language, reg.contr.iface)
	launch := c.newLauncher(reg, key, src, spec, mod, meters)
	instances := c.pluginPool(reg, key, src, meters, launch.start, launch.opts.HTTP)

	err = instances.warm(ctx)
	if err != nil {
		return nil, err
	}

	gen := newGeneration(key, reg.contr.iface, &src.desc, instances)

	defer func() {
		if v := recover(); v != nil {
			c.closeUnpublished(ctx, []*plugin{gen})
			panic(v)
		}
	}()

	c.logger.LogAttrs(ctx, slog.LevelInfo, "plg: plugin loaded", slog.String("key", key),
		slog.String("language", string(src.desc.Language)), slog.String("version", src.desc.Version),
		slog.Uint64("memory_limit", src.limit), slog.Int("pool_min", reg.cfg.poolMin),
		slog.Int("pool_max", reg.cfg.poolMax), slog.Duration("duration", time.Since(start)))

	return gen, nil
}

// newLauncher returns the launcher of a plugin version's instances, which start from spec.
func (c *Container) newLauncher(reg *registration, key string, src *source, spec launchSpec, mod *engine.Module,
	meters *pluginMeters,
) *launcher {
	output := c.panics.guardedLogger(c.logger, key, src.desc.Language)
	if c.unguardedOutput.Load() {
		output = c.logger
	}

	return &launcher{
		mod:     mod,
		output:  output,
		panics:  c.panics,
		payload: spec.payload,
		opts: engine.InstanceOptions{
			Mounts:      spec.mounts,
			HTTP:        c.httpHandler(reg, key, src.desc.Language, meters),
			HTTPRefuses: reg.http.Refuses,
			Host:        reg.host.handler(key, src.desc.Language, c.panics),
		},
		key:  key,
		lang: src.desc.Language,
	}
}

// pluginPool returns the (not yet warm) pool of a plugin's instances, started by spawn and measured in meters.
// Instances of a language whose guest multiplexes serve up to the Register's [WithConcurrentCalls] calls at
// once, making their HTTP requests through httpFn.
func (c *Container) pluginPool(reg *registration, key string, src *source, meters *pluginMeters,
	spawn func(context.Context) (instance, error), httpFn engine.HTTPFunc,
) *pool {
	lanes := 1
	if src.loader.multiplexes() {
		lanes = reg.cfg.concurrent
	}

	return newPool(&poolConfig{
		panics:       c.panics,
		spawn:        spawn,
		http:         httpFn,
		budget:       &c.budget,
		meters:       meters,
		key:          key,
		lang:         src.desc.Language,
		limit:        src.limit,
		minSize:      int32(reg.cfg.poolMin), //nolint:gosec // G115: pool sizes are validated to at most maxPoolSize
		maxSize:      int32(reg.cfg.poolMax), //nolint:gosec // G115: pool sizes are validated to at most maxPoolSize
		lanes:        int32(lanes),
		closeTimeout: c.closeTimeout,
	})
}

// publish changes the plugins of iface in the catalogue in one atomic step: the plugins in put are added or
// replace those with the same key, and the keys in removed are dropped. It fails with [ErrClosed] once the
// container is closed.
func (c *Container) publish(iface reflect.Type, put []*plugin, removed []string) error {
	for {
		old := c.catalogue.Load()
		if old.closed {
			return ErrClosed
		}

		if c.catalogue.CompareAndSwap(old, old.with(iface, put, removed)) {
			return nil
		}
	}
}

// closeUnpublished closes generations that were never published and gives their memory reservations back,
// logging failures. Their instances get the container's close timeout to run the plugin's Close.
func (c *Container) closeUnpublished(ctx context.Context, plugins []*plugin) {
	for _, gen := range plugins {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.closeTimeout)
		err := gen.pool.close(closeCtx)

		cancel()
		c.budget.unreserve(gen.reserved)

		if err != nil {
			c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: closing an unpublished plugin failed",
				slog.String("key", gen.key), slog.String("error", err.Error()))
		}
	}
}
