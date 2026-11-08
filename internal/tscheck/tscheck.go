// Package tscheck type-checks TypeScript with Microsoft's TypeScript compiler written in Go (typescript-go's tsgo
// command), compiled to a wasip1 command by `make ts-checker` (the version is pinned by the module in
// guestsrc/tscheck) and run by plg's engine, so `plg validate` needs neither Node.js nor network access to check a
// TypeScript plugin. Only the plg command links it: tsgo.wasm.gz is about 11 MB.
package tscheck

import (
	"bufio"
	"bytes"
	"context"
	_ "embed" // tsgo.wasm.gz
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/guestreg"
)

// Name names the compiler's module, in compile logs and as the command's name.
const Name = "tsgo"

// MemoryLimit caps the compiler's memory: a plugin and the declarations of ECMAScript's library take about
// 100 MiB.
const MemoryLimit = 1 << 30

// ErrCompiler reports a run of the compiler that failed without reporting diagnostics (a bad option, a crash).
var ErrCompiler = errors.New("tscheck: the TypeScript compiler failed")

//go:embed tsgo.wasm.gz
var artifact []byte

// wasm returns the compiler's module, inflated on first use and kept for the life of the process.
//
//nolint:gochecknoglobals // the once-per-process inflation, as the guests' (internal/guestreg)
var wasm = sync.OnceValues(func() ([]byte, error) { return guestreg.Inflate(artifact) })

// Checker runs the compiler. It is safe for concurrent use.
type Checker struct {
	mod *engine.Module
}

// Compile compiles the compiler's module with rt (through its engine's compilation cache), whose memory limit
// should be [MemoryLimit]. A cold compile takes about 10 seconds; loading it from the cache well under one.
func Compile(ctx context.Context, rt *engine.Runtime) (*Checker, error) {
	module, err := wasm()
	if err != nil {
		return nil, fmt.Errorf("tscheck: %w", err)
	}

	mod, err := rt.Compile(ctx, Name, module)
	if err != nil {
		return nil, fmt.Errorf("tscheck: %w", err)
	}

	return &Checker{mod: mod}, nil
}

// Diagnostic is an error the compiler reports.
type Diagnostic struct {
	// File is the file as the compiler names it (relative to the working directory), empty for an error about
	// the project as a whole.
	File string
	// Code is the compiler's error code, e.g. TS2322.
	Code string
	// Message is the compiler's message, the lines of its explanation joined by spaces.
	Message string
	// Line and Column start at 1; zero without a file.
	Line, Column int
}

// String formats the diagnostic as file:line:column: message (code).
func (d *Diagnostic) String() string {
	if d.File == "" {
		return fmt.Sprintf("%s (%s)", d.Message, d.Code)
	}

	return fmt.Sprintf("%s:%d:%d: %s (%s)", d.File, d.Line, d.Column, d.Message, d.Code)
}

// Check type-checks the project whose tsconfig.json is config, a path inside one of the mounts, in the working
// directory cwd (a mount's directory), and returns the compiler's diagnostics: none when the project
// type-checks. It fails when the run fails otherwise (ctx ending included).
func (c *Checker) Check(ctx context.Context, cwd, config string, mounts ...engine.Mount) ([]Diagnostic, error) {
	var stdout, stderr bytes.Buffer

	code, err := c.mod.Run(ctx, &engine.RunOptions{
		Stdout: &stdout, Stderr: &stderr, Mounts: mounts, Env: []string{"PWD=" + cwd},
		Args: []string{"--pretty", "false", "--singleThreaded", "--project", config},
	})
	if err != nil {
		return nil, fmt.Errorf("tscheck: %w", err)
	}

	diags := parse(stdout.Bytes())

	switch {
	case code == 0 && len(diags) == 0:
		return nil, nil
	case code != 0 && len(diags) > 0:
		return diags, nil
	default:
		return nil, fmt.Errorf("%w: exit code %d: %s", ErrCompiler, code,
			strings.TrimSpace(stdout.String()+"\n"+stderr.String()))
	}
}

// The compiler's diagnostic lines with --pretty false: `file(line,col): error TSn: message`, or `error TSn:
// message` without a location, each followed by the indented lines of its explanation.
var (
	located = regexp.MustCompile(`^(.+)\((\d+),(\d+)\): error (TS\d+): (.*)$`)
	global  = regexp.MustCompile(`^error (TS\d+): (.*)$`)
)

// parse reads the compiler's diagnostics from its output.
func parse(out []byte) []Diagnostic {
	var diags []Diagnostic

	lines := bufio.NewScanner(bytes.NewReader(out))
	lines.Buffer(nil, len(out)+1)

	for lines.Scan() {
		line := lines.Text()

		if m := located.FindStringSubmatch(line); m != nil {
			lineNo, _ := strconv.Atoi(m[2]) //nolint:errcheck // \d+: only an overflow fails, and leaves 0
			column, _ := strconv.Atoi(m[3]) //nolint:errcheck // as above
			diags = append(diags, Diagnostic{File: m[1], Line: lineNo, Column: column, Code: m[4], Message: m[5]})

			continue
		}

		if m := global.FindStringSubmatch(line); m != nil {
			diags = append(diags, Diagnostic{Code: m[1], Message: m[2]})

			continue
		}

		if len(diags) > 0 && strings.HasPrefix(line, " ") && strings.TrimSpace(line) != "" {
			last := &diags[len(diags)-1]
			last.Message += " " + strings.TrimSpace(line)
		}
	}

	return diags
}
