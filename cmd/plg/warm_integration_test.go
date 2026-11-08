//go:build integration

package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveSrc is a service's program: on the compilation cache in os.Args[1], it registers Greeter with one Go
// plugin and calls it, logging as JSON to stdout (the guest compile included).
const serveSrc = `package main

import (
	"context"
	"log/slog"
	"os"
	"testing/fstest"

	"github.com/heynemann/go-plg"
	_ "github.com/heynemann/go-plg/guest/golang"

	"example.com/svc/contract"
)

const plugin = "package greeter\n\nimport (\n\t\"context\"\n\n\t\"example.com/svc/contract\"\n)\n\n" +
	"type greeter struct{}\n\nfunc Create() (contract.Greeter, error) { return greeter{}, nil }\n\n" +
	"func (greeter) Greet(_ context.Context, name string) (string, error) { return \"hi \" + name, nil }\n"

func main() {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	c := plg.New(plg.WithCompilationCacheDir(os.Args[1]), plg.WithLogger(logger))

	store := plg.NewMemoryStorage("mem")
	err := store.Put(&plg.Descriptor{
		Key: "g", Implements: "example.com/svc/contract.Greeter@v1.0.0", Language: plg.LanguageGo,
		Entrypoint: "plugin.go",
	}, fstest.MapFS{"plugin.go": {Data: []byte(plugin)}})
	if err == nil {
		err = plg.Register[contract.Greeter](ctx, c, "greeter", plg.WithStorage(store), plg.WithPool(1, 1))
	}
	var greeting string
	if err == nil {
		var g contract.Greeter
		g, err = plg.Get[contract.Greeter](ctx, c, "greeter/g")
		if err == nil {
			greeting, err = g.Greet(ctx, "warm")
		}
	}
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	logger.Info("greeted", slog.String("greeting", greeting))
	if err := c.Close(ctx); err != nil {
		os.Exit(1)
	}
}
`

// TestWarmSparesAFreshProcessTheColdCompile runs `plg warm` into an empty cache directory the way a Docker build
// would, then a fresh service process on that directory: it must load the Go guest from the cache (no entry
// written, a fraction of the cold compile) and serve its plugin. Both processes are built without the race
// detector, so the timings are the ones a service sees.
func TestWarmSparesAFreshProcessTheColdCompile(t *testing.T) {
	t.Parallel()

	dir := generate(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cmd", "serve"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd", "serve", "main.go"), []byte(serveSrc), 0o600))

	serve := filepath.Join(t.TempDir(), "serve")
	out, err := goCmd(t, dir, "build", "-o", serve, "./cmd/serve")
	require.NoError(t, err, out)

	cache := filepath.Join(t.TempDir(), "plg-cache")

	out, err = goCmd(t, dir, "run", "github.com/heynemann/go-plg/cmd/plg", "warm", "-cache-dir", cache)
	require.NoError(t, err, out)

	cold := compileDuration(t, out, `guest=go duration=(\S+)`, time.ParseDuration)
	warmed := cacheEntries(t, cache)
	require.NotEmpty(t, warmed, "warm persisted the compiled guests")

	//nolint:gosec // G204: runs the program built above
	served, err := exec.CommandContext(t.Context(), serve, cache).Output()
	require.NoError(t, err, string(served))
	assert.Contains(t, string(served), `"msg":"greeted","greeting":"hi warm"`)

	warm := compileDuration(t, string(served), `"msg":"plg: guest compiled","guest":"go","duration":(\d+)`,
		func(s string) (time.Duration, error) {
			ns, err := strconv.ParseInt(s, 10, 64)

			return time.Duration(ns), err
		})

	t.Logf("go guest compile: plg warm (cold) %v, fresh process on the warmed cache %v", cold, warm)
	assert.Equal(t, warmed, cacheEntries(t, cache), "a cache hit writes no entry; a cold compile would")
	assert.Less(t, warm, time.Second, "a warm cache loads well under a second")
	assert.Less(t, warm*5, cold, "the fresh process did not compile cold")
}

// compileDuration finds the guest compile's duration in a process's log.
func compileDuration(
	t *testing.T, log, pattern string, parse func(string) (time.Duration, error),
) time.Duration {
	t.Helper()

	m := regexp.MustCompile(pattern).FindStringSubmatch(log)
	require.NotNil(t, m, "the compile is logged: %s", log)

	d, err := parse(m[1])
	require.NoError(t, err)

	return d
}

// cacheEntries returns the files below a compilation cache directory with their sizes and modification times.
func cacheEntries(t *testing.T, dir string) map[string]string {
	t.Helper()

	entries := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("cache entry %s: %w", path, err)
		}

		entries[path] = strconv.FormatInt(info.Size(), 10) + " " + info.ModTime().String()

		return nil
	})
	require.NoError(t, err)

	return entries
}
