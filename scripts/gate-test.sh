#!/usr/bin/env bash
# Tests of the gates themselves (RULES.md §2): the coverage gate fails closed on a profile it cannot use, and
# `make` runs it only after the test run that writes the profile. Also the container rule (RULES.md §7): every Go
# file that starts a testcontainers container registers its termination with CleanupContainer on the next line.
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

# owned_containers <file>...: prints, and fails on, each testcontainers start (Run or GenericContainer of a package
# imported from testcontainers-go) not followed on the next line by `testcontainers.CleanupContainer(tb, ctr)` for
# the container it assigned, and each termination by other means (TerminateContainer, Terminate): containers are
# terminated through the test's cleanup, never by TestMain or a defer (RULES.md §7).
owned_containers() {
	awk '
		function fail(msg) { printf "%s:%d: %s (RULES.md §7)\n", FILENAME, FNR, msg; bad = 1 }
		function unregistered() { fail("register the termination of " pending " on the next line: " root ".CleanupContainer(tb, " pending ")") }
		FNR == 1 {
			if (pending != "") { fail_previous() }
			split("", aliases); root = "testcontainers"; pending = ""
		}
		function fail_previous() { printf "%s:%d: register the termination of %s on the next line: %s.CleanupContainer(tb, %s) (RULES.md §7)\n", file, last, pending, root, pending; bad = 1 }
		/"github\.com\/testcontainers\/testcontainers-go(\/[^"]*)?"/ {
			line = $0
			sub(/^[ \t]*(import[ \t]+)?/, "", line)
			path = line; sub(/^[^"]*"/, "", path); sub(/".*$/, "", path)
			alias = line; sub(/[ \t]*".*$/, "", alias)
			if (alias == "") { n = split(path, parts, "/"); alias = parts[n] }
			if (path == "github.com/testcontainers/testcontainers-go") {
				if (alias == "testcontainers-go") alias = "testcontainers"
				root = alias
			}
			aliases[alias] = 1
		}
		pending != "" {
			if ($0 !~ ("^[ \t]*" root "\\.CleanupContainer\\([A-Za-z_][A-Za-z0-9_]*, " pending "[,)]")) unregistered()
			pending = ""
		}
		/TerminateContainer\(|\.Terminate\(/ {
			for (alias in aliases) { fail("terminate containers only through " root ".CleanupContainer"); break }
		}
		{
			for (alias in aliases) {
				if ($0 !~ ("(^|[^A-Za-z0-9_.])" alias "\\.(Run|GenericContainer)\\(")) continue
				ctr = $0
				sub(/^[ \t]*/, "", ctr)
				if (ctr !~ /^[A-Za-z_][A-Za-z0-9_]*, [A-Za-z_][A-Za-z0-9_]* :?= /) {
					fail("assign the started container to a variable, then register its termination")
					continue
				}
				sub(/,.*$/, "", ctr)
				pending = ctr
			}
			file = FILENAME; last = FNR
		}
		END {
			if (pending != "") { fail_previous() }
			exit bad
		}
	' "$@"
}

# expect_owned <want exit: 0 or nonzero> <case> <Go source>: runs the container check on the source.
expect_owned() {
	local got=0
	printf '%s\n' "$3" >"$dir/case.go"
	owned_containers "$dir/case.go" >"$dir/out" 2>&1 || got=$?
	if { [ "$1" = 0 ] && [ "$got" != 0 ]; } || { [ "$1" != 0 ] && [ "$got" = 0 ]; }; then
		printf 'FAIL container check, %s: exit %s, want %s\n' "$2" "$got" "$1"
		sed -e 's/^/     /' "$dir/out"
		failures=1
	else
		printf 'ok   container check, %s\n' "$2"
	fi
}

imports='import (
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)'

expect_owned 0 "cleanup right after the start" "$imports
func start(tb testing.TB) {
	ctr, err := postgres.Run(tb.Context(), \"postgres:17-alpine\")
	testcontainers.CleanupContainer(tb, ctr)
	require.NoError(tb, err)
}"
expect_owned 0 "aliased imports" 'import (
	tc "github.com/testcontainers/testcontainers-go"
	pg "github.com/testcontainers/testcontainers-go/modules/postgres"
)
func start(tb testing.TB) {
	c, err := pg.Run(tb.Context(), "postgres:17-alpine")
	tc.CleanupContainer(tb, c)
}'
expect_owned 0 "no testcontainers import" 'func TestX(t *testing.T) {
	t.Run("x", func(t *testing.T) { postgres.Run(); x.Terminate(ctx) })
}'
expect_owned 1 "TestMain with a defer" "$imports
func TestMain(m *testing.M) {
	ctr, err := postgres.Run(context.Background(), \"postgres:17-alpine\")
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
}"
expect_owned 1 "cleanup after the error check" "$imports
func start(tb testing.TB) {
	ctr, err := testcontainers.GenericContainer(tb.Context(), req)
	require.NoError(tb, err)
	testcontainers.CleanupContainer(tb, ctr)
}"
expect_owned 1 "cleanup of another container" "$imports
func start(tb testing.TB) {
	ctr, err := testcontainers.Run(tb.Context(), \"alpine\")
	testcontainers.CleanupContainer(tb, other)
}"
expect_owned 1 "start as the last line" "$imports
	ctr, err := postgres.Run(tb.Context(), \"postgres:17-alpine\")"
expect_owned 1 "a started container not assigned" "$imports
func start(tb testing.TB) {
	testcontainers.CleanupContainer(tb, must(postgres.Run(tb.Context(), \"x\")))
}"
expect_owned 1 "Terminate called directly" "$imports
func stop(tb testing.TB, ctr testcontainers.Container) {
	tb.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
}"

mapfile -t sources < <(git ls-files '*.go')
if owned_containers "${sources[@]}" >"$dir/out" 2>&1; then
	printf 'ok   container check: every container started in this repository is terminated by its test\n'
else
	printf 'FAIL container check: containers not terminated through their test'"'"'s cleanup\n'
	sed -e 's/^/     /' "$dir/out"
	failures=1
fi

exit "$failures"
