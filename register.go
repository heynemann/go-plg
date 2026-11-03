package plg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/internal/wire"
)

// Register loads, under name, every plugin its storages list for the interface T, and publishes them in one
// step: each plugin's key is "<name>/<descriptor key>". It loads synchronously and fails fast: on any error
// nothing is published.
//
// It fails if T breaks the interface contract (every method takes a context.Context first, returns an error
// last and uses only data types) with [ErrInvalidInterface], if T has no [Contract] with [ErrNoContract], if
// name is already registered with [ErrDuplicateName], if a plugin's language guest is not linked into the
// program with [ErrGuestNotLinked], and if a plugin fails to load (its Create included) with [ErrLoad].
func Register[T any](ctx context.Context, c *Container, name string, opts ...RegisterOption) error {
	return c.register(ctx, reflect.TypeFor[T](), name, opts)
}

func (c *Container) register(ctx context.Context, iface reflect.Type, name string, opts []RegisterOption) error {
	contr, storages, err := c.prepare(iface, name, opts)
	if err != nil {
		return err
	}

	if _, dup := c.names.LoadOrStore(name, true); dup {
		return fmt.Errorf("%w: %s", ErrDuplicateName, name)
	}

	plugins, err := c.loadAll(ctx, contr, name, storages)
	if err == nil {
		err = c.publish(ctx, iface, plugins)
	}

	if err != nil {
		c.names.Delete(name)

		return err
	}

	return nil
}

// prepare checks a Register call's arguments and returns the interface's contract and the storages.
func (c *Container) prepare(iface reflect.Type, name string, opts []RegisterOption) (*contract, []Storage, error) {
	if c.closed.Load() {
		return nil, nil, ErrClosed
	}

	if name == "" || strings.Contains(name, "/") {
		return nil, nil, fmt.Errorf("%w %q: names are non-empty and have no slash", ErrInvalidName, name)
	}

	err := validateInterface(iface)
	if err != nil {
		return nil, nil, err
	}

	contr, err := lookupContract(iface)
	if err != nil {
		return nil, nil, err
	}

	var cfg registerConfig

	for _, opt := range opts {
		opt(&cfg)
	}

	if len(cfg.storages) == 0 || slices.Contains(cfg.storages, nil) {
		return nil, nil, fmt.Errorf("%w: register %s with at least one non-nil WithStorage", ErrNoStorage, name)
	}

	return contr, cfg.storages, nil
}

// source is a plugin a storage listed.
type source struct {
	storage Storage
	guest   guestreg.Guest
	desc    Descriptor
}

// listPlugins lists the storages and checks their descriptors: every key and entrypoint first, then that each
// plugin's language guest is linked.
func listPlugins(ctx context.Context, storages []Storage) ([]source, error) {
	var srcs []source

	owner := map[string]string{} // key -> storage name

	for _, store := range storages {
		descs, err := store.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: listing storage %s: %w", ErrLoad, store.Name(), err)
		}

		for _, desc := range descs {
			if desc.Key == "" || desc.Entrypoint == "" {
				return nil, fmt.Errorf("%w: storage %s lists %+v without a key or an entrypoint",
					ErrInvalidDescriptor, store.Name(), desc)
			}

			if prev, dup := owner[desc.Key]; dup {
				return nil, fmt.Errorf("%w %q: listed by storages %s and %s", ErrDuplicateKey, desc.Key, prev,
					store.Name())
			}

			owner[desc.Key] = store.Name()
			srcs = append(srcs, source{storage: store, desc: desc})
		}
	}

	for i := range srcs {
		var err error

		srcs[i].guest, err = guestFor(srcs[i].desc.Language)
		if err != nil {
			return nil, fmt.Errorf("plugin %s (storage %s): %w", srcs[i].desc.Key, srcs[i].storage.Name(), err)
		}
	}

	return srcs, nil
}

