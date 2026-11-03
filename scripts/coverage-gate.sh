#!/usr/bin/env bash
# Per-package statement coverage gate (RULES.md §2).
#
# Usage: scripts/coverage-gate.sh <coverprofile> <min-percent>
#
# The profile comes from one `go test -coverpkg=./... -coverprofile` run over every package (unit and
# integration tests together), so a statement counts as covered if any test binary executed it. Blocks that
# appear several times (once per test binary) are merged before counting.
#
# Excluded from the gate:
#   - guestsrc/golang: the wasip1-only `package main` with the //go:wasmexport shims. It is not built for the
#     host, so it never appears in the profile; its logic lives in internal/goguest, which is gated.
#   - generated files (the Go convention's generated-code header line), found by scanning the tree: the
#     yaegi symbol tables in guestsrc/golang/symbols. Their interface wrapper methods only run when a plugin
#     calls through them.
#   - packages without statements (pure declarations) contribute nothing.
set -euo pipefail

profile=${1:?usage: coverage-gate.sh <coverprofile> <min-percent>}
min=${2:?usage: coverage-gate.sh <coverprofile> <min-percent>}

module=$(go list -m)
generated=$(grep -rlE --include='*.go' --exclude-dir=testdata '^// Code [g]enerated .* DO NOT E[D]IT\.$' . |
	sed -e "s|^\./|$module/|" | tr '\n' ' ')

awk -v min="$min" -v generated="$generated" '
	BEGIN {
		n = split(generated, g, " ")
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
