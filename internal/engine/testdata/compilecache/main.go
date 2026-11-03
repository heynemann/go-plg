// Command compilecache compiles the Go guest through the on-disk compilation cache in os.Args[1] and logs
// the compile, as JSON, to stdout. TestCompileCacheAcrossProcesses runs it twice: a fresh process per load,
// built without the race detector, so the timings are the ones a service sees.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	_ "github.com/heynemann/go-plg/guest/golang"
	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
)

func main() {
	if err := run(os.Args[1]); err != nil {
		slog.Error("compilecache failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(dir string) error {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	e := engine.New(ctx, engine.Options{CacheDir: dir, Logger: logger})
	defer e.Close(ctx)

	if e.CacheDir() != dir {
		return errors.New("the cache directory is unusable")
	}

	guest, _ := guestreg.Lookup("go")

	wasm, err := guest.Wasm()
	if err != nil {
		return err
	}

	rt := e.NewRuntime(ctx, engine.RuntimeOptions{MemoryLimit: 128 << 20})
	defer rt.Close(ctx)

	_, err = rt.Compile(ctx, "go", wasm)

	return err
}
