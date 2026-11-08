package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/sys"
)

// RunOptions configure one run of a WASI command module.
type RunOptions struct {
	// Stdout and Stderr receive the command's output; nil discards it.
	Stdout, Stderr io.Writer
	// Args are the command's arguments, after its name (the module's).
	Args []string
	// Mounts are the command's only file system access.
	Mounts []Mount
	// Env is the command's environment, NAME=value pairs; the command has no other.
	Env []string
}

// Run runs the module as a WASI command: its _start, with the module's name and opts.Args as its arguments, until
// it exits; it returns the command's exit code. The command has what an instance has (clocks, sleeps that end
// with ctx or the runtime's close, a cryptographic random source, the given mounts), opts.Env and no sockets, HTTP
// or Host. A run stopped because ctx ended fails with ctx's error; one stopped by [Runtime.Close] with
// [ErrStopped]; a trap with wazero's error.
func (m *Module) Run(ctx context.Context, opts *RunOptions) (uint32, error) {
	name := "plg-command-" + strconv.FormatUint(m.rt.names.Add(1), 10)
	state := newHostState(&InstanceOptions{}, m.rt)
	m.rt.hosts.Store(name, state)

	defer m.rt.hosts.Delete(name)

	// As in Instantiate: either Close finds this run, or this run sees closing and never starts.
	guestCtx, cancel, ok := state.enter(ctx)
	if ok && m.rt.closing.Load() {
		state.leave(ctx, cancel)

		ok = false
	}

	if !ok {
		return 0, fmt.Errorf("running %s: %w", m.name, ErrStopped)
	}

	fsCfg := wazero.NewFSConfig()
	for _, mnt := range opts.Mounts {
		fsCfg = fsCfg.WithFSMount(mnt.FS, mnt.GuestPath)
	}

	cfg := wazero.NewModuleConfig().
		WithName(name).
		WithStartFunctions("_start").
		WithArgs(append([]string{m.name}, opts.Args...)...).
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(rand.Reader).
		WithFSConfig(fsCfg).
		WithStdout(orDiscard(opts.Stdout)).
		WithStderr(orDiscard(opts.Stderr))

	for _, pair := range opts.Env {
		key, value, _ := strings.Cut(pair, "=")
		cfg = cfg.WithEnv(key, value)
	}

	mod, err := m.rt.rt.InstantiateModule(guestCtx, m.cm, cfg)

	state.leave(ctx, cancel)

	if mod != nil { // the command returned from _start without exiting
		//nolint:errcheck // the command ended: closing its files reports nothing it did not report itself
		_ = mod.Close(ctx)
	}

	return exitStatus(ctx, m.name, state.isStopped(), err)
}

// exitStatus turns how a command run ended into its exit code or error: wazero reports an exit with a non-zero
// code, and a run stopped through its context, as a [sys.ExitError].
func exitStatus(ctx context.Context, name string, stopped bool, err error) (uint32, error) {
	switch {
	case stopped:
		return 0, fmt.Errorf("running %s: %w", name, ErrStopped)
	case ctx.Err() != nil:
		return 0, fmt.Errorf("running %s: %w", name, ctx.Err())
	case err == nil:
		return 0, nil
	}

	if exit, ok := errors.AsType[*sys.ExitError](err); ok {
		return exit.ExitCode(), nil
	}

	return 0, fmt.Errorf("running %s: %w", name, err)
}
