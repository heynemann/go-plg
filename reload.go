package plg

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Reload makes every registered interface mirror its storages again, as [ReloadInterface] does for one, and
// returns the failures of all of them, joined. It fails with [ErrClosed] once the container is closed.
func (c *Container) Reload(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}

	type pending struct {
		round *reloadRound
		name  string
	}

	var rounds []pending

	for name, reg := range c.regs.All() {
		rounds = append(rounds, pending{name: name, round: reg.reloads.request()})
	}

	slices.SortFunc(rounds, func(a, b pending) int { return cmp.Compare(a.name, b.name) })

	errs := make([]error, 0, len(rounds))
	for _, p := range rounds {
		errs = append(errs, p.round.wait(ctx))
	}

	return errors.Join(errs...)
}

// ReloadInterface makes the plugins registered under name mirror its storages again. It lists the storages,
// loads the plugins that are new or whose descriptor or version changed (an empty Version stands for a hash of
// the plugin's files), leaves the others untouched and publishes every change in one atomic step:
//   - a plugin no longer listed is removed: Get fails with [ErrNotFound], and so do calls through it;
//   - a new plugin that fails to load is skipped, and an updated plugin that fails to load keeps serving its last
//     good version;
//   - a key listed by more than one storage, or with an invalid descriptor, keeps its previous version, if any;
//   - a new version needs its worst-case memory in the container's budget ([WithMemoryBudget]) while the
//     version it replaces still holds its own, until it is closed; one that does not fit fails to load.
//
// Each failure is logged and returned, the failures joined; Reload fails without changing anything when a
// storage cannot be listed. A replaced or removed version takes no new calls, and calls running on it finish
// there; after its last call its instances are closed, each running the plugin's optional Close (a method
// Close() error, or Close(), of the instance Create returned), and its memory reservation is given back.
// Adapters obtained from Get before a reload call the new version.
//
// Reloads of one registration run one at a time, in a goroutine of the registration (the poller of
// [WithPollInterval]); ctx bounds only the wait for it, and a reload requested while another runs is served by
// the next. It fails with [ErrNotFound] when name is not registered and with [ErrClosed] once the container is
// closed. It is safe for concurrent use.
func ReloadInterface(ctx context.Context, c *Container, name string) error {
	reg, ok := c.regs.Load(name)
	if !ok {
		if c.closed.Load() {
			return ErrClosed
		}

		return fmt.Errorf("%w: no interface registered as %q", ErrNotFound, name)
	}

	return reg.reloads.request().wait(ctx)
}

// reloader runs the reloads of one registration in one goroutine, so they never overlap: a round for each
// request (requests made while a round runs are served together by the next one) and for each poll tick.
type reloader struct {
	next    atomic.Pointer[reloadRound] // the round the next wake-up runs
	wake    chan struct{}               // a pending request
	stop    context.CancelFunc          // ends the goroutine's context
	stopped chan struct{}               // closed when the goroutine returns
}

// reloadRound is one reload; err is set before done is closed.
type reloadRound struct {
	err  error
	done chan struct{}
}

func newReloader(stop context.CancelFunc) *reloader {
	r := &reloader{wake: make(chan struct{}, 1), stop: stop, stopped: make(chan struct{})}
	r.next.Store(&reloadRound{done: make(chan struct{})})

	return r
}

// request asks for a reload and returns the round that serves it: the next round to start, which lists the
// storages after the request.
func (r *reloader) request() *reloadRound {
	round := r.next.Load()

	select {
	case r.wake <- struct{}{}:
	default: // a request is already pending
	}

	return round
}

// wait returns the round's outcome, waiting for it on ctx.
func (round *reloadRound) wait(ctx context.Context) error {
	select {
	case <-round.done:
		return round.err
	case <-ctx.Done():
		return fmt.Errorf("plg: waiting for a reload: %w", context.Cause(ctx))
	}
}

