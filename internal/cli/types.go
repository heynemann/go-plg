package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
)

// runTypes runs `plg types`.
func runTypes(env *Env, args []string) outcome {
	flags := flag.NewFlagSet("types", flag.ContinueOnError)
	host := flags.String("host", "", "the contract's Host interface the plugin uses")

	operands, res, ok := parseMixed("types", flags, args)
	if !ok {
		return res
	}

	if len(operands) != 1 {
		return usageError("types", "name one plugin directory")
	}

	dir := resolve(env.Dir, operands[0])
	run := &validation{env: env, dir: dir, arg: operands[0], host: *host, report: newPluginReport(operands[0], dir)}

	written := run.writeTypings(context.Background())
	run.report.finish()

	err := run.report.steps(env.Stdout, wantsColour(env, colourAuto))
	if err != nil {
		return failure("types", err)
	}

	if !written {
		return outcome{code: exitFailure, msg: fmt.Sprintf("plg types: no typings written for %s\n", dir)}
	}

	return outcome{code: exitOK}
}

// writeTypings resolves the plugin's contract as validate does and writes its typings into the plugin directory.
func (v *validation) writeTypings(ctx context.Context) bool {
	res, cleanup, ok := v.resolveContract(ctx)
	if !ok {
		return false
	}

	defer cleanup()

	typ, err := generateTypings(res, v.sharedDirs())
	if err != nil {
		return v.failed("typings", err)
	}

	written, err := typ.Write(v.dir)
	if err != nil {
		return v.failed("typings", err)
	}

	var changed, unchanged []string

	for _, file := range written {
		if file.Changed {
			changed = append(changed, file.Name)
		} else {
			unchanged = append(unchanged, file.Name)
		}
	}

	var detail []string

	if len(changed) > 0 {
		detail = append(detail, "wrote "+strings.Join(changed, ", "))
	}

	if len(unchanged) > 0 {
		detail = append(detail, strings.Join(unchanged, ", ")+" already up to date")
	}

	v.report.row("typings", resultOK, strings.Join(detail, "; "))

	return true
}
