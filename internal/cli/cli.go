// Package cli is the plg command. cmd/plg only calls [Main], so the whole command runs in tests.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/heynemann/go-plg"
	// The guests plg ships, which `plg warm` compiles: every guest package registers itself in internal/guestreg,
	// and plg.Warm compiles what is registered.
	_ "github.com/heynemann/go-plg/guest/golang"
	_ "github.com/heynemann/go-plg/guest/lua"
	_ "github.com/heynemann/go-plg/guest/typescript"
	"github.com/heynemann/go-plg/internal/buildguest"
	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/gen"
	"github.com/heynemann/go-plg/internal/tscheck"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

const usage = `usage: plg <command> [arguments]

commands:
  gen [-fast=false] <Interface>...
                      generate the plg adapters and contract registration for interfaces declared in the Go
                      package in the current directory, checking their plg directives (//plg:limits,
                      //plg:guest-packages, //plg:validation); run it from a go:generate line next to them:
                      //go:generate go run github.com/heynemann/go-plg/cmd/plg gen Fooer
                      -fast=false leaves out the fast paths (typed adapters and codecs generated for the
                      interfaces' types): plg then calls the plugins through its generic, reflective binder
  warm [-cache-dir dir] [-validate]
                      compile every guest plg ships into the compilation cache (default: $PLG_CACHE_DIR, else
                      the user cache directory's plg/<wazero version>), e.g. in a Docker build, so services
                      using that cache start without compiling them:
                      go run github.com/heynemann/go-plg/cmd/plg warm -cache-dir /var/cache/plg
                      -validate also compiles what validate runs besides the guests: the TypeScript compiler
  build-guest -pkg <import path> [-pkg ...] -o <file>
                      build a custom Go guest whose plugins may also import the given packages (standard library
                      packages, or pure-Go packages of modules the service module in the current directory
                      requires); pass it to plg.WithGoGuest
  validate [-host Interface] [-cache-dir dir] [-guest-cache dir] [-format table|json] [-junit file]
           [-color auto|always|never] [-near-limit share] <plugin dir or author repository>
                      check a plugin before registering it, exiting 1 on any error (0 with warnings): read its
                      plugin.yaml and check its ownership, resolve the contract module it pins with the go command
                      (go mod download: GOPROXY, GOPRIVATE and credentials apply, and a module in the Go module
                      cache needs no network), type-check the contract and read its directives; run the language
                      checks: a Go plugin's go.mod requires the pinned version, it starts no goroutines and
                      imports only what the Go guest provides, and it type-checks; a Lua or TypeScript plugin's
                      typings are what types writes, a TypeScript plugin type-checks, every Lua file compiles;
                      then load the plugin and run its Create in the real guest, with a Host whose every call
                      fails, under plg.yaml's limits over the contract's //plg:limits over plg's defaults
                      (memory, pool; timeout bounds Create); run Validate with each examples/validation/*.yaml
                      input, the test cases (tests/*.yaml), and report each case's peak memory and time against
                      the limits; a Go plugin of a contract with //plg:guest-packages runs in that custom guest,
                      built from the plugin's module as build-guest builds one and kept in -guest-cache (default:
                      the user cache directory's plg/guests); a directory holding plg.yaml is an author
                      repository: every plugin it lists is validated, then summarised, and its settings (shared
                      directories, limits, requireOwnership, replace) apply, also to one of its plugins validated
                      alone;
                      -host names the contract's Host interface a Lua or TypeScript plugin uses (a Go plugin's
                      Create names it; by default the one the plugin's typings were written for, else the
                      contract's only interface that can be a Host),
                      -cache-dir is the compilation cache directory, as for warm,
                      -format json prints the report as JSON (its schema: the README) instead of a table,
                      -junit also writes it as JUnit XML to file, -color colours the table (auto: on a terminal
                      without NO_COLOR or TERM=dumb), -near-limit flags test cases at or above that share of a
                      limit (default 0.8)
  types [-host Interface] <plugin dir>
                      write a Lua or TypeScript plugin's typings, to commit with it, from the contract its
                      plugin.yaml pins (resolved as validate resolves it): TypeScript declarations (plg.d.ts) and
                      the compiler options validate type-checks with (tsconfig.json), or Lua Language Server
                      annotations (plg.d.lua); files it did not write are never overwritten; -host as for validate
  init -implements <package>.<Interface>@<version> -lang go|lua|ts [-host Interface] [dir]
                      create a plugin directory (default: the interface's name, lowered) implementing the
                      interface: plugin.yaml (implements, language, entrypoint and an owner block to fill in), the
                      typings (Lua and TypeScript), a skeleton implementing every method with its results' zero
                      values (and Validate, returning no issues, when the contract has //plg:validation), and
                      tests/example.yaml calling each method; a Go plugin's module requires the contract module (a
                      go.mod of the plugin's own, or the author repository's, which gains the requirement); a
                      plugin inside an author repository is added to its plg.yaml. The contract is resolved as
                      validate resolves it; its Host is -host's, else the contract's only Host candidate
  dev [-host Interface] [-cache-dir dir] [-guest-cache dir] [plugin dir]
                      validate a plugin (default: the current directory, or an author repository's only plugin)
                      and again on each change to its files, its tests, the repository's shared directories or
                      plg.yaml; the contract module is resolved once and the guests stay compiled in the
                      compilation cache between runs. Reads commands from stdin: call <Method> [input file]
                      calls one method (or Validate) in a fresh instance with the parameters the YAML input file
                      names (as a test case's input; the Host and HTTP requests fail) and prints its result; run
                      validates again; quit (or the end of the input) stops
  status [-profile name] [-format table|json] [-team name] [key]
                      show every plugin an environment's admin handler (plg.AdminHandler) serves, or one in
                      detail; -team keeps the plugins whose owner team is name
  events [-profile name] [-format table|json] <key>
                      show a plugin's newest events (at most 100), oldest first

status and events read the environment named by -profile (default: the profiles file's default, or its only
profile) from $XDG_CONFIG_HOME/plg/profiles.yaml (default ~/.config/plg/profiles.yaml):

  default: staging
  profiles:
    - name: staging
      url: https://staging.example.com/admin/plg   # where the service mounts plg.AdminHandler
      token: {env: PLG_STAGING_TOKEN}              # sent as "Authorization: Bearer <token>"
    - name: prod
      url: https://prod.example.com/admin/plg
      token: {command: [vault, read, -field=token, secret/plg/prod]}   # its output, trimmed
`