// run runs a round for each request and, when interval is positive, every interval, until ctx ends.
func (r *reloader) run(ctx context.Context, interval time.Duration, round func(context.Context) error) {
	defer close(r.stopped)
	defer r.end()

	var tick <-chan time.Time

	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		tick = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
		case <-r.wake:
		case <-tick:
		}

		if ctx.Err() != nil { // stopping wins over a pending request or tick
			return
		}

		// The round starting now serves every request made so far: the wake-ups they left are spent, and later
		// requests wait for the round swapped in.
		select {
		case <-r.wake:
		default:
		}

		current := r.next.Swap(&reloadRound{done: make(chan struct{})})
		current.err = round(ctx)
		close(current.done)
	}
}

// end fails the pending round and every later request with [ErrClosed].
func (r *reloader) end() {
	ended := &reloadRound{err: ErrClosed, done: make(chan struct{})}
	close(ended.done)

	pending := r.next.Swap(ended)
	pending.err = ErrClosed
	close(pending.done)
}

// serve starts the registration's reloader and makes the registration reachable by reloads. Close stops the
// reloaders it finds; a Register racing Close stops its own, which Close may have missed.
func (c *Container) serve(ctx context.Context, reg *registration) {
	ctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	reg.reloads = newReloader(stop)

	go reg.reloads.run(ctx, reg.cfg.pollInterval, func(ctx context.Context) error {
		return c.reloadRound(ctx, reg)
	})

	c.regs.Store(reg.name, reg)

	if c.closed.Load() { // Close sets closed before it looks for reloaders
		stop()
		<-reg.reloads.stopped
		reg.http.Close()
	}
}

// stopReloaders stops every registration's reloader, waiting on ctx for the rounds in progress, whose loads
// stop with their context.
func (c *Container) stopReloaders(ctx context.Context) error {
	for _, reg := range c.regs.All() {
		reg.reloads.stop()
	}

	for _, reg := range c.regs.All() {
		select {
		case <-reg.reloads.stopped:
		case <-ctx.Done():
			return fmt.Errorf("plg: stopping reloads: %w", context.Cause(ctx))
		}
	}

	return nil
}

// reloadChanges is what a reload round changes: the generations it loaded, the round's to close until it
// publishes them, and the generations it removes.
type reloadChanges struct {
	loaded  []*plugin
	removed []*plugin
}

// reloadRound reloads reg once. A panic in the round that no inner boundary recovered (a storage's Name, List and
// Open and a plugin's load report their own) is reported as a [PanicError] in [PhasePoll] and returned, so that
// the reloader survives it; generations loaded but not published are closed.
func (c *Container) reloadRound(ctx context.Context, reg *registration) (err error) {
	var changes reloadChanges

	defer func() {
		if v := recover(); v != nil {
			failure := &PanicError{Key: reg.name, Phase: PhasePoll}
			c.panics.recovered(ctx, failure, v)
			c.meters.countReloadFailure(ctx, reg.name)

			err = failure
		}

		c.closeUnpublished(ctx, changes.loaded)
	}()

	return c.refresh(ctx, reg, &changes)
}

// refresh makes reg's plugins mirror its storages; see [ReloadInterface]. Rounds of one registration never
// overlap and Register never changes another name's plugins, so what c serves for reg when the round starts
// is what it replaces, unless the container closes meanwhile.
func (c *Container) refresh(ctx context.Context, reg *registration, changes *reloadChanges) error {
	current := c.generations(reg)

	srcs, problems, err := c.relistPlugins(ctx, reg.name, reg.cfg)
	if err != nil {
		err = fmt.Errorf("plg: reloading %s: %w", reg.name, err)
		c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: reload failed", slog.String("name", reg.name),
			slog.String("error", err.Error()))
		c.meters.countReloadFailure(ctx, reg.name)

		return err
	}

	listed := make(map[string]bool, len(srcs)+len(problems))
	errs := make([]error, 0, len(problems)+1)

	for _, p := range problems {
		key := reg.name + "/" + p.key
		listed[key] = true
		errs = append(errs, c.rejected(ctx, key, current[key], p.err))
	}

	for i := range srcs {
		key := reg.name + "/" + srcs[i].desc.Key
		listed[key] = true

		err := c.reloadOne(ctx, reg, key, &srcs[i], current[key], changes)
		if err != nil {
			if ctx.Err() != nil { // only Close ends the round's context
				return fmt.Errorf("plg: reloading %s: %w", reg.name, ErrClosed)
			}

			errs = append(errs, err)
		}
	}

	for key, gen := range current {
		if !listed[key] {
			changes.removed = append(changes.removed, gen)
		}
	}

	return errors.Join(append(errs, c.apply(ctx, reg, current, changes))...)
}

