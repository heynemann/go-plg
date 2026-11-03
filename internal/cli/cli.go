// Package cli is the plg command. cmd/plg only calls [Main], so the whole command runs in tests.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/heynemann/go-plg"
	// The guests plg ships, which `plg warm` compiles: every guest package registers itself in internal/guestreg,
	// and plg.Warm compiles what is registered.
	_ "github.com/heynemann/go-plg/guest/golang"
	_ "github.com/heynemann/go-plg/guest/lua"
	_ "github.com/heynemann/go-plg/guest/typescript"
	"github.com/heynemann/go-plg/internal/buildguest"
	"github.com/heynemann/go-plg/internal/gen"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

const usage = `usage: plg <command> [arguments]

commands:
  gen <Interface>...  generate the plg adapters and contract registration for interfaces declared in the Go
                      package in the current directory; run it from a go:generate line next to them:
                      //go:generate go run github.com/heynemann/go-plg/cmd/plg gen Fooer
  warm [-cache-dir dir]
                      compile every guest plg ships into the compilation cache (default: $PLG_CACHE_DIR, else
                      the user cache directory's plg/<wazero version>), e.g. in a Docker build, so services
                      using that cache start without compiling them:
                      go run github.com/heynemann/go-plg/cmd/plg warm -cache-dir /var/cache/plg
  build-guest -pkg <import path> [-pkg ...] -o <file>
                      build a custom Go guest whose plugins may also import the given packages (standard library
                      packages, or pure-Go packages of modules the service module in the current directory
                      requires); pass it to plg.WithGoGuest
`

// Main runs plg with args (the command line without the program name) in dir, and returns the process exit
// code. Errors, usage and warm's compile log go to stderr; if stderr fails, the exit code still reports a
// failure.
func Main(args []string, dir string, stderr io.Writer) int {
	res := run(args, dir, stderr)
	if res.msg == "" {
		return res.code
	}

	_, err := io.WriteString(stderr, res.msg)
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

func run(args []string, dir string, stderr io.Writer) outcome {
	if len(args) == 0 {
		return outcome{code: exitUsage, msg: usage}
	}

	cmd, args := args[0], args[1:]

	switch cmd {
	case "gen":
		if len(args) == 0 {
			return usageError(cmd, "name at least one interface")
		}

		return failure(cmd, generate(dir, args))
	case "warm":
		return runWarm(dir, args, stderr)
	case "build-guest":
		return runBuildGuest(dir, args)
	default:
		return outcome{code: exitUsage, msg: fmt.Sprintf("plg: unknown command %q\n\n%s", cmd, usage)}
	}
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

func generate(dir string, interfaces []string) error {
	file, err := gen.Generate(dir, interfaces)
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

	if res, ok := parse("warm", flags, args); !ok {
		return res
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))

	err := plg.Warm(context.Background(), plg.WithLogger(logger),
		plg.WithCompilationCacheDir(resolve(dir, *cacheDir)))
	if err != nil {
		return outcome{code: exitFailure, msg: err.Error() + "\n"} // "plg: warm: ..." names the command already
	}

	return outcome{code: exitOK}
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

	err := buildguest.Build(context.Background(), buildguest.Options{Dir: dir, Output: *output, Packages: pkgs})

	return failure("build-guest", err)
}
