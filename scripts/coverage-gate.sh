#!/usr/bin/env bash
# Per-package statement coverage gate (RULES.md §2).
#
# Usage: scripts/coverage-gate.sh <coverprofile> <min-percent>, from the root of the module the profile covers
# (this module, or examples/).
#
# The profile comes from one `go test -coverpkg=./... -coverprofile` run over every package (unit and
# integration tests together), so a statement counts as covered if any test binary executed it. Blocks that
# appear several times (once per test binary) are merged before counting.
#
# Excluded from the gate:
#   - guestsrc/golang and guestsrc/golang/reactor: the wasip1-only `package main` and the //go:wasmexport shims it
#     links. They are not built for the host, so they never appear in the profile; their logic lives in
#     internal/goguest, which is gated.
#   - cmd/plg/main.go: the one-statement main of the plg command, `os.Exit(cli.Main(...))`. It cannot run
#     inside a test process (os.Exit) and the go:generate integration test runs it as a subprocess, whose
#     coverage go test does not collect; everything it calls lives in internal/cli, which is gated.
#   - examples/service/main.go, for the same reason: the service's main only wires signals and os.Exit around
#     `run`, which the tests call in process; TestServiceProgram runs the built program as a subprocess.
#   - generated files (the Go convention's generated-code header line), found by scanning the tree: the
#     yaegi symbol tables in guestsrc/golang/symbols and the `plg gen` output in internal/testcontract and
#     examples/pricing. The symbol tables' interface wrapper methods only run when a plugin calls through them.
#   - packages without statements (pure declarations) contribute nothing.
set -euo pipefail

profile=${1:?usage: coverage-gate.sh <coverprofile> <min-percent>}
min=${2:?usage: coverage-gate.sh <coverprofile> <min-percent>}

module=$(go list -m)
excluded=$(grep -rlE --include='*.go' --exclude-dir=testdata '^// Code [g]enerated .* DO NOT E[D]IT\.$' . |
	sed -e "s|^\./|$module/|" | tr '\n' ' ')
case $module in
*/examples) excluded="$excluded $module/service/main.go" ;;
*) excluded="$excluded $module/cmd/plg/main.go" ;;
esac

awk -v min="$min" -v excluded="$excluded" '
	BEGIN {
		n = split(excluded, g, " ")
		for (i = 1; i <= n; i++) skip[g[i]] = 1
	}
	NR == 1 { next } # mode line
	{
		# github.com/x/pkg/file.go:12.3,14.5 2 1
		split($1, loc, ":")
		file = loc[1]
		if (file in skip) next
		key = $1
		stmts[key] = $2
		if ($3 > 0) hit[key] = 1
		pkg = file
		sub(/\/[^\/]*$/, "", pkg)
		pkgOf[key] = pkg
	}
	END {
		for (key in stmts) {
			p = pkgOf[key]
			total[p] += stmts[key]
			if (key in hit) covered[p] += stmts[key]
		}
		failed = 0
		n = 0
		for (p in total) names[++n] = p
		for (i = 2; i <= n; i++) { # insertion sort, for stable output
			v = names[i]; j = i - 1
			while (j > 0 && names[j] > v) { names[j + 1] = names[j]; j-- }
			names[j + 1] = v
		}
		for (i = 1; i <= n; i++) {
			p = names[i]
			if (total[p] == 0) continue
			pct = 100 * covered[p] / total[p]
			status = "ok"
			if (pct < min) { status = "FAIL"; failed = 1 }
			printf "%-4s %6.1f%%  %s\n", status, pct, p
		}
		if (failed) {
			printf "coverage gate: some packages are below %s%%\n", min
			exit 1
		}
		printf "coverage gate: every package is at or above %s%%\n", min
	}
' "$profile"