// reloadOne adds to changes the new generation of the listed plugin src when it is new or changed, and returns
// the failure (logged) when it cannot be loaded; old is the generation served now, if any.
func (c *Container) reloadOne(ctx context.Context, reg *registration, key string, src *source, old *plugin,
	changes *reloadChanges,
) error {
	gen, err := c.update(ctx, reg, key, src, old)

	switch {
	case err != nil && ctx.Err() != nil: // the container is closing: not the plugin's failure
		return err
	case err != nil:
		c.meters.countLoad(ctx, key, src.desc.Language, err)

		return c.rejected(ctx, key, old, err)
	case gen != old:
		c.meters.countLoad(ctx, key, src.desc.Language, nil)

		changes.loaded = append(changes.loaded, gen)
	default: // unchanged
	}

	return nil
}

// generations returns the generations c serves for reg, by key.
func (c *Container) generations(reg *registration) map[string]*plugin {
	prefix := reg.name + "/"
	gens := map[string]*plugin{}

	for _, gen := range c.catalogue.Load().byIface[reg.contr.iface] {
		if strings.HasPrefix(gen.key, prefix) {
			gens[gen.key] = gen
		}
	}

	return gens
}

// update returns the generation to serve under key for the listed plugin src: old, the one served now, when src
// is the same plugin (same descriptor, version included), otherwise a new generation, within the container's
// memory budget. Opening and launching run in [Container.loadStep], as Register's loads do.
func (c *Container) update(ctx context.Context, reg *registration, key string, src *source, old *plugin) (
	*plugin, error,
) {
	if old != nil && src.desc.Version != "" && src.desc == old.desc {
		return old, nil
	}

	var files fs.FS

	err := c.loadStep(ctx, key, src, func() error {
		var openErr error

		files, openErr = c.open(ctx, key, src)

		return openErr
	})
	if err != nil {
		return nil, fmt.Errorf("%w: storage %s: %w", ErrLoad, src.storeName, err)
	}

	if old != nil && src.desc == old.desc {
		return old, nil
	}

	worst := worstCase(reg.cfg, src)
	if !c.budget.reserve(worst) {
		return nil, fmt.Errorf("%w: it needs up to %d bytes, and %d of the %d-byte budget are reserved (a "+
			"replaced version's reservation counts until it is closed)", ErrMemoryBudget, worst,
			c.budget.reserved.Load(), c.budget.total)
	}

	var gen *plugin

	err = c.loadStep(ctx, key, src, func() error {
		var launchErr error

		gen, launchErr = c.launch(ctx, reg, key, src, files)

		return launchErr
	})
	if err != nil {
		c.budget.unreserve(worst)

		return nil, fmt.Errorf("%w: storage %s: %w", ErrLoad, src.storeName, err)
	}

	gen.reserved = worst

	if old != nil {
		gen.value = old.value
	} else {
		gen.value = reg.contr.bind(c, key) //nolint:contextcheck // each adapter call takes the context it is passed
	}

	return gen, nil
}

