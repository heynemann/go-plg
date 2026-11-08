package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/authorrepo"
	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/modfetch"
	"github.com/heynemann/go-plg/internal/offline"
	"github.com/heynemann/go-plg/internal/testcase"
)

// devPrompt is what plg dev prints when it waits for a command or a change.
const devPrompt = "plg dev> "

// pollInterval is how often plg dev's default watcher looks at the files.
const pollInterval = 250 * time.Millisecond

// errSteps reports a call that could not run because a step before it failed; the steps' rows say which.
var errSteps = errors.New("a step failed")

// modules keeps the contract modules plg dev resolved, by pin, so that later runs reuse them without the go
// command. A nil modules keeps none. Only plg dev's goroutine uses it.
type modules struct {
	resolved map[contractref.Ref]modfetch.Module
}

func (m *modules) find(ref contractref.Ref) (modfetch.Module, bool) {
	if m == nil {
		return modfetch.Module{}, false
	}

	mod, ok := m.resolved[ref]

	return mod, ok
}

func (m *modules) keep(ref contractref.Ref, mod modfetch.Module) {
	if m != nil {
		m.resolved[ref] = mod
	}
}

// runDev runs `plg dev`.
func runDev(env *Env, args []string) outcome {
	flags := flag.NewFlagSet("dev", flag.ContinueOnError)
	host := flags.String("host", "", "the contract's Host interface the plugin's Create takes")
	cacheDir := flags.String("cache-dir", "", "compilation cache directory")
	guestCache := flags.String("guest-cache", "", "directory of the custom Go guests built for contracts' "+
		"//plg:guest-packages (default <user cache>/plg/guests)")

	operands, res, ok := parseMixed("dev", flags, args)
	if !ok {
		return res
	}

	if len(operands) > 1 {
		return usageError("dev", "name at most one plugin directory")
	}

	arg := "."
	if len(operands) == 1 {
		arg = operands[0]
	}

	arg, res, ok = devPlugin(env, arg)
	if !ok {
		return res
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	session := &devSession{
		env: env, base: validation{
			env: env, dir: resolve(env.Dir, arg), arg: arg, host: *host, cacheDir: resolve(env.Dir, *cacheDir),
			modules: &modules{resolved: map[contractref.Ref]modfetch.Module{}},
		},
	}

	if *guestCache != "" {
		session.base.guestCache = resolve(env.Dir, *guestCache)
	}

	return session.loop(ctx)
}

// devPlugin returns the plugin directory plg dev runs on, as named on the command line: arg, or the only plugin of
// the author repository arg is.
func devPlugin(env *Env, arg string) (string, outcome, bool) {
	dir := resolve(env.Dir, arg)

	if exists(filepath.Join(dir, "plugin.yaml")) || !authorrepo.Exists(dir) {
		return arg, outcome{}, true
	}

	repo, err := authorrepo.Load(dir)
	if err != nil {
		return "", failure("dev", err), false
	}

	if len(repo.Plugins) != 1 {
		return "", usageError("dev", fmt.Sprintf("%s lists %d plugins: name one (%s)", repo.File(),
			len(repo.Plugins), strings.Join(repo.Plugins, ", "))), false
	}

	return filepath.Join(arg, filepath.FromSlash(repo.Plugins[0])), outcome{}, true
}

// devSession is a run of plg dev: base is what every validation starts from.
type devSession struct {
	env  *Env
	base validation
}

// loop validates the plugin, then again on each change and as the commands read from stdin say, until stdin ends,
// a quit command or ctx ends.
func (s *devSession) loop(ctx context.Context) outcome {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	watch := s.env.Watch
	if watch == nil {
		watch = pollWatch
	}

	paths := s.watched()
	changes := watch(ctx, paths)
	lines := readLines(ctx, s.env.Stdin)

	s.validate(ctx)
	s.printf("watching %s\n", strings.Join(paths, ", "))
	s.help()

	for {
		s.printf("%s", devPrompt)

		select {
		case <-ctx.Done():
			return outcome{code: exitOK}
		case changed, open := <-changes:
			if !open {
				return outcome{code: exitOK}
			}

			s.printf("\nchanged: %s\n", strings.Join(changed, ", "))
			s.validate(ctx)
		case line, open := <-lines:
			if !open {
				s.printf("\n")

				return outcome{code: exitOK}
			}

			if !s.command(ctx, line) {
				return outcome{code: exitOK}
			}
		}
	}
}

// watched returns the paths plg dev watches: the plugin directory (its tests among its files) and, when an author
// repository lists it, the repository's plg.yaml and shared directories.
func (s *devSession) watched() []string {
	paths := []string{s.base.dir}

	repo, err := authorrepo.Find(s.base.dir)
	if err != nil || repo == nil {
		return paths
	}

	if _, listed := repo.Lists(s.base.dir); !listed {
		return paths
	}

	paths = append(paths, repo.File())
	for _, shared := range repo.Shared {
		paths = append(paths, filepath.Join(repo.Dir, filepath.FromSlash(shared)))
	}

	return paths
}

// run returns a fresh validation of the plugin, its steps' rows going to out.
func (s *devSession) run(out io.Writer) *validation {
	v := s.base
	v.table = newStepTable(out)

	return &v
}

// validate validates the plugin as plg validate does, reusing the contract modules earlier runs resolved.
func (s *devSession) validate(ctx context.Context) {
	run := s.run(s.env.Stdout)
	run.table.row("STEP", "RESULT", "DETAIL")

	valid := run.run(ctx)

	err := run.table.flush()
	if err != nil {
		return
	}

	if valid {
		s.printf("plg dev: %s is valid\n", run.dir)
	} else {
		s.printf("plg dev: %s is not a valid plugin\n", run.dir)
	}
}

// command runs one command line; it reports whether plg dev goes on.
func (s *devSession) command(ctx context.Context, line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return true
	}

	switch fields[0] {
	case "quit", "exit":
		return false
	case "run":
		s.validate(ctx)
	case "call":
		s.callCommand(ctx, fields[1:])
	default:
		s.help()
	}

	return true
}