// Env is what a command runs in.
type Env struct {
	// Stdin is what plg dev reads its commands from; nil reads none.
	Stdin io.Reader
	// Stdout gets what status and events show.
	Stdout io.Writer
	// Stderr gets errors, usage and warm's compile log.
	Stderr io.Writer
	// Getenv reads the environment variables (os.Getenv).
	Getenv func(key string) string
	// Environ returns the environment (os.Environ), which validate runs the go command in; nil means the
	// process's.
	Environ func() []string
	// Watch watches the files under paths (directories and files) for plg dev until ctx ends, sending the paths
	// that changed; nil polls the file system every quarter of a second.
	Watch func(ctx context.Context, paths []string) <-chan []string
	// Dir is the working directory, against which relative paths resolve.
	Dir string
}

// environ returns the environment commands run in.
func (e *Env) environ() []string {
	if e.Environ == nil {
		return os.Environ()
	}

	return e.Environ()
}

// Main runs plg with args (the command line without the program name) in env, and returns the process exit
// code. If stderr fails, the exit code still reports a failure.
func Main(args []string, env *Env) int {
	res := run(args, env)
	if res.msg == "" {
		return res.code
	}

	_, err := io.WriteString(env.Stderr, res.msg)
	if err != nil {
		return exitFailure
	}

	return res.code
}

// outcome is how a command ended: its exit code, and what to tell the user on stderr.
type outcome struct {
	msg  string
	code int
}

// commands are plg's commands, by name.
//
//nolint:gochecknoglobals // immutable lookup table
var commands = map[string]func(env *Env, args []string) outcome{
	"gen":         func(env *Env, args []string) outcome { return runGen(env.Dir, args) },
	"warm":        func(env *Env, args []string) outcome { return runWarm(env.Dir, args, env.Stderr) },
	"build-guest": func(env *Env, args []string) outcome { return runBuildGuest(env.Dir, args) },
	"status":      runStatus,
	"events":      runEvents,
	"validate":    runValidate,
	"types":       runTypes,
	"init":        runInit,
	"dev":         runDev,
}

func run(args []string, env *Env) outcome {
	if len(args) == 0 {
		return outcome{code: exitUsage, msg: usage}
	}

	cmd, args := args[0], args[1:]

	command, ok := commands[cmd]
	if !ok {
		return outcome{code: exitUsage, msg: fmt.Sprintf("plg: unknown command %q\n\n%s", cmd, usage)}
	}

	return command(env, args)
}

// usageError is a command's misuse.
func usageError(cmd, msg string) outcome {
	return outcome{code: exitUsage, msg: fmt.Sprintf("plg %s: %s\n\n%s", cmd, msg, usage)}
}

