package plg

import (
	"context"
	"errors"
	"fmt"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
)

// Warm compiles every guest linked into the program (guest/golang, ...) into the on-disk compilation cache, so
// that containers using that cache start without compiling them: a cold compile of the Go guest takes seconds,
// loading it from the cache a fraction of a second. Run it once where the cache will be used, e.g. in the
// Docker build of the service's image, through the service's own binary or `plg warm` (the plg command links
// every guest it ships):
//
//	RUN go run github.com/heynemann/go-plg/cmd/plg warm -cache-dir /var/cache/plg
//
// The containers must then use the same directory: [WithCompilationCacheDir] or the PLG_CACHE_DIR environment
// variable, resolved as [New] does. Of the options, only [WithCompilationCacheDir] and [WithLogger] (each
// compile is logged at Info, with its duration) apply. The cache is keyed by the guest module's content, the
// wazero version and the CPU features, so it serves services built against the same plg version on machines
// with the CPU of the warming one; anything else is an ordinary cold compile, never an error. Custom guests
// ([WithGoGuest]) are not linked, so Warm does not compile them.
//
// Unlike [New], Warm fails when the cache directory cannot be created or written, rather than compiling into
// memory, and it fails with [ErrGuestNotLinked] when the program links no guest.
func Warm(ctx context.Context, opts ...Option) error {
	cfg := containerOptions(opts)

	guests := guestreg.All()
	if len(guests) == 0 {
		return fmt.Errorf("plg: warm: %w: the program links no guest package", ErrGuestNotLinked)
	}

	eng, err := engine.NewOnDisk(engine.Options{Logger: cfg.logger, CacheDir: cfg.cacheDir})
	if err != nil {
		return fmt.Errorf("plg: warm: %w", err)
	}

	// The memory limit is not part of the cache key: every container runtime reads what this one compiles.
	rt := eng.NewRuntime(ctx, engine.RuntimeOptions{})

	errs := make([]error, 0, len(guests)+2)
	for _, guest := range guests {
		_, err = compileGuest(ctx, rt, Language(guest.Language), guest)
		errs = append(errs, err)
	}

	errs = append(errs, rt.Close(ctx), eng.Close(ctx))

	err = errors.Join(errs...)
	if err != nil {
		return fmt.Errorf("plg: warm: %w", err)
	}

	return nil
}
