package plg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestout"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/wire"
)

// loader is what loading a plugin needs from its language; everything else (the guest ABI, the pool, output
// capture, panic reporting, reloads) is shared by every language.
type loader interface {
	// guestImport is the package that links the language's guest, named when it is not linked.
	guestImport() string
	// memoryLimit is the default per-instance memory limit of the language's plugins, for a Register without
	// [WithMemoryLimit].
	memoryLimit() uint64
	// multiplexes reports whether the language's guest can serve several calls at once ([WithConcurrentCalls]).
	multiplexes() bool
	// prepare turns an opened plugin's files (the snapshot of src) into what each of its instances starts from:
	// the init payload and the file trees mounted read-only into the guest. It fails when the plugin cannot load
	// (a missing entrypoint, source the host rejects) and when ctx ends; c runs what it runs on the host
	// (TypeScript's bundler).
	prepare(ctx context.Context, c *Container, reg *registration, src *source, files *memfs.FS) (launchSpec, error)
}

// launchSpec is how every instance of one plugin version starts.
type launchSpec struct {
	payload []byte
	mounts  []engine.Mount
}

// loaderFor returns the loader of a language, or [ErrUnsupportedLanguage].
//
//nolint:ireturn // the loader is the per-language seam
func loaderFor(lang Language) (loader, error) {
	switch lang {
	case LanguageGo:
		return goLoader{}, nil
	case LanguageTypeScript:
		return tsLoader{}, nil
	case LanguageLua:
		return luaLoader{}, nil
	default:
		return nil, fmt.Errorf("%w %q", ErrUnsupportedLanguage, lang)
	}
}

// guestFor returns the loader of a language and its linked guest.
//
//nolint:ireturn // the loader is the per-language seam
func guestFor(lang Language) (loader, guestreg.Guest, error) {
	langLoader, err := loaderFor(lang)
	if err != nil {
		return nil, guestreg.Guest{}, err
	}

	guest, ok := guestreg.Lookup(string(lang))
	if !ok {
		return nil, guestreg.Guest{}, fmt.Errorf("%w: %s plugins need the program to import _ %q",
			ErrGuestNotLinked, lang, langLoader.guestImport())
	}

	return langLoader, guest, nil
}

var (
	errCreate      = errors.New("plugin Create failed")
	errGuestLoad   = errors.New("plugin source failed to load")
	errGuestStatus = errors.New("guest status")
)

// launcher starts the instances of one plugin version.
type launcher struct {
	mod *engine.Module
	// output logs the instances' output: the user's logger behind the callback boundary
	// ([panicReporter.guardedLogger]), since its records are logged inside the guest's steps.
	output  *slog.Logger
	panics  *panicReporter
	opts    engine.InstanceOptions
	key     string
	lang    Language
	payload []byte
}

// start starts an instance, mounting the plugin's files, and runs the plugin's Create. A guest that panics,
// traps or breaks the ABI meanwhile fails with a reported [PanicError] in [PhaseCreate]. Until start returns the
// instance, it is start's to close: a failed Create closes it, and so does a panic on the way to the pool.
//
//nolint:ireturn // it is the pool's spawn function, which returns the pool's instance interface
func (l *launcher) start(ctx context.Context) (instance, error) {
	out := guestout.New(l.output, l.key)
	opts := l.opts
	opts.Stdout, opts.Stderr = out.Stdout(), out.Stderr()

	out.Begin()

	inst, err := l.mod.Instantiate(ctx, &opts)
	if err != nil {
		out.End(ctx)

		return nil, fmt.Errorf("starting an instance: %w", err)
	}

	created := false

	defer func() {
		if !created { // a panic is unwinding start
			l.discardUnstarted(ctx, inst)
		}
	}()

	err = l.create(ctx, inst, out)
	created = true

	if err != nil {
		return nil, errors.Join(err, inst.Close(context.WithoutCancel(ctx)))
	}

	return &guestInstance{Instance: inst, launcher: l, out: out}, nil
}

