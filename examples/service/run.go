package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/examples/plugins"
)

// Exit codes of run.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

// run parses the command line, then serves until ctx ends; it returns the process's exit code.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("service", flag.ContinueOnError)
	flags.SetOutput(stderr)

	addr := flags.String("addr", "localhost:8080", "address to listen on")
	dir := flags.String("plugins", "", "directory of plugins to serve instead of the embedded ones")
	poll := flags.Duration("poll", 0, "how often to reload -plugins (0: never)")

	err := flags.Parse(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}

		return exitUsage
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))

	err = listenAndServe(ctx, *addr, config{storage: storageFor(*dir), poll: *poll, logger: logger})
	if err != nil {
		logger.ErrorContext(ctx, "service failed", slog.Any("error", err))

		return exitFailed
	}

	return exitOK
}

// storageFor returns the storage of the plugins in dir, or of the embedded plugins when dir is empty.
func storageFor(dir string) *plg.FSStorage {
	if dir == "" {
		return plg.NewFSStorageFromFS("embedded", plugins.FS)
	}

	return plg.NewFSStorage(dir)
}

func listenAndServe(ctx context.Context, addr string, cfg config) error {
	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err //nolint:wrapcheck // net's error names the address
	}

	return serve(ctx, listener, cfg)
}

// config is what serve needs besides its listener.
type config struct {
	storage plg.Storage
	logger  *slog.Logger
	poll    time.Duration
}
