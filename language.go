package plg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestout"
	"github.com/heynemann/go-plg/internal/guestreg"
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
	// prepare turns an opened plugin's files into what each of its instances starts from: the init payload and
	// the file trees mounted read-only into the guest. It fails when the plugin cannot load (a missing
	// entrypoint, source the host rejects).
	prepare(reg *registration, files fs.FS, entrypoint string) (launchSpec, error)
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
	mod     *engine.Module
	logger  *slog.Logger
	panics  *panicReporter
	opts    engine.InstanceOptions
	key     string
	lang    Language
	payload []byte
}

// start starts an instance, mounting the plugin's files, and runs the plugin's Create. A guest that panics,
// traps or breaks the ABI meanwhile fails with a reported [PanicError] in [PhaseCreate].
//
//nolint:ireturn // it is the pool's spawn function, which returns the pool's instance interface
func (l *launcher) start(ctx context.Context) (instance, error) {
	out := guestout.New(l.logger, l.key)
	opts := l.opts
	opts.Stdout, opts.Stderr = out.Stdout(), out.Stderr()

	out.Begin()

	inst, err := l.mod.Instantiate(ctx, opts)
	if err != nil {
		out.End(ctx)

		return nil, fmt.Errorf("starting an instance: %w", err)
	}

	res, err := inst.Init(ctx, l.payload)
	out.End(ctx)

	switch {
	case err != nil:
		err = l.failed(ctx, PhaseCreate, err, out)
	case res.Status == wire.StatusOK:
		return &guestInstance{Instance: inst, launcher: l, out: out}, nil
	case res.Status == wire.StatusError:
		err = fmt.Errorf("%w: %s", errCreate, res.Message)
	case res.Status == wire.StatusLoad:
		err = fmt.Errorf("%w: %s", errGuestLoad, res.Message)
	default:
		err = l.failed(ctx, PhaseCreate, guestFailure(res), out)
	}

	return nil, errors.Join(err, inst.Close(context.WithoutCancel(ctx)))
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
