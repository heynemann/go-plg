// Package engine runs guests in wazero: the compilation cache, runtimes, compiled modules and the instances
// that speak the plg guest ABI.
//
// An [Engine] belongs to one container and owns the compilation cache shared by all of its runtimes.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"

	"github.com/tetratelabs/wazero"
)

const (
	wazeroModule = "github.com/tetratelabs/wazero"
	cacheDirPerm = 0o750

	// CacheDirEnv names the environment variable that, when set, is the default compilation cache directory.
	CacheDirEnv = "PLG_CACHE_DIR"
)

// Options configure an [Engine].
type Options struct {
	// Logger receives cache warnings and compile timings; nil discards them.
	Logger *slog.Logger
	// CacheDir is the on-disk compilation cache directory; empty means [DefaultCacheDir].
	CacheDir string
}

// Engine owns a compilation cache. It is safe for concurrent use.
type Engine struct {
	cache    wazero.CompilationCache
	logger   *slog.Logger
	cacheDir string
}

// New returns an engine whose compilation cache lives on disk in opts.CacheDir (or [DefaultCacheDir]). When
// that directory cannot be resolved, created or written, it logs a warning and falls back to an in-memory
// cache: a cold compile is slow, never fatal.
func New(ctx context.Context, opts Options) *Engine {
	primeWazeroVersion()

	eng := &Engine{logger: orDiscardLogger(opts.Logger)}

	err := eng.openDiskCache(opts.CacheDir)
	if err != nil {
		eng.cache = wazero.NewCompilationCache()
		eng.logger.LogAttrs(ctx, slog.LevelWarn,
			"plg: compilation cache directory unusable; using an in-memory cache",
			slog.String("dir", opts.CacheDir), slog.String("error", err.Error()))
	}

	return eng
}

// NewOnDisk is [New] for the callers whose point is the on-disk cache, like warming it: a directory that cannot
// be resolved, created or written is an error rather than a fallback to memory.
func NewOnDisk(opts Options) (*Engine, error) {
	primeWazeroVersion()

	eng := &Engine{logger: orDiscardLogger(opts.Logger)}

	err := eng.openDiskCache(opts.CacheDir)
	if err != nil {
		return nil, err
	}

	return eng, nil
}

func orDiscardLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.New(slog.DiscardHandler)
	}

	return logger
}

// primeWazeroVersion works around a data race in wazero v1.12.0: its internal/version package caches the
// module version in an unsynchronized variable the first time a compilation cache or compiler runtime is
// created, so the first concurrent creations race. Resolving it once, before any engine touches wazero, leaves
// only reads. A NUL byte makes the directory invalid on every platform, so the call fails without touching
// the disk; wazero resolves its version before trying, which is all this is for.
//
//nolint:gochecknoglobals // process-wide one-time workaround, see above
var primeWazeroVersion = sync.OnceFunc(func() {
	_, err := wazero.NewCompilationCacheWithDir("\x00")
	if err == nil {
		panic("engine: wazero accepted an invalid cache directory")
	}
})

// CacheDir returns the on-disk cache directory in use, or "" when the cache is in memory.
func (e *Engine) CacheDir() string {
	return e.cacheDir
}

// Close releases the compilation cache. Runtimes must be closed first.
func (e *Engine) Close(ctx context.Context) error {
	err := e.cache.Close(ctx)
	if err != nil {
		return fmt.Errorf("closing compilation cache: %w", err)
	}

	return nil
}

// WazeroVersion returns the wazero module version linked into the binary, or "dev" when the binary carries no
// module information.
func WazeroVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}

	return depVersion(info.Deps, wazeroModule)
}

func depVersion(deps []*debug.Module, path string) string {
	for _, dep := range deps {
		if dep.Path != path {
			continue
		}

		if dep.Replace != nil && dep.Replace.Version != "" {
			return dep.Replace.Version
		}

		if dep.Version != "" && dep.Version != "(devel)" {
			return dep.Version
		}
	}

	return "dev"
}

// DefaultCacheDir returns the directory named by the PLG_CACHE_DIR environment variable ([CacheDirEnv]) when it
// is set, otherwise os.UserCacheDir()/plg/<wazero version>.
func DefaultCacheDir() (string, error) {
	if dir := os.Getenv(CacheDirEnv); dir != "" {
		return dir, nil
	}

	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolving the user cache directory: %w", err)
	}

	return filepath.Join(dir, "plg", WazeroVersion()), nil
}

// openDiskCache opens the engine's on-disk compilation cache in dir, or in [DefaultCacheDir] when dir is empty.
func (e *Engine) openDiskCache(dir string) error {
	if dir == "" {
		def, err := DefaultCacheDir()
		if err != nil {
			return err
		}

		dir = def
	}

	err := probeWritable(dir)
	if err != nil {
		return fmt.Errorf("compilation cache directory %s: %w", dir, err)
	}

	e.cache, err = wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return fmt.Errorf("compilation cache directory %s: %w", dir, err)
	}

	e.cacheDir = dir

	return nil
}

// probeWritable creates dir and a temporary file in it.
func probeWritable(dir string) error {
	err := os.MkdirAll(dir, cacheDirPerm)
	if err != nil {
		return fmt.Errorf("creating cache directory: %w", err)
	}

	f, err := os.CreateTemp(dir, ".plg-probe-*")
	if err != nil {
		return fmt.Errorf("writing cache directory: %w", err)
	}

	return errors.Join(f.Close(), os.Remove(f.Name()))
}
