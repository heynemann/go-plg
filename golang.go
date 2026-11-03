package plg

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"path"
	"strings"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/guestout"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/wire"
)

// goGuestImport is the package that links the Go guest.
const goGuestImport = "github.com/heynemann/go-plg/guest/golang"

// guestFor returns the linked guest that runs lang plugins.
func guestFor(lang Language) (guestreg.Guest, error) {
	if lang != LanguageGo {
		return guestreg.Guest{}, fmt.Errorf("%w %q", ErrUnsupportedLanguage, lang)
	}

	guest, ok := guestreg.Lookup(string(lang))
	if !ok {
		return guestreg.Guest{}, fmt.Errorf("%w: %s plugins need the program to import _ %q",
			ErrGuestNotLinked, lang, goGuestImport)
	}

	return guest, nil
}

var (
	errEntrypointPath = errors.New("entrypoint is not a valid path")
	errEntrypointFile = errors.New("entrypoint is neither a directory nor a .go file")
)

// goSourceTree returns the tree a Go plugin instance mounts at [goabi.GoPath]: the contract package's source
// at its import path, the plugin's package directory at [goabi.PluginImportPath] and the dispatcher at
// [goabi.DispatchImportPath]. It rejects plugin source containing go statements.
func goSourceTree(contr *contract, dispatcher []byte, files fs.FS, entrypoint string) (*memfs.FS, error) {
	dir, err := goPackageDir(files, entrypoint)
	if err != nil {
		return nil, err
	}

	err = checkNoGoStatements(files, dir)
	if err != nil {
		return nil, err
	}

	var tree memfs.Builder

	err = tree.AddTree(path.Join("src", contr.iface.PkgPath()), contr.source, ".")
	if err == nil {
		err = tree.AddTree(path.Join("src", goabi.PluginImportPath), files, dir)
	}

	if err == nil {
		err = tree.Add(path.Join("src", goabi.DispatchImportPath, "dispatch.go"), dispatcher)
	}

	if err != nil {
		return nil, fmt.Errorf("copying sources: %w", err)
	}

	mounted, err := tree.Build()
	if err != nil {
		return nil, fmt.Errorf("copying sources: %w", err)
	}

	return mounted, nil
}

// goPackageDir returns the plugin's package directory: the entrypoint itself, or the directory of an
// entrypoint .go file.
func goPackageDir(files fs.FS, entrypoint string) (string, error) {
	if !fs.ValidPath(entrypoint) {
		return "", fmt.Errorf("%w: %q", errEntrypointPath, entrypoint)
	}

	info, err := fs.Stat(files, entrypoint)
	if err != nil {
		return "", fmt.Errorf("entrypoint: %w", err)
	}

	switch {
	case info.IsDir():
		return entrypoint, nil
	case info.Mode().IsRegular() && path.Ext(entrypoint) == ".go":
		return path.Dir(entrypoint), nil
	default:
		return "", fmt.Errorf("%w: %q", errEntrypointFile, entrypoint)
	}
}

// checkNoGoStatements parses every regular .go file below dir except tests, the source a Go plugin instance
// receives (symbolic links are not copied into it), and fails with [ErrGoStatement] naming the position
// (file:line:column, the file relative to the plugin's fs.FS) of every go statement. A file that does not parse
// fails the load as the guest would.
func checkNoGoStatements(files fs.FS, dir string) error {
	fset := token.NewFileSet()

	var found []string

	err := fs.WalkDir(files, dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() || path.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			return err
		}

		src, err := fs.ReadFile(files, name)
		if err != nil {
			return fmt.Errorf("reading the plugin source: %w", err)
		}

		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("%w: %w", errGuestLoad, err)
		}

		ast.Inspect(file, func(node ast.Node) bool {
			if stmt, ok := node.(*ast.GoStmt); ok {
				found = append(found, fset.Position(stmt.Go).String())
			}

			return true
		})

		return nil
	})
	if err != nil {
		return err //nolint:wrapcheck // the callback's errors are wrapped already; WalkDir adds the path to its own
	}

	if len(found) > 0 {
		return fmt.Errorf("%w: %s", ErrGoStatement, strings.Join(found, ", "))
	}

	return nil
}

var (
	errCreate      = errors.New("plugin Create failed")
	errGuestLoad   = errors.New("plugin source failed to load")
	errGuestStatus = errors.New("guest status")
)

// goLauncher starts the instances of one Go plugin.
type goLauncher struct {
	mod     *engine.Module
	logger  *slog.Logger
	panics  *panicReporter
	opts    engine.InstanceOptions
	key     string
	payload []byte
}

// start starts an instance, mounting the plugin's source tree, and runs the plugin's Create. A guest that
// panics, traps or breaks the ABI meanwhile fails with a reported [PanicError] in [PhaseCreate].
//
//nolint:ireturn // it is the pool's spawn function, which returns the pool's instance interface
func (l *goLauncher) start(ctx context.Context) (instance, error) {
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
		return &goInstance{Instance: inst, launcher: l, out: out}, nil
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
func (l *goLauncher) failed(ctx context.Context, phase Phase, value any, out *guestout.Capture) *PanicError {
	failure := &PanicError{Key: l.key, Language: LanguageGo, Phase: phase, Value: value, Guest: out.Guest()}
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

// goInstance is a Go guest instance and the capture of its output: each step (call, close) begins and ends a
// capture step, so that a failure reports what the guest wrote to stderr meanwhile.
type goInstance struct {
	*engine.Instance

	launcher *goLauncher
	out      *guestout.Capture
}

func (g *goInstance) Call(ctx context.Context, method uint32, args []byte) (engine.Result, error) {
	g.out.Begin()

	res, err := g.Instance.Call(ctx, method, args)
	g.out.End(ctx)

	return res, err //nolint:wrapcheck // the instance's errors are wrapped where they become call errors
}

// Close closes the instance, running the plugin's optional Close. A Close that returns an error fails with that
// error; a guest that panics, traps, is stopped by ctx or breaks the ABI meanwhile fails with a reported
// [PanicError] in [PhaseClose].
func (g *goInstance) Close(ctx context.Context) error {
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
func (g *goInstance) Guest() string {
	return g.out.Guest()
}
