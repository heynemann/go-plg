#!/usr/bin/env bash
# Tests of the gates themselves (RULES.md §2): the coverage gate fails closed on a profile it cannot use, and
# `make` runs it only after the test run that writes the profile.
#
# Usage: scripts/gate-test.sh, from the root of this module.
set -euo pipefail

module=$(go list -m)
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT

failures=0

# expect <want exit: 0 or nonzero> <case> <profile>: runs the coverage gate (minimum 95%) on the profile.
expect() {
	local got=0
	scripts/coverage-gate.sh "$3" 95 >"$dir/out" 2>&1 || got=$?
	if { [ "$1" = 0 ] && [ "$got" != 0 ]; } || { [ "$1" != 0 ] && [ "$got" = 0 ]; }; then
		printf 'FAIL coverage gate, %s: exit %s, want %s\n' "$2" "$got" "$1"
		sed -e 's/^/     /' "$dir/out"
		failures=1
	else
		printf 'ok   coverage gate, %s\n' "$2"
	fi
}

: >"$dir/empty"
printf 'mode: atomic\n' >"$dir/header"
printf 'mode: atomic\n%s/internal/wire/wire.go:1.1,2.2 3 1\n' "$module" >"$dir/covered"
printf 'mode: atomic\n%s/internal/wire/wire.go:1.1,2.2 3 0\n' "$module" >"$dir/uncovered"
printf 'mode: atomic\nnot a coverage block\n' >"$dir/garbage"
printf '%s/internal/wire/wire.go:1.1,2.2 3 1\n' "$module" >"$dir/headless"

expect 1 "missing profile" "$dir/missing"
expect 1 "empty profile" "$dir/empty"
expect 1 "header-only profile" "$dir/header"
expect 1 "malformed block" "$dir/garbage"
expect 1 "no mode line" "$dir/headless"
expect 1 "package below the minimum" "$dir/uncovered"
expect 0 "package covered" "$dir/covered"

# The gate runs after the test run writing its profile, which starts by removing the previous one, even with -j:
# make -n prints recipes in the order make runs them.
order=$(make -n -j8 coverage | grep -nE '^(rm -f coverage\.out|go test .*-coverprofile=coverage\.out|scripts/coverage-gate\.sh)' |
	cut -d: -f1 | tr '\n' ' ')
lines=($order)
if [ "${#lines[@]}" = 3 ] && [ "${lines[0]}" -lt "${lines[1]}" ] && [ "${lines[1]}" -lt "${lines[2]}" ]; then
	printf 'ok   make runs coverage after removing and rewriting the profile\n'
else
	printf 'FAIL make -n -j8 coverage does not remove the profile, run the tests, then the gate (lines: %s)\n' "$order"
	failures=1
fi

exit "$failures"