// loadAll loads every listed plugin; on failure it closes the ones already loaded.
func (c *Container) loadAll(ctx context.Context, contr *contract, name string, storages []Storage) (
	[]*plugin, error,
) {
	srcs, err := listPlugins(ctx, storages)
	if err != nil {
		return nil, err
	}

	plugins := make([]*plugin, 0, len(srcs))

	for i := range srcs {
		key := name + "/" + srcs[i].desc.Key

		plug, err := c.load(ctx, contr, key, &srcs[i])
		if err != nil {
			c.closePlugins(ctx, plugins)

			return nil, fmt.Errorf("%w: %s (storage %s): %w", ErrLoad, key, srcs[i].storage.Name(), err)
		}

		plugins = append(plugins, plug)
	}

	return plugins, nil
}

// load opens a plugin, starts its first instance (running its Create) and binds an adapter to it.
func (c *Container) load(ctx context.Context, contr *contract, key string, src *source) (*plugin, error) {
	start := time.Now()

	files, err := src.storage.Open(ctx, src.desc)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	tree, err := goSourceTree(contr, files, src.desc.Entrypoint)
	if err != nil {
		return nil, err
	}

	mod, err := c.module(ctx, src.desc.Language, src.guest)
	if err != nil {
		return nil, err
	}

	spawn := func(ctx context.Context) (*engine.Instance, error) {
		return startGo(ctx, mod, tree, contr.payload)
	}

	first, err := spawn(ctx)
	if err != nil {
		return nil, err
	}

	plug := &plugin{key: key, iface: contr.iface}
	plug.pool = newPool(key, first, spawn, c.logger)
	plug.value = contr.bind(plug) //nolint:contextcheck // each adapter call takes the context it is passed

	c.logger.LogAttrs(ctx, slog.LevelInfo, "plg: plugin loaded", slog.String("key", key),
		slog.String("language", string(src.desc.Language)), slog.String("version", src.desc.Version),
		slog.Duration("duration", time.Since(start)))

	return plug, nil
}

var (
	errCreate     = errors.New("plugin Create failed")
	errGuestLoad  = errors.New("plugin source failed to load")
	errInitStatus = errors.New("guest failed to initialize")
)

// startGo starts a Go guest instance over the plugin's source tree and runs the plugin's Create.
func startGo(ctx context.Context, mod *engine.Module, tree fs.FS, payload []byte) (*engine.Instance, error) {
	inst, err := mod.Instantiate(ctx, engine.InstanceOptions{
		Mounts: []engine.Mount{{FS: tree, GuestPath: goabi.GoPath}},
	})
	if err != nil {
		return nil, fmt.Errorf("starting an instance: %w", err)
	}

	res, err := inst.Init(ctx, payload)
	if err == nil {
		err = initError(res)
	}

	if err != nil {
		return nil, errors.Join(err, inst.Close(context.WithoutCancel(ctx)))
	}

	return inst, nil
}

// initError maps a plg_init result to an error.
func initError(res engine.Result) error {
	switch res.Status { //nolint:exhaustive // panic and ABI statuses both mean the guest failed to initialize
	case wire.StatusOK:
		return nil
	case wire.StatusError:
		return fmt.Errorf("%w: %s", errCreate, res.Message)
	case wire.StatusLoad:
		return fmt.Errorf("%w: %s", errGuestLoad, res.Message)
	default:
		return fmt.Errorf("%w: status %s: %s", errInitStatus, res.Status, res.Message)
	}
}

// publish adds the plugins to the catalogue in one atomic step.
func (c *Container) publish(ctx context.Context, iface reflect.Type, plugins []*plugin) error {
	for {
		old := c.catalogue.Load()
		if old.closed {
			c.closePlugins(ctx, plugins)

			return ErrClosed
		}

		if c.catalogue.CompareAndSwap(old, old.with(iface, plugins)) {
			return nil
		}
	}
}

// closePlugins closes plugins that were never published, logging failures.
func (c *Container) closePlugins(ctx context.Context, plugins []*plugin) {
	for _, p := range plugins {
		err := p.pool.close(context.WithoutCancel(ctx))
		if err != nil {
			c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: closing an unpublished plugin failed",
				slog.String("key", p.key), slog.String("error", err.Error()))
		}
	}
}
