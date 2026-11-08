//go:build integration

package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/cli"
	"github.com/heynemann/go-plg/internal/testproxy"
)

// devPrompt is what plg dev prints when it waits.
const devPrompt = "plg dev> "

// untilPrompt reads what plg dev prints up to its next prompt; at the end of its output it fails the test.
func untilPrompt(t *testing.T, out *bufio.Reader) string {
	t.Helper()

	var read strings.Builder

	for !strings.HasSuffix(read.String(), devPrompt) {
		b, err := out.ReadByte()
		require.NoError(t, err, "plg dev stopped; it printed:\n"+read.String())
		read.WriteByte(b)
	}

	return read.String()
}

// TestDevRevalidatesOnChangesAndCallsMethods runs plg dev on a plugin plg init wrote, with an injected watcher and
// stdin: it validates the plugin (watching its directory), validates it again when the watcher reports a change
// (reusing the contract module it resolved: the go command is gone by then) and shows the test case the change
// broke; call runs one method with an input file and prints its result, or why it could not (the plugin's error on
// one line, its escape sequence neutralized); the end of stdin stops it.
func TestDevRevalidatesOnChangesAndCallsMethods(t *testing.T) {
	t.Parallel()

	start := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	root := t.TempDir()

	created := plgInEnv(start, root, "init", "-implements", pricingRule, "-lang", "lua", "pricing")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)

	var environ atomic.Pointer[[]string]

	environ.Store(&start)

	watched := make(chan []string, 1)
	changes := make(chan []string)
	stdin, typed := io.Pipe()
	printed, stdout := io.Pipe()
	done := make(chan int, 1)

	go func() {
		defer func() { assert.NoError(t, stdout.Close()) }()

		done <- cli.Main([]string{"dev", "pricing"}, &cli.Env{
			Stdin: stdin, Stdout: stdout, Stderr: io.Discard,
			Environ: func() []string { return *environ.Load() }, Dir: root,
			Watch: func(_ context.Context, paths []string) <-chan []string {
				watched <- paths

				return changes
			},
		})
	}()

	out := bufio.NewReader(printed)
	plugin := filepath.Join(root, "pricing")

	first := untilPrompt(t, out)
	assert.Equal(t, []string{plugin}, <-watched)
	assert.Contains(t, rows(first), "tests ok 5 passed")
	assert.Contains(t, first, "plg dev: "+plugin+" is valid\n")

	environ.Store(&[]string{"PATH="}) // no go command from now on

	source := filepath.Join(plugin, "plugin.lua")
	replaceIn(t, plugin, "plugin.lua", "return { total = 0,", "return { total = 1,")

	changes <- []string{source}

	second := untilPrompt(t, out)
	assert.Contains(t, second, "changed: "+source+"\n")
	assert.Contains(t, second, "resolved by an earlier run)")
	assert.Contains(t, rows(second), "test FAIL tests/example.yaml: Quote returns the skeleton's zero values: Quote "+
		"returned other results than expect.result")
	assert.Contains(t, second, "plg dev: "+plugin+" is not a valid plugin\n")

	require.NoError(t, os.WriteFile(filepath.Join(root, "order.yaml"), []byte(
		"order: {customer: alice, lines: [{sku: tea, qty: 2, cents: 150}]}\n"), 0o600))

	_, err := io.WriteString(typed, "call Quote order.yaml\n")
	require.NoError(t, err)
	assert.Equal(t, "Quote: result {total: 1, discount: 0, reasons: null}\n"+devPrompt, untilPrompt(t, out))

	_, err = io.WriteString(typed, "call Split\n")
	require.NoError(t, err)
	assert.Contains(
		t,
		untilPrompt(t, out),
		`Split failed: call Split: input gives no "cents" (parameters: cents, parts)`,
	)

	_, err = io.WriteString(typed, "call Quote missing.yaml\n")
	require.NoError(t, err)
	assert.Contains(t, untilPrompt(t, out), "Quote failed: open missing.yaml: ")

	replaceIn(t, plugin, "plugin.lua", "return { total = 1,", `error("\27[2Jboom\nagain") return { total = 1,`)

	_, err = io.WriteString(typed, "call Quote order.yaml\n")
	require.NoError(t, err)

	failed := untilPrompt(t, out)
	assert.Regexp(t, "^Quote failed: .*\ufffd\\[2Jboom again\n"+devPrompt+"$", failed,
		"the plugin's error, on one line")
	assert.NotContains(t, failed, "\x1b", "the plugin's error does not drive the terminal")

	require.NoError(t, typed.Close())

	rest, err := io.ReadAll(out) // up to the end of plg dev's output
	require.NoError(t, err)
	assert.Equal(t, "\n", string(rest))
	assert.Equal(t, 0, <-done)
}

