package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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
	state, name := newHostState(&InstanceOptions{}, m.rt), m.rt.moduleName("plg-command-")

	guestCtx, cancel, ok := m.enterNew(ctx, name, state)
	if !ok {
		return 0, fmt.Errorf("running %s: %w", m.name, ErrStopped)
	}

	defer m.rt.hosts.Delete(name)

	cfg := sandbox(name, opts.Mounts, opts.Stdout, opts.Stderr).
		WithStartFunctions("_start").
		WithArgs(append([]string{m.name}, opts.Args...)...)

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
