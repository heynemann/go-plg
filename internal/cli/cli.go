// Package cli is the plg command. cmd/plg only calls [Main], so the whole command runs in tests.
package cli

import (
	"fmt"
	"io"

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
`

// Main runs plg with args (the command line without the program name) on the Go package in dir, and returns
// the process exit code. Errors and usage go to stderr; if stderr fails too, the exit code still reports
// the failure.
func Main(args []string, dir string, stderr io.Writer) int {
	res := run(args, dir)
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

func run(args []string, dir string) outcome {
	if len(args) == 0 {
		return outcome{code: exitUsage, msg: usage}
	}

	switch args[0] {
	case "gen":
		if len(args) == 1 {
			return outcome{code: exitUsage, msg: "plg gen: name at least one interface\n\n" + usage}
		}

		err := generate(dir, args[1:])
		if err != nil {
			return outcome{code: exitFailure, msg: fmt.Sprintf("plg gen: %v\n", err)}
		}

		return outcome{code: exitOK}
	default:
		return outcome{code: exitUsage, msg: fmt.Sprintf("plg: unknown command %q\n\n%s", args[0], usage)}
	}
}

func generate(dir string, interfaces []string) error {
	file, err := gen.Generate(dir, interfaces)
	if err != nil {
		return err //nolint:wrapcheck // gen's errors are complete; run prefixes the command
	}

	return gen.Write(dir, file) //nolint:wrapcheck // as above
}
