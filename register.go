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

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/heynemann/go-plg/internal/artifact"
	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/internal/httpcap"
	"github.com/heynemann/go-plg/internal/memfs"
)

// Register loads, under name, every plugin its storages list for the interface T, and publishes them in one
// step: each plugin's key is "<name>/<descriptor key>". It loads synchronously and fails fast: on any error
// nothing is published. Plugins declaring another interface ([Descriptor.Implements]) and plugins in a language
// the language options ([WithGolangPlugins], …) leave out are skipped and logged, each with its reason
// ([ErrOtherInterface], [ErrUnsupportedLanguage]). Afterwards the registration follows its storages on
// [Container.Reload], [ReloadInterface] and, with [WithPollInterval], by itself.
//
// A plugin implementing T is written against a version of T's contract module, which its Implements names. The
// program's version of that module is the one its build info records ([runtime/debug.ReadBuildInfo]): a plugin
// loads when both have the same major version and the plugin's minor version is not newer, and is otherwise
// rejected with an error issue ([CodeContractVersion]) naming both versions. A program whose contract module is
// built from a local checkout (its version is "(devel)": tests, go run, a replace directive pointing at a
// directory) cannot tell, so each load carries a warning issue instead ([CodeContractDevel]).
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

	return c.registerPrepared(ctx, iface, reg)
}

