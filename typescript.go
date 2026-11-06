package plg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/scriptabi"
	"github.com/heynemann/go-plg/internal/wire"
)

// The bundler's sandbox. esbuild can take exponential time on a few hundred bytes (`export *` layers re-walked
// for every path through them, TypeScript arrow functions re-parsed at every level of nesting), and nothing stops
// a Go function from outside, so plg never runs it on the host: the TypeScript guest links esbuild compiled to
// WebAssembly (guestsrc/tsbundle), and each load bundles in an instance of it, stopped when its time limit or
// Register's (or the reload's) context ends and limited in memory like a plugin instance.
const (
	// bundleTimeBase and bundleTimePerMiB make a bundling's time limit: the base plus the allowance for each MiB
	// of the plugin's files, imported or not. esbuild in WebAssembly bundles 0.35–0.6 MB per second here (a
	// 3.6 MB plugin in 9–11 s, a 600 KB one in 1 s, a small one in about 40 ms), 50 to 90 times slower than
	// natively: the allowance alone is nearly twice the slowest of those.
	bundleTimeBase   = 10 * time.Second
	bundleTimePerMiB = 5 * time.Second
	// bundleMemoryLimit caps the bundler instance's memory: the 3.6 MB plugin above took 71 MiB.
	bundleMemoryLimit = 512 << 20
	// mebibyte is the unit of bundleTimePerMiB.
	mebibyte = 1 << 20
	// bundlerID tells the bundler's compiled module apart from the TypeScript guest's.
	bundlerID = "bundler"
	// bundlerPluginPath is where the bundler finds the plugin's files (tsbundle.PluginPath).
	bundlerPluginPath = "/plugin"
	// bundlerStderr is how much of the bundler's stderr a failure reports: the Go runtime's fatal error, which
	// comes first.
	bundlerStderr = 512
)

var errBundle = errors.New("bundling the TypeScript plugin failed")

// tsLoader loads TypeScript plugins: esbuild bundles the entrypoint and the files it imports, from the plugin's
// files, in the bundler guest, and the QuickJS guest evaluates the bundle. The guest needs no mount: the bundle
// is the whole plugin.
type tsLoader struct{}

func (tsLoader) guestImport() string { return "github.com/heynemann/go-plg/guest/typescript" }

func (tsLoader) memoryLimit() uint64 { return scriptMemoryLimit }

func (tsLoader) multiplexes() bool { return true }

func (tsLoader) missingValidate(*memfs.FS, string, *validation) string {
	return "the object Create returned has no method Validate: inst.Validate(v) returns an array of issues " +
		"({severity, code, message, field}), or a Promise of one"
}

func (tsLoader) prepare(ctx context.Context, c *Container, reg *registration, src *source, files *memfs.FS) (
	launchSpec, error,
) {
	bundle, err := c.bundle(ctx, src.guest, files, src.desc.Entrypoint)
	if err != nil {
		return launchSpec{}, err
	}

	payload := scriptabi.Payload{
		Name:     src.desc.Entrypoint,
		Source:   bundle,
		Methods:  uint32(len(reg.contr.methods)), //nolint:gosec // G115: an interface has far fewer than 2^32 methods
		Schema:   reg.schema,
		Validate: reg.validation != nil,
	}

	return launchSpec{payload: payload.Append(nil)}, nil
}

// bundle bundles the TypeScript plugin whose entrypoint is entrypoint inside files in an instance of guest's
// bundler, within the time limit of files' size (see bundleTimeBase) and the bundler's memory limit. It fails
// with the plugin's bundling errors, with ctx's cause once ctx ends, and when the bundler runs out of time or
// memory.
func (c *Container) bundle(ctx context.Context, guest guestreg.Guest, files *memfs.FS, entrypoint string) (
	string, error,
) {
	bundler := guestreg.Guest{Language: guest.Language, ID: bundlerID, Wasm: guest.Bundler}

	mod, err := c.module(ctx, LanguageTypeScript, bundler, c.bundleMemory)
	if err != nil {
		return "", err
	}

	limit := c.bundleTimeBase + time.Duration(files.Size())*bundleTimePerMiB/mebibyte

	run, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	stderr := &headWriter{max: bundlerStderr}

	inst, err := mod.Instantiate(run, &engine.InstanceOptions{
		Stderr: stderr,
		Mounts: []engine.Mount{{FS: files, GuestPath: bundlerPluginPath}},
	})
	if err != nil {
		return "", bundleFailure(ctx, run, limit, err, stderr)
	}

	res, err := inst.Init(run, []byte(entrypoint))

	var bundle string

	if err == nil && res.Status == wire.StatusOK {
		bundle = string(res.Body) // res.Body is the instance's memory, which closing it frees
	}

	err = errors.Join(err, inst.Close(context.WithoutCancel(ctx)))

	switch {
	case err != nil:
		return "", bundleFailure(ctx, run, limit, err, stderr)
	case res.Status != wire.StatusOK: // esbuild's errors (StatusLoad)
		return "", fmt.Errorf("%w: %s", errBundle, res.Message)
	default:
		return bundle, nil
	}
}

// bundleFailure reports why the bundler instance run under run, derived from ctx with the time limit limit,
// stopped with err.
func bundleFailure(ctx, run context.Context, limit time.Duration, err error, stderr *headWriter) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("bundling the TypeScript plugin: %w", context.Cause(ctx))
	case run.Err() != nil:
		return fmt.Errorf("%w: bundling took longer than its time limit, %s", errBundle, limit.Round(time.Millisecond))
	default: // the Go runtime's fatal error first (out of memory), then wazero's, without its wasm stack trace
		report, _, _ := strings.Cut(err.Error(), "\n")
		if line, _, _ := bytes.Cut(stderr.buf, []byte("\n")); len(line) > 0 {
			report = string(line) + " (" + report + ")"
		}

		return fmt.Errorf("%w: the bundler stopped: %s", errBundle, report)
	}
}

// headWriter keeps the first max bytes written to it.
type headWriter struct {
	buf []byte
	max int
}

func (w *headWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p[:min(len(p), w.max-len(w.buf))]...)

	return len(p), nil
}