// failure is a command's outcome after running: success, or its error.
func failure(cmd string, err error) outcome {
	if err != nil {
		return outcome{code: exitFailure, msg: fmt.Sprintf("plg %s: %v\n", cmd, err)}
	}

	return outcome{code: exitOK}
}

// runGen runs `plg gen`.
func runGen(dir string, args []string) outcome {
	flags := flag.NewFlagSet("gen", flag.ContinueOnError)
	fast := flags.Bool("fast", true, "generate the fast paths")

	flags.SetOutput(io.Discard)

	err := flags.Parse(args)

	switch {
	case errors.Is(err, flag.ErrHelp):
		return outcome{code: exitUsage, msg: usage}
	case err != nil:
		return usageError("gen", err.Error())
	case flags.NArg() == 0:
		return usageError("gen", "name at least one interface")
	}

	return failure("gen", generate(dir, flags.Args(), gen.Options{Generic: !*fast}))
}

func generate(dir string, interfaces []string, opts gen.Options) error {
	file, err := gen.Generate(dir, interfaces, opts)
	if err != nil {
		return err //nolint:wrapcheck // gen's errors are complete; run prefixes the command
	}

	return gen.Write(dir, file) //nolint:wrapcheck // as above
}

// parse parses a command's flags; it accepts no positional arguments.
func parse(cmd string, flags *flag.FlagSet, args []string) (outcome, bool) {
	flags.SetOutput(io.Discard)

	err := flags.Parse(args)

	switch {
	case errors.Is(err, flag.ErrHelp):
		return outcome{code: exitUsage, msg: usage}, false
	case err != nil:
		return usageError(cmd, err.Error()), false
	case flags.NArg() > 0:
		return usageError(cmd, "unexpected arguments: "+strings.Join(flags.Args(), " ")), false
	default:
		return outcome{}, true
	}
}

// resolve returns path, relative to dir unless absolute.
func resolve(dir, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(dir, path)
}

func runWarm(dir string, args []string, stderr io.Writer) outcome {
	flags := flag.NewFlagSet("warm", flag.ContinueOnError)
	cacheDir := flags.String("cache-dir", "", "compilation cache directory")
	validate := flags.Bool("validate", false, "also compile the TypeScript compiler plg validate runs")

	if res, ok := parse("warm", flags, args); !ok {
		return res
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	ctx := context.Background()

	err := plg.Warm(ctx, plg.WithLogger(logger), plg.WithCompilationCacheDir(resolve(dir, *cacheDir)))
	if err == nil && *validate {
		err = warmChecker(ctx, logger, resolve(dir, *cacheDir))
	}

	if err != nil {
		return outcome{code: exitFailure, msg: err.Error() + "\n"} // "plg: warm: ..." names the command already
	}

	return outcome{code: exitOK}
}

// warmChecker compiles the TypeScript compiler into the on-disk compilation cache in cacheDir (the default one
// when empty), as plg.Warm compiles the guests.
func warmChecker(ctx context.Context, logger *slog.Logger, cacheDir string) (err error) {
	eng, err := engine.NewOnDisk(engine.Options{Logger: logger, CacheDir: cacheDir})
	if err != nil {
		return fmt.Errorf("plg: warm: %w", err)
	}

	rt := eng.NewRuntime(ctx, engine.RuntimeOptions{MemoryLimit: tscheck.MemoryLimit})

	defer func() { err = errors.Join(err, rt.Close(ctx), eng.Close(ctx)) }()

	_, err = tscheck.Compile(ctx, rt)
	if err != nil {
		return fmt.Errorf("plg: warm: %w", err)
	}

	return nil
}

// packages is a repeatable flag.
type packages []string

func (p *packages) String() string { return strings.Join(*p, " ") }

func (p *packages) Set(pkg string) error {
	*p = append(*p, pkg)

	return nil
}

func runBuildGuest(dir string, args []string) outcome {
	var pkgs packages

	flags := flag.NewFlagSet("build-guest", flag.ContinueOnError)
	output := flags.String("o", "", "output file")

	flags.Var(&pkgs, "pkg", "import path of an extra package (repeatable)")

	if res, ok := parse("build-guest", flags, args); !ok {
		return res
	}

	switch {
	case len(pkgs) == 0:
		return usageError("build-guest", "name at least one package with -pkg")
	case *output == "":
		return usageError("build-guest", "name the output file with -o")
	}

	err := buildguest.Build(context.Background(), &buildguest.Options{Dir: dir, Output: *output, Packages: pkgs})

	return failure("build-guest", err)
}