// TestDevUsage checks plg dev's arguments: one plugin directory at most, an author repository listing several
// plugins needs one named, and an invalid one fails.
func TestDevUsage(t *testing.T) {
	t.Parallel()

	ran := plgInEnv(nil, t.TempDir(), "dev", "a", "b")
	assert.Equal(t, 2, ran.code)
	assert.Contains(t, ran.stderr, "plg dev: name at most one plugin directory")

	ran = plgInEnv(nil, t.TempDir(), "dev", "-bogus")
	assert.Equal(t, 2, ran.code)
	assert.Contains(t, ran.stderr, "plg dev: flag provided but not defined: -bogus")

	ran = plgInEnv(nil, filepath.Join("..", "..", "testdata", "validate", "repo"), "dev")
	assert.Equal(t, 2, ran.code)
	assert.Contains(t, ran.stderr, "lists 3 plugins: name one (gold, rules/silver, coupon)")

	invalid := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(invalid, "plg.yaml"), []byte("plugins: []\n"), 0o600))

	ran = plgInEnv(nil, invalid, "dev")
	assert.Equal(t, 1, ran.code)
	assert.Contains(t, ran.stderr, "plg dev: invalid plg.yaml: ")
}

// devSession is plg dev running in the background with an injected watcher and stdin.
type devSession struct {
	t       *testing.T
	typed   *io.PipeWriter
	out     *bufio.Reader
	changes chan []string
	watched chan []string
	done    chan int
}

// startDev starts plg dev with args from dir in the environment environ.
func startDev(t *testing.T, environ []string, dir string, args ...string) *devSession {
	t.Helper()

	stdin, typed := io.Pipe()
	printed, stdout := io.Pipe()
	session := &devSession{
		t: t, typed: typed, out: bufio.NewReader(printed), changes: make(chan []string),
		watched: make(chan []string, 1), done: make(chan int, 1),
	}

	go func() {
		defer func() { assert.NoError(t, stdout.Close()) }()

		session.done <- cli.Main(append([]string{"dev"}, args...), &cli.Env{
			Stdin: stdin, Stdout: stdout, Stderr: io.Discard,
			Environ: func() []string { return environ }, Dir: dir,
			Watch: func(_ context.Context, paths []string) <-chan []string {
				session.watched <- paths

				return session.changes
			},
		})
	}()

	return session
}

// command types line and returns what plg dev prints up to its next prompt.
func (s *devSession) command(line string) string {
	s.t.Helper()

	_, err := io.WriteString(s.typed, line+"\n")
	require.NoError(s.t, err)

	return untilPrompt(s.t, s.out)
}

// TestDevInAnAuthorRepository runs plg dev on an author repository listing one Go plugin, which it picks: it
// watches the plugin, plg.yaml and the shared directory; empty lines do nothing, an unknown command lists the
// commands, run validates again, call runs Validate in the Go guest, and a call once plugin.yaml is broken shows the
// step that failed; quit stops it.
func TestDevInAnAuthorRepository(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	repo := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(repo, "lib"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "gold"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "plg.yaml"), []byte("plugins: [gold]\nshared: [lib]\n"),
		0o600))

	created := plgInEnv(environ, repo, "init", "-implements", pricingRule, "-lang", "go", "gold")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)

	dev := startDev(t, environ, repo)
	plugin := filepath.Join(repo, "gold")

	first := untilPrompt(t, dev.out)
	assert.Contains(t, first, "plg dev: "+plugin+" is valid\n")
	assert.Equal(t, []string{plugin, filepath.Join(repo, "plg.yaml"), filepath.Join(repo, "lib")}, <-dev.watched)

	assert.Equal(t, devPrompt, dev.command(""))
	assert.Equal(t, "commands: call <Method> [input file], run, quit\n"+devPrompt, dev.command("help"))
	assert.Equal(t, "commands: call <Method> [input file], run, quit\n"+devPrompt, dev.command("call a b c"))
	assert.Contains(t, dev.command("run"), "plg dev: "+plugin+" is valid\n")

	require.NoError(t, os.WriteFile(filepath.Join(repo, "limits.yaml"), []byte("in: {maxPercent: 5}\n"), 0o600))
	assert.Equal(t, "Validate: issues []\n"+devPrompt, dev.command("call Validate limits.yaml"))

	require.NoError(t, os.WriteFile(filepath.Join(plugin, "plugin.yaml"), []byte("language: go\n"), 0o600))

	broken := dev.command("call Validate limits.yaml")
	assert.Contains(
		t,
		strings.Join(rows(broken), "\n"),
		"manifest FAIL plg: invalid plugin.yaml: implements is required",
	)
	assert.Contains(t, broken, "Validate failed: a step failed\n")

	_, err := io.WriteString(dev.typed, "quit\n")
	require.NoError(t, err)

	rest, err := io.ReadAll(dev.out)
	require.NoError(t, err)
	assert.Empty(t, string(rest))
	assert.Equal(t, 0, <-dev.done)
	require.NoError(t, dev.typed.Close())
}