// create runs the plugin's Create in inst and ends out's step, returning why Create failed.
func (l *launcher) create(ctx context.Context, inst *engine.Instance, out *guestout.Capture) error {
	res, err := inst.Init(ctx, l.payload)
	out.End(ctx)

	switch {
	case err != nil:
		return l.failed(ctx, PhaseCreate, err, out)
	case res.Status == wire.StatusOK:
		return nil
	case res.Status == wire.StatusError:
		return fmt.Errorf("%w: %s", errCreate, res.Message)
	case res.Status == wire.StatusLoad:
		return fmt.Errorf("%w: %s", errGuestLoad, res.Message)
	default:
		return l.failed(ctx, PhaseCreate, guestFailure(res), out)
	}
}

// closer is an instance as discardUnstarted closes it.
type closer interface {
	Close(ctx context.Context) error
}

// discardUnstarted closes an instance whose start a panic is unwinding, as the pool closes a discarded instance:
// under an ended context, so without the plugin's Close, which whatever panicked may leave unable to end. The
// panic's own report is the pool's; a failure closing is logged, as the pool logs it.
func (l *launcher) discardUnstarted(ctx context.Context, inst closer) {
	ended, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cancel()

	err := inst.Close(ended)
	if err != nil {
		l.output.LogAttrs(ctx, slog.LevelWarn, "plg: closing a discarded instance failed",
			slog.String("key", l.key), slog.String("error", err.Error()))
	}
}

// failed reports a step (Create, Close) the guest failed, with what it wrote to stderr meanwhile.
func (l *launcher) failed(ctx context.Context, phase Phase, value any, out *guestout.Capture) *PanicError {
	failure := &PanicError{Key: l.key, Language: l.lang, Phase: phase, Value: value, Guest: out.Guest()}
	l.panics.report(ctx, failure)

	return failure
}

// guestFailure is the Value of a PanicError for a guest's panic or ABI status: a panic's message, or the
// status and its message.
func guestFailure(res engine.Result) any {
	if res.Status == wire.StatusPanic {
		return res.Message
	}

	return fmt.Errorf("%w %s: %s", errGuestStatus, res.Status, res.Message)
}

// guestInstance is a guest instance and the capture of its output: each step (call, close) begins and ends a
// capture step, so that a failure reports what the guest wrote to stderr meanwhile.
type guestInstance struct {
	*engine.Instance

	launcher *launcher
	out      *guestout.Capture
}

// Start starts a call on a multiplexing guest, capturing the step's output like a call's.
func (g *guestInstance) Start(ctx context.Context, call, method uint32, args []byte) (engine.Step, error) {
	g.out.Begin()

	step, err := g.Instance.Start(ctx, call, method, args)
	g.out.End(ctx)

	return step, err //nolint:wrapcheck // the instance's errors are wrapped where they become call errors
}

// Resume delivers an HTTP answer to a multiplexing guest, capturing the step's output like a call's.
func (g *guestInstance) Resume(ctx context.Context, request uint32, answer []byte) (engine.Step, error) {
	g.out.Begin()

	step, err := g.Instance.Resume(ctx, request, answer)
	g.out.End(ctx)

	return step, err //nolint:wrapcheck // the instance's errors are wrapped where they become call errors
}

func (g *guestInstance) Call(ctx context.Context, method uint32, args []byte) (engine.Result, error) {
	g.out.Begin()

	res, err := g.Instance.Call(ctx, method, args)
	g.out.End(ctx)

	return res, err //nolint:wrapcheck // the instance's errors are wrapped where they become call errors
}

// Close closes the instance, running the plugin's optional Close. A Close that returns an error (or throws) fails
// with that error; a guest that panics, traps, is stopped by ctx or breaks the ABI meanwhile fails with a reported
// [PanicError] in [PhaseClose].
func (g *guestInstance) Close(ctx context.Context) error {
	g.out.Begin()

	err := g.Instance.Close(ctx)
	g.out.End(ctx)

	if err == nil {
		return nil
	}

	value := any(err)

	if closeErr, ok := errors.AsType[*engine.CloseError](err); ok {
		if closeErr.Status == wire.StatusError {
			return err //nolint:wrapcheck // the pool wraps close errors with the instance
		}

		value = guestFailure(engine.Result{Status: closeErr.Status, Message: closeErr.Message})
	}

	return g.launcher.failed(ctx, PhaseClose, value, g.out)
}

// Guest returns what the guest wrote to stderr during its last step.
func (g *guestInstance) Guest() string {
	return g.out.Guest()
}
