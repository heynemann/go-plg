#!/usr/bin/env bash
# Benchmark budgets (RULES.md §3): Get takes at most 50 ns/op, and neither Get nor All allocates.
#
# Usage: scripts/bench-budget.sh <go test -bench output>
#
# The input holds several runs of BenchmarkGet and BenchmarkAll (`make budgets`: -count=5, -benchmem, without the
# race detector or coverage, which slow every operation down). The time budget applies to the fastest run, so that
# a run slowed by other work on the machine cannot fail the gate while a real regression, which slows every run,
# still does; the allocation budget applies to every run. A missing benchmark fails the gate.
set -euo pipefail

input=${1:?usage: bench-budget.sh <go test -bench output>}

awk '
	BEGIN {
		maxNs["Get"] = 50
		maxAllocs["Get"] = 0
		maxAllocs["All"] = 0
	}
	# BenchmarkGet-32   	100000000	        10.98 ns/op	       0 B/op	       0 allocs/op
	$1 ~ /^Benchmark(Get|All)(-[0-9]+)?$/ {
		name = $1
		sub(/^Benchmark/, "", name)
		sub(/-[0-9]+$/, "", name)
		for (i = 3; i < NF; i++) {
			if ($(i + 1) == "ns/op" && (!(name in minNs) || $i + 0 < minNs[name])) minNs[name] = $i + 0
			if ($(i + 1) == "allocs/op" && (!(name in worstAllocs) || $i + 0 > worstAllocs[name])) worstAllocs[name] = $i + 0
		}
		runs[name]++
	}
	END {
		failed = 0
		for (name in maxAllocs) {
			if (!(name in runs) || !(name in worstAllocs)) {
				printf "FAIL Benchmark%s: no result with allocations (run it with -benchmem)\n", name
				failed = 1
				continue
			}
			status = "ok"
			if (worstAllocs[name] > maxAllocs[name]) { status = "FAIL"; failed = 1 }
			printf "%-4s Benchmark%s: %d allocs/op at most over %d runs (budget %d)\n", status, name, worstAllocs[name],
				runs[name], maxAllocs[name]
		}
		for (name in maxNs) {
			if (!(name in minNs)) {
				printf "FAIL Benchmark%s: no ns/op result\n", name
				failed = 1
				continue
			}
			status = "ok"
			if (minNs[name] > maxNs[name]) { status = "FAIL"; failed = 1 }
			printf "%-4s Benchmark%s: %.2f ns/op, the fastest of %d runs (budget %d)\n", status, name, minNs[name],
				runs[name], maxNs[name]
		}
		if (failed) {
			print "benchmark budgets: exceeded"
			exit 1
		}
		print "benchmark budgets: met"
	}
' "$input"