// help lists the commands.
func (s *devSession) help() {
	s.printf("commands: call <Method> [input file], run, quit\n")
}

// callCommand runs a call command, whose arguments are the method and the input file, if any.
func (s *devSession) callCommand(ctx context.Context, args []string) {
	switch len(args) {
	case 1:
		s.call(ctx, args[0], "")
	case 2:
		s.call(ctx, args[0], args[1])
	default:
		s.help()
	}
}

// call calls the plugin's method with the parameters of the input file and prints what it returned.
func (s *devSession) call(ctx context.Context, method, input string) {
	var steps strings.Builder

	v := s.run(&steps)
	v.table.row("STEP", "RESULT", "DETAIL") // shown when a step fails, to say why

	result, err := v.invoke(ctx, method, input)
	if errors.Is(err, errSteps) {
		_ = v.table.flush() //nolint:errcheck // a Builder never fails

		s.printf("%s%s failed: %v\n", steps.String(), method, err)

		return
	}

	if err != nil {
		s.printf("%s failed: %v\n", method, err)

		return
	}

	s.printf("%s: %s\n", method, result)
}

// invoke resolves the plugin's contract as validate does and calls method in a fresh instance of the plugin, with
// the arguments of the input file (none when empty), under the plugin's limits and without the service's Host.
func (v *validation) invoke(ctx context.Context, method, input string) (string, error) {
	res, cleanup, ok := v.resolveContract(ctx)
	if !ok {
		return "", errSteps
	}

	defer cleanup()

	opts := v.options(&res.desc, res.loaded.Directives.Limits)

	if res.desc.Language == plg.LanguageGo {
		opts.GoGuest, ok = v.goGuest(ctx, res.loaded.Directives.GuestPackages)
		if !ok {
			return "", errSteps
		}
	}

	runner, err := offline.Open(ctx, res.loaded.Contract, v.plugin(res.manifest), opts)
	if err != nil {
		return "", err //nolint:wrapcheck // plg's errors name the plugin
	}

	defer runner.Close(context.WithoutCancel(ctx)) //nolint:errcheck // the call's outcome is what matters

	var (
		files fs.FS
		name  string
	)

	if input != "" {
		path := resolve(v.env.Dir, input)
		files, name = os.DirFS(filepath.Dir(path)), filepath.Base(path)
	}

	return testcase.Invoke(ctx, res.loaded.Contract, files, runner, method, name) //nolint:wrapcheck // names the file
}

// printf writes to stdout.
func (s *devSession) printf(format string, args ...any) {
	fmt.Fprintf(s.env.Stdout, format, args...) //nolint:errcheck // a terminal's failures go unnoticed
}

// readLines sends the lines of in until it ends (then closes the channel) or ctx ends. A nil in sends none.
func readLines(ctx context.Context, input io.Reader) <-chan string {
	if input == nil {
		return nil
	}

	lines := make(chan string)

	go func() {
		defer close(lines)

		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()

	return lines
}

// pollWatch is plg dev's default watcher: it looks at the files under paths every pollInterval.
func pollWatch(ctx context.Context, paths []string) <-chan []string {
	ticker := time.NewTicker(pollInterval)
	context.AfterFunc(ctx, ticker.Stop)

	return watchTicks(ctx, paths, ticker.C)
}

// watchTicks looks at the files under paths on each tick, sending the paths of those created, changed (in size,
// mode or modification time) or removed since it last looked, until ctx ends.
func watchTicks(ctx context.Context, paths []string, ticks <-chan time.Time) <-chan []string {
	changes := make(chan []string)
	last := snapshot(paths) // before returning: a change made once it returns is one

	go func() {
		defer close(changes)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
			}

			now := snapshot(paths)

			changed := changedFiles(last, now)
			if len(changed) == 0 {
				continue
			}

			select {
			case changes <- changed:
			case <-ctx.Done():
				return
			}

			last = now
		}
	}()

	return changes
}

// fileStamp is what tells a file's versions apart.
type fileStamp struct {
	modTime time.Time
	size    int64
	mode    fs.FileMode
}

// snapshot stamps the files under paths; those it cannot read are left out.
func snapshot(paths []string) map[string]fileStamp {
	files := map[string]fileStamp{}

	for _, root := range paths {
		//nolint:errcheck,gosec // the walk function never fails
		filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil //nolint:nilerr // an unreadable file is left out, the walk goes on
			}

			info, err := entry.Info()
			if err == nil {
				files[path] = fileStamp{modTime: info.ModTime(), size: info.Size(), mode: info.Mode()}
			}

			return nil
		})
	}

	return files
}

// changedFiles returns the paths, sorted, whose stamps differ between before and after.
func changedFiles(before, after map[string]fileStamp) []string {
	var changed []string

	for path, now := range after {
		if was, ok := before[path]; !ok || !was.modTime.Equal(now.modTime) || was.size != now.size ||
			was.mode != now.mode {
			changed = append(changed, path)
		}
	}

	for path := range before {
		if _, ok := after[path]; !ok {
			changed = append(changed, path)
		}
	}

	slices.Sort(changed)

	return changed
}