// registerPrepared loads and publishes reg, a prepared Register call of iface, under its name.
func (c *Container) registerPrepared(ctx context.Context, iface reflect.Type, reg *registration) error {
	name := reg.name

	if _, dup := c.names.LoadOrStore(name, reg); dup {
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

	for _, plug := range plugins {
		c.events.generation(ctx, EventLoaded, plug, plug.report)
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
	validation *validation     // the WithValidation option's; nil without one
	reloads    *reloader       // set once the registration is published
	// keys are the plugin keys its storages list, as of their latest listing, with why each is not served as
	// listed and the panics reported under it.
	keys *xsync.Map[string, *listedPlugin]
	// module is the contract module the program is built with, which the plugins' pinned versions are checked
	// against: the contract's.
	module     contractref.Module
	name       string
	dispatcher []byte // the Go dispatcher: the contract's, or the one rendered with the Host and the validation
	goPayload  []byte // the Go init payload, likewise
	schema     []byte // the script guests' wire.Schema: the interface's methods, then the Host's
}

// prepare checks a Register call's arguments and returns the interface's contract, the Register options (with
// their defaults), the HTTP client of its policy, its Host and its validation.
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

	return c.newRegistration(contr, cfg, name, hst)
}

// newRegistration prepares the registration of a Register call whose contract, options and Host (nil without one)
// were checked: its validation, the Go dispatcher rendered for them, its HTTP client and the script guests' schema.
func (c *Container) newRegistration(contr *contract, cfg *registerConfig, name string, hst *host) (
	*registration, error,
) {
	reg := &registration{
		contr: contr, cfg: cfg, name: name, keys: xsync.NewMap[string, *listedPlugin](),
		module: contr.module, dispatcher: contr.dispatcher, goPayload: contr.payload, host: hst,
	}

	validation, err := newValidation(contr, cfg)
	reg.validation = validation

	if err == nil && (reg.host != nil || reg.validation != nil) {
		err = reg.renderGo()
	}

	if err != nil {
		return nil, err
	}

	reg.http, err = httpClient(cfg, c.logger)
	if err != nil {
		return nil, err
	}

	var hostMethods []*method

	if reg.host != nil {
		hostMethods = reg.host.methods
	}

	reg.schema = scriptSchema(contr.methods, reg.validation, hostMethods)

	return reg, nil
}

// source is a plugin a storage listed.
type source struct {
	storage    Storage
	loader     loader
	guest      guestreg.Guest
	provenance *Provenance     // the provenance of the artifact Open returned; nil for other files
	ref        contractref.Ref // the interface desc.Implements names
	storeName  string          // storage.Name(), asked once
	desc       Descriptor
	limit      uint64 // per-instance memory limit, whole pages
}

// problem is a listed plugin that cannot be loaded: its descriptor is invalid, its key is listed twice or its
// language's guest is missing. storage names the storage listing it and desc is as listed, its Key empty when it
// has none.
type problem struct {
	err     error
	storage string
	desc    Descriptor
}

// pluginList is what listing a registration's storages found.
type pluginList struct {
	srcs     []source        // the plugins to load
	skipped  []skippedSource // the plugins implementing another interface, or in languages the Register leaves out
	problems []problem       // the plugins that cannot be loaded; none for Register, which fails on the first
}

// skippedSource is a listed plugin its registration skips, and why: [ErrOtherInterface] or
// [ErrUnsupportedLanguage].
type skippedSource struct {
	reason error
	src    source
}

// listPlugins lists the storages for Register, which fails on any problem: a storage that cannot be listed, an
// invalid descriptor, a key listed twice, a language whose guest is not linked. Plugins implementing another
// interface or in languages the Register did not enable are skipped and logged.
func (c *Container) listPlugins(ctx context.Context, reg *registration) (pluginList, error) {
	name := reg.name

	listed, problems, err := c.listStorages(ctx, name, reg.cfg.storages)
	for i := range problems {
		c.events.problem(ctx, problemRejection(name+"/"+problems[i].desc.Key, &problems[i]))
	}

	if err == nil && len(problems) > 0 {
		err = problems[0].err
	}

	if err != nil {
		return pluginList{}, err
	}

	found := pluginList{srcs: listed[:0]}

	for i := range listed {
		reason := c.skip(ctx, reg, &listed[i], slog.LevelInfo)
		if reason != nil {
			found.skipped = append(found.skipped, skippedSource{reason: reason, src: listed[i]})

			continue
		}

		err := admit(reg, &listed[i])
		if err != nil {
			c.events.problem(ctx, problemRejection(name+"/"+listed[i].desc.Key,
				&problem{err: err, storage: listed[i].storeName, desc: listed[i].desc}))

			return pluginList{}, err
		}

		found.srcs = append(found.srcs, listed[i])
	}

	return found, nil
}

// relistPlugins lists the storages for a reload: it returns the plugins to load, those skipped and the problems
// of the others, and fails only when a storage cannot be listed. Skipped plugins are logged at Debug level only,
// rather than on every poll.
func (c *Container) relistPlugins(ctx context.Context, reg *registration) (pluginList, error) {
	listed, problems, err := c.listStorages(ctx, reg.name, reg.cfg.storages)
	if err != nil {
		return pluginList{}, err
	}

	found := pluginList{srcs: listed[:0], problems: problems}

	for i := range listed {
		reason := c.skip(ctx, reg, &listed[i], slog.LevelDebug)
		if reason != nil {
			found.skipped = append(found.skipped, skippedSource{reason: reason, src: listed[i]})

			continue
		}

		err := admit(reg, &listed[i])
		if err != nil {
			found.problems = append(found.problems,
				problem{err: err, storage: listed[i].storeName, desc: listed[i].desc})

			continue
		}

		found.srcs = append(found.srcs, listed[i])
	}

	return found, nil
}

// skip returns why reg skips a listed plugin ([registration.skips]), nil when it does not; a skip is logged at level
// and counted.
func (c *Container) skip(ctx context.Context, reg *registration, src *source, level slog.Level) error {
	kind, reason := reg.skips(src)
	if reason == nil {
		return nil
	}

	key := reg.name + "/" + src.desc.Key
	attrs := []slog.Attr{
		slog.String("key", key), slog.String("storage", src.storeName),
		slog.String("language", string(src.desc.Language)), slog.String("implements", src.desc.Implements),
		src.desc.owner().logAttr(),
	}

	if kind == skipInterface {
		c.logger.LogAttrs(ctx, level, "plg: plugin skipped: it implements another interface", attrs...)
	} else {
		c.logger.LogAttrs(ctx, level, "plg: plugin skipped: its language is not enabled", attrs...)
	}

	c.countSkipped(ctx, key, src.desc.Language, src.desc.owner(), kind)

	return reason
}

// admit prepares a listed plugin reg serves for loading. It fails when the language's guest is not linked.
func admit(reg *registration, src *source) error {
	ld, guest, err := reg.cfg.guestFor(src.desc.Language)
	if err != nil {
		return fmt.Errorf("plugin %s (storage %s): %w", src.desc.Key, src.storeName, err)
	}

	src.loader, src.guest, src.limit = ld, guest, instanceLimit(reg.cfg.limit(ld), &src.desc)

	return nil
}

// skipKind is why a registration skips a listed plugin, as the plg.plugin.skipped metric's reason attribute says.
type skipKind string

const (
	skipInterface skipKind = "interface" // the plugin implements another interface
	skipLanguage  skipKind = "language"  // the Register does not enable the plugin's language
)

// skips returns why r skips src, a listed plugin, and the kind of that reason; a nil error when r serves it. It
// skips a plugin implementing another interface than r's ([ErrOtherInterface]) and, among the others, one in a
// language the Register does not enable ([ErrUnsupportedLanguage]).
func (r *registration) skips(src *source) (skipKind, error) {
	contr := r.contr
	if src.ref.Package != contr.pkgPath || src.ref.Interface != contr.name {
		return skipInterface, fmt.Errorf("%w: %s implements %s, not %s.%s", ErrOtherInterface, src.desc.Key,
			src.ref.Name(), contr.pkgPath, contr.name)
	}

	if !r.cfg.enabled(src.desc.Language) {
		return skipLanguage, fmt.Errorf("%w: %s plugins are not enabled for %s", ErrUnsupportedLanguage,
			src.desc.Language, r.name)
	}

	return "", nil
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

// listStorages lists every storage, checking each descriptor ([checkDescriptor]) and that no key is listed twice,
// whatever its interface or language. A key with a problem is not loaded from any storage. A panicking storage
// fails with a reported [PanicError].
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
			ref, err := checkDescriptor(storeName, &desc)
			if err == nil {
				if prev, dup := owner[desc.Key]; dup {
					err = fmt.Errorf("%w %q: listed by storages %s and %s", ErrDuplicateKey, desc.Key, prev,
						storeName)
				}
			}

			if err != nil {
				problems = append(problems, problem{err: err, storage: storeName, desc: desc})

				continue
			}

			owner[desc.Key] = storeName
			srcs = append(srcs, source{storage: store, storeName: storeName, desc: desc, ref: ref})
		}
	}

	srcs = slices.DeleteFunc(srcs, func(src source) bool {
		return slices.ContainsFunc(problems, func(p problem) bool { return p.desc.Key == src.desc.Key })
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

// checkDescriptor checks that a descriptor has a key, an entrypoint and a well-formed Implements, which it returns
// parsed, a memory limit from one page to 4 GiB if any, an owner within the limits and valid secret names.
func checkDescriptor(storeName string, desc *Descriptor) (contractref.Ref, error) {
	if desc.Key == "" || desc.Entrypoint == "" {
		return contractref.Ref{}, fmt.Errorf("%w: storage %s lists %+v without a key or an entrypoint",
			ErrInvalidDescriptor, storeName, *desc)
	}

	ref, err := contractref.Parse(desc.Implements)
	if err != nil {
		return contractref.Ref{}, fmt.Errorf("%w: storage %s lists %q: %w", ErrInvalidDescriptor, storeName,
			desc.Key, err)
	}

	if desc.MemoryLimit != 0 && !validMemoryLimit(desc.MemoryLimit) {
		return contractref.Ref{}, fmt.Errorf("%w: storage %s lists %q with a memory limit of %d bytes, not from "+
			"one %d-byte page to 4 GiB", ErrInvalidDescriptor, storeName, desc.Key, desc.MemoryLimit, minMemoryLimit)
	}

	err = desc.owner().check()
	if err != nil {
		return contractref.Ref{}, fmt.Errorf("%w: storage %s lists %q: %w", ErrInvalidDescriptor, storeName,
			desc.Key, err)
	}

	err = checkSecretNames(desc.Secrets)
	if err != nil {
		return contractref.Ref{}, fmt.Errorf("%w: storage %s lists %q: %w", ErrInvalidDescriptor, storeName,
			desc.Key, err)
	}

	return ref, nil
}

// sameDescriptor reports whether a and b describe the same plugin version: every field, the owner and the secrets
// included.
func sameDescriptor(a, b *Descriptor) bool {
	return a.Key == b.Key && a.Implements == b.Implements && a.Language == b.Language &&
		a.Entrypoint == b.Entrypoint && a.Version == b.Version && a.MemoryLimit == b.MemoryLimit &&
		a.owner().equal(b.owner()) && slices.Equal(a.Secrets, b.Secrets)
}

// loadAll reserves the listed plugins' worst-case memory in the container's budget and loads them; on failure
// it closes the ones already loaded and gives the reservation back. It records in reg the keys listed (before
// each load, which may report panics under its key) and the plugins skipped, whose [EventSkipped] events follow
// their records (each event is kept in its key's record).
func (c *Container) loadAll(ctx context.Context, reg *registration) ([]*plugin, error) {
	found, err := c.listPlugins(ctx, reg)
	if err != nil {
		return nil, err
	}

	for i := range found.skipped {
		key := reg.name + "/" + found.skipped[i].src.desc.Key
		rej := skipRejection(key, &found.skipped[i])
		reg.entry(key).rejection.Store(rej)
		c.events.source(ctx, EventSkipped, key, &found.skipped[i].src, rej.report)
	}

	srcs := found.srcs

	reserved, err := c.reserveMemory(reg.name, reg.cfg, srcs)
	if err != nil {
		return nil, err
	}

	plugins := make([]*plugin, 0, len(srcs))

	for i := range srcs {
		key := reg.name + "/" + srcs[i].desc.Key
		reg.entry(key)

		report := newReport(key, &srcs[i], time.Now())

		plug, err := c.load(ctx, reg, key, &srcs[i], report)
		c.finishReport(ctx, report, &srcs[i], err)

		if err != nil {
			c.events.source(ctx, EventRejected, key, &srcs[i], report)
			c.countLoad(ctx, key, srcs[i].desc.Language, srcs[i].desc.owner(), err)
			c.closeUnpublished(ctx, plugins)
			c.budget.unreserve(reserved)

			return nil, fmt.Errorf("%w: %s (storage %s): %w", ErrLoad, key, srcs[i].storeName, err)
		}

		plug.value = reg.contr.bind(c, key) //nolint:contextcheck // each adapter call takes the context it is passed
		plug.reserved = worstCase(reg.cfg, &srcs[i])
		plug.report = report
		reserved -= plug.reserved
		plugins = append(plugins, plug) // the Register's to close from now on

		c.countLoad(ctx, key, srcs[i].desc.Language, srcs[i].desc.owner(), nil)
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

// load opens a listed plugin and launches it, adding what it finds to report. A panic on the host meanwhile (the
// plugin's fs.FS is the storage's code) fails with a reported [PanicError] in [PhaseLoad].
func (c *Container) load(ctx context.Context, reg *registration, key string, src *source, report *Report) (
	*plugin, error,
) {
	var plug *plugin

	err := c.loadStep(ctx, key, src, func() error {
		files, err := c.open(ctx, key, src)
		if err == nil {
			plug, err = c.launch(ctx, reg, key, src, files, report)
		}

		return err
	})

	return plug, err
}

// loadStep runs a step of loading the plugin key, turning a panic into a reported [PanicError] in [PhaseLoad].
func (c *Container) loadStep(ctx context.Context, key string, src *source, step func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			failure := &PanicError{
				Key: key, Language: src.desc.Language, Phase: PhaseLoad, Owner: *src.desc.owner(),
				tags: newOwnerTags(src.desc.owner(), src.desc.Version, src.storeName),
			}
			c.panics.recovered(ctx, failure, v)

			err = failure
		}
	}()

	return step()
}

// open opens a listed plugin's files, copies them into the snapshot it returns (see [Storage]) and resolves its
// version: a content hash of the snapshot when the storage gives none. Hashing, bundling and loading all read
// that one snapshot.
func (c *Container) open(ctx context.Context, key string, src *source) (*memfs.FS, error) {
	var opened fs.FS

	err := c.storageCall(ctx, key, "Open", func() error {
		var openErr error

		opened, openErr = src.storage.Open(ctx, &src.desc)

		return openErr //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	return snapshotPlugin(ctx, src, opened)
}

// snapshotPlugin copies a plugin's files into the snapshot it returns, under the snapshot limits (see [Storage]), and
// resolves its version: a content hash of the snapshot when its descriptor gives none. Files opened as an
// [Artifact] are used as they are, and its provenance recorded; their content hash is its digest, which
// [OpenArtifact] checked them against.
func snapshotPlugin(ctx context.Context, src *source, opened fs.FS) (*memfs.FS, error) {
	src.provenance = nil
	if art, ok := opened.(*Artifact); ok {
		src.provenance = &art.provenance
	}

	files, err := snapshotFiles(ctx, opened)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	switch {
	case src.desc.Version != "":
	case src.provenance != nil: // OpenArtifact checked these files against its digest
		src.desc.Version = src.provenance.Digest
	default:
		src.desc.Version = artifact.Digest(files)
	}

	return files, nil
}

// launch checks an opened plugin, adding its issues to report, with [WithValidation] validates it in an
// instance of its own, and starts the minimum instances of its pool (running Create in each); it returns the
// generation the pool's instances serve. An issue that blocks the load (or a failed validation) fails it before
// any pool instance starts: the validation instance runs, and is closed, before the pool warms, so it fits in
// the version's memory reservation. Its adapter, its memory reservation and its report are the caller's to set.
// A panic once the instances started closes them before going on, nothing else owning them yet (the logger is
// guarded, so its handler's panic is not one).
func (c *Container) launch(ctx context.Context, reg *registration, key string, src *source, files *memfs.FS,
	report *Report,
) (*plugin, error) {
	start := time.Now()

	launch, meters, err := c.prepareLaunch(ctx, reg, key, src, files, report)
	if err != nil {
		return nil, err
	}

	// The version's reservation covers its pool's maximum and no pool instance runs yet, so it has room for the
	// validation instance: nothing is reserved beyond it.
	found := len(report.Issues)
	err = c.validate(ctx, reg, src, files, launch, 0, report)
	c.countValidation(ctx, reg, launch, triggerLoad, err, report.Issues[found:])

	if err != nil {
		return nil, err
	}

	owner := launch.owner
	instances := c.pluginPool(reg, key, src, owner, meters, launch.start, launch.opts.HTTP)

	err = instances.warm(ctx)
	if err != nil {
		return nil, err
	}

	gen := newGeneration(key, reg.contr.iface, &src.desc, instances)
	gen.reg, gen.launch, gen.storage, gen.spans = reg, launch, src.storeName, launch.spans
	gen.provenance = src.provenance

	defer func() {
		if v := recover(); v != nil {
			c.closeUnpublished(ctx, []*plugin{gen})
			panic(v)
		}
	}()

	c.logger.LogAttrs(ctx, slog.LevelInfo, "plg: plugin loaded", slog.String("key", key), owner.log,
		slog.String("language", string(src.desc.Language)), slog.String("version", src.desc.Version),
		slog.Uint64("memory_limit", src.limit), slog.Int("pool_min", reg.cfg.poolMin),
		slog.Int("pool_max", reg.cfg.poolMax), slog.Duration("duration", time.Since(start)))

	return gen, nil
}

// prepareLaunch checks an opened plugin, adding its issues to report, and prepares the launcher its instances
// start from and the meters they are measured in; no instance starts yet. It fails when an issue blocks the
// load, the plugin's language cannot validate under [WithValidation] or the plugin cannot be prepared.
func (c *Container) prepareLaunch(ctx context.Context, reg *registration, key string, src *source,
	files *memfs.FS, report *Report,
) (*launcher, *pluginMeters, error) {
	owner := newOwnerTags(src.desc.owner(), src.desc.Version, src.storeName)

	if limit := reg.cfg.limit(src.loader); src.desc.MemoryLimit > limit {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: plugin memory limit capped at the Register limit",
			slog.String("key", key), owner.log, slog.Uint64("asked", src.desc.MemoryLimit), slog.Uint64("limit", limit))
	}

	err := checkLoad(reg, src, report)
	if err != nil {
		return nil, nil, err
	}

	spec, err := src.loader.prepare(ctx, c, reg, src, files)
	if err != nil {
		return nil, nil, err
	}

	mod, err := c.module(ctx, src.desc.Language, src.guest, src.limit)
	if err != nil {
		return nil, nil, err
	}

	meters := newPluginMeters(c.meters, key, src.desc.Language, owner, reg.contr.methods)
	spans := newPluginSpans(c.tracing, key, src.desc.Language, owner, reg.contr.methods, reg.host)

	return c.newLauncher(reg, key, src, owner, spec, mod, meters, spans), meters, nil
}

// checkLoad checks a plugin about to launch, adding the issues found to report: the contract version it pins
// against the program's and its ownership. It fails when an issue blocks the load.
func checkLoad(reg *registration, src *source, report *Report) error {
	checkContract(report, reg.module, src.ref)
	checkOwnership(report, src.desc.owner(), reg.cfg.ownership)

	return report.blocking()
}

// newLauncher returns the launcher of a plugin version's instances, which start from spec and read the secrets
// the version declares.
func (c *Container) newLauncher(reg *registration, key string, src *source, owner *ownerTags, spec launchSpec,
	mod *engine.Module, meters *pluginMeters, spans *pluginSpans,
) *launcher {
	output := c.panics.pluginLogger(key, src.desc.Language, owner)
	info := PluginInfo{
		Owner: *src.desc.owner(), Key: key, Version: src.desc.Version, Language: src.desc.Language,
		Storage: src.storeName,
	}
	secrets := newPluginSecrets(reg.cfg, &info, src.desc.Secrets, c.panics, output, owner)

	return &launcher{
		mod:           mod,
		output:        output,
		panics:        c.panics,
		owner:         owner,
		spans:         spans,
		secrets:       secrets,
		payload:       spec.payload,
		createTimeout: reg.cfg.createTimeout,
		opts: engine.InstanceOptions{
			Mounts:      spec.mounts,
			HTTP:        c.httpHandler(reg, key, src.desc.Language, owner, meters, spans, secrets.redact),
			HTTPRefuses: reg.http.Refuses,
			Host:        reg.host.handler(key, src.desc.Language, owner, c.panics, spans),
			Secret:      secrets.serve,
		},
		key:  key,
		lang: src.desc.Language,
	}
}

// pluginPool returns the (not yet warm) pool of a plugin's instances, started by spawn and measured in meters.
// Instances of a language whose guest multiplexes serve up to the Register's [WithConcurrentCalls] calls at
// once, making their HTTP requests through httpFn.
func (c *Container) pluginPool(reg *registration, key string, src *source, owner *ownerTags, meters *pluginMeters,
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
		owner:        owner,
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
				slog.String("key", gen.key), gen.pool.owner.log, slog.String("error", err.Error()))
		}
	}
}