// TestDevStopsWhenTheWatcherDoes runs plg dev without stdin: it validates the plugin and stops once its watcher's
// channel closes.
func TestDevStopsWhenTheWatcherDoes(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	root := t.TempDir()

	created := plgInEnv(environ, root, "init", "-implements", pricingRule, "-lang", "lua", "rule")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)

	var stdout bytes.Buffer

	code := cli.Main([]string{"dev", "rule"}, &cli.Env{
		Stdout: &stdout, Stderr: io.Discard,
		Environ: func() []string { return environ }, Dir: root,
		Watch: func(context.Context, []string) <-chan []string {
			closed := make(chan []string)
			close(closed)

			return closed
		},
	})
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "plg dev: "+filepath.Join(root, "rule")+" is valid\n")
}

// TestDevStopsAtTheEndOfItsInput runs plg dev with its default watcher on a plugin in a repository that does not
// list it: it watches the plugin alone, validates it without the repository's settings, and stops at the end of
// stdin.
func TestDevStopsAtTheEndOfItsInput(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	root := t.TempDir()

	created := plgInEnv(environ, root, "init", "-implements", pricingRule, "-lang", "lua", "rule")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "other"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "plg.yaml"), []byte("plugins: [other]\n"), 0o600))

	var stdout bytes.Buffer

	code := cli.Main([]string{"dev", "-guest-cache", t.TempDir(), "rule"}, &cli.Env{
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: io.Discard,
		Environ: func() []string { return environ }, Dir: root,
	})
	assert.Equal(t, 0, code)
	assert.Contains(t, rows(stdout.String()), "plg.yaml warning "+filepath.Join(root, "plg.yaml")+" does not list "+
		"this plugin: it is validated without the repository's settings")
	assert.Contains(t, stdout.String(), "watching "+filepath.Join(root, "rule")+"\n")
}

// TestDevReusesTheCompilationCache runs plg dev with a compilation cache of its own: its first run compiles the
// guest into it, and the run after a save compiles nothing new (every cache file is the one the first run wrote,
// unchanged): warm guests are the on-disk compilation cache, reused across saves.
func TestDevReusesTheCompilationCache(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	root := t.TempDir()
	cache := t.TempDir()

	created := plgInEnv(environ, root, "init", "-implements", pricingRule, "-lang", "lua", "pricing")
	require.Equal(t, 0, created.code, created.stdout+created.stderr)

	changes := make(chan []string)
	stdin, typed := io.Pipe()
	printed, stdout := io.Pipe()
	done := make(chan int, 1)

	go func() {
		defer func() { assert.NoError(t, stdout.Close()) }()

		done <- cli.Main([]string{"dev", "-cache-dir", cache, "pricing"}, &cli.Env{
			Stdin: stdin, Stdout: stdout, Stderr: io.Discard,
			Environ: func() []string { return environ }, Dir: root,
			Watch: func(context.Context, []string) <-chan []string { return changes },
		})
	}()

	out := bufio.NewReader(printed)
	plugin := filepath.Join(root, "pricing")

	assert.Contains(t, untilPrompt(t, out), "plg dev: "+plugin+" is valid\n")

	compiled := cacheFiles(t, cache)
	require.NotEmpty(t, compiled, "the first run compiles the guest into the cache")

	replaceIn(t, plugin, "plugin.lua", "return { total = 0,", "return { total = 0 ,")

	changes <- []string{filepath.Join(plugin, "plugin.lua")}

	assert.Contains(t, untilPrompt(t, out), "plg dev: "+plugin+" is valid\n")
	assert.Equal(t, compiled, cacheFiles(t, cache), "the second run compiles nothing new")

	require.NoError(t, typed.Close())

	_, err := io.ReadAll(out)
	require.NoError(t, err)
	assert.Equal(t, 0, <-done)
}

// cacheFiles returns the compilation cache's files in dir with each one's size and modification time.
func cacheFiles(t *testing.T, dir string) map[string]string {
	t.Helper()

	files := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		info, err := entry.Info()
		if err != nil {
			return err //nolint:wrapcheck // the test fails with it
		}

		files[path] = strconv.FormatInt(info.Size(), 10) + " " + info.ModTime().String()

		return nil
	})
	require.NoError(t, err)

	return files
}