// rejected logs a plugin that a reload cannot load and returns the failure: a new plugin stays absent, an
// updated one keeps old, its last good generation.
func (c *Container) rejected(ctx context.Context, key string, old *plugin, err error) error {
	err = fmt.Errorf("plg: reloading %s: %w", key, err)
	c.meters.countReloadFailure(ctx, key)

	if old == nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: reload skipped a new plugin that failed to load",
			slog.String("key", key), slog.String("error", err.Error()))
	} else {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: reload kept the last good version of a plugin",
			slog.String("key", key), slog.String("version", old.desc.Version), slog.String("error", err.Error()))
	}

	return err
}

// apply publishes a round's changes in one atomic step, then retires the generations they replace or remove.
func (c *Container) apply(ctx context.Context, reg *registration, current map[string]*plugin,
	changes *reloadChanges,
) error {
	if len(changes.loaded) == 0 && len(changes.removed) == 0 {
		return nil
	}

	removed := make([]string, len(changes.removed))
	for i, gen := range changes.removed {
		removed[i] = gen.key
	}

	err := c.publish(reg.contr.iface, changes.loaded, removed)
	if err != nil {
		return fmt.Errorf("plg: reloading %s: %w", reg.name, err)
	}

	loaded := changes.loaded
	changes.loaded = nil // published: the round no longer closes them

	for _, gen := range loaded {
		if old := current[gen.key]; old != nil {
			c.retire(ctx, old)
		}
	}

	for _, gen := range changes.removed {
		c.retire(ctx, gen)
	}

	for _, gen := range changes.removed {
		c.logger.LogAttrs(ctx, slog.LevelInfo, "plg: plugin removed", slog.String("key", gen.key),
			slog.String("version", gen.desc.Version))
	}

	return nil
}

// retire stops serving gen, which the published catalogue no longer holds. A goroutine waits for its last call
// in flight to leave, then closes its instances, each running the plugin's optional Close, and gives its memory
// reservation back; [Container.Close] waits for it.
func (c *Container) retire(ctx context.Context, gen *plugin) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.retired.Store(gen, cancel)

	go c.closeRetired(ctx, gen)

	gen.retire()
}

// closeRetired closes a retired generation once no call is in flight, or once ctx ends: Close gave up waiting
// for the calls, and their instances are left to the runtimes. Closing gets the container's close timeout; a
// failure is logged and kept in gen.closeErr. A panic meanwhile (the user's logger) is reported as a
// [PanicError] in [PhaseClose] and kept too; the generation counts as closed all the same.
func (c *Container) closeRetired(ctx context.Context, gen *plugin) {
	defer func() {
		if v := recover(); v != nil {
			failure := &PanicError{Key: gen.key, Language: gen.desc.Language, Phase: PhaseClose}
			c.panics.recovered(ctx, failure, v)

			gen.closeErr = errors.Join(gen.closeErr, failure)
		}

		c.budget.unreserve(gen.reserved)
		close(gen.closed)

		if stop, ok := c.retired.LoadAndDelete(gen); ok {
			stop()
		}
	}()

	select {
	case <-gen.idle:
	case <-ctx.Done():
	}

	closeCtx, cancel := context.WithTimeout(ctx, c.closeTimeout)
	defer cancel()

	err := gen.pool.close(closeCtx)
	gen.closeErr = err

	if err != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: closing a plugin version failed", slog.String("key", gen.key),
			slog.String("version", gen.desc.Version), slog.String("error", err.Error()))
	} else {
		c.logger.LogAttrs(ctx, slog.LevelInfo, "plg: plugin version closed", slog.String("key", gen.key),
			slog.String("version", gen.desc.Version))
	}
}

// awaitRetired waits on ctx for every retired generation to be closed. When ctx ends first, it makes the
// others close what is idle at once, leaving the instances still in use to the runtimes, and waits for that.
func (c *Container) awaitRetired(ctx context.Context) error {
	var err error

	for gen, stop := range c.retired.All() {
		if err == nil {
			select {
			case <-gen.closed:
				continue
			case <-ctx.Done():
				err = fmt.Errorf("plg: draining: %w", context.Cause(ctx))
			}
		}

		stop()
		<-gen.closed
	}

	return err
}
