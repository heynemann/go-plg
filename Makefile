# Gates and guest builds. `make check` runs exactly what CI runs (RULES.md §6).

SHELL := /bin/bash -o pipefail

# Pinned toolchain: guests are rebuilt with exactly this Go, so the committed artifact is reproducible.
export GOTOOLCHAIN := go1.27.3
export GOFLAGS := -mod=readonly

BIN := $(CURDIR)/bin
COVERAGE := coverage.out
COVERAGE_MIN := 95
BENCH_OUT := bench.txt
BUDGET_RUNS := 5
BUDGET_OUT := $(BIN)/budgets.txt

# go test's -timeout, per test binary: a hang detector, not a speed limit, at about twice what the slowest binary
# takes on CI (GitHub's 4-CPU ubuntu-latest), so that a hang fails the run within minutes while a slow runner does
# not (PLAN.md, "Test time"). TEST_TIMEOUT is `make test`'s, set by the root package, which runs alongside the other
# packages' tests: 471-545 s on CI on main, then 515-792 s as the other packages' tests grew, killed twice by go
# test at 13m12s (12m and a tenth; the binary's own alarm, 12m, times its tests but not its TestMain, which built
# the custom Go guest cold). The same run on 4 CPUs here, with a cold go build cache as CI's: 401 s, and 342 s once
# `make warm` fills the build cache; CI runs it 1.3-2 times slower, so about 450-690 s, and 25m.
# SHORT_TEST_TIMEOUT is the other runs': `make repeat`'s root package took 124 s on CI, then 283 s (run
# 42480219888) and past 300 s (run 42481291163, timed out at 5m) as the unit tests grew, so 10m. Benchmarks run
# outside -timeout.
TEST_TIMEOUT := 25m
SHORT_TEST_TIMEOUT := 10m

# golangci-lint is pinned and built with the repo's toolchain, into bin/, with a cache of its own. A binary built
# with an older Go (a distro or Homebrew build) loads this go1.27.3 module without full type information and
# silently skips type-aware rules such as revive's time-naming, and sharing its cache replays those results; CI
# then failed on findings the local gate never showed. `make check` and CI both lint with this binary.
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT := $(BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)/golangci-lint
export GOLANGCI_LINT_CACHE := $(BIN)/golangci-lint-cache

# The Go guest's import allowlist (PLAN.md: the standard library packages plugins may import and plg's plugin
# package) has one source, internal/goallow/packages.txt, which `plg gen` and the tests read too. Symbol tables
# are extracted for every package except cmp (yaegi compiles it from source) and two extracted separately:
# encoding/json, whose Options interface has a method on an internal type, so its wrapper cannot be generated
# and is excluded; and net/http, whose client side only is allowlisted (requests go through the host transport;
# the guest has no sockets), so the server-side symbols in GUEST_HTTP_SERVER are left out.
GUEST_ALLOWLIST := $(shell sed -e 's/#.*//' internal/goallow/packages.txt)
GUEST_EXTRACTED := $(filter-out cmp encoding/json net/http,$(GUEST_ALLOWLIST))
GUEST_HTTP_SERVER := AllowQuerySemicolons CloseNotifier ConnState CrossOriginProtection DefaultServeMux Dir \
	Error ErrAbortHandler ErrBodyNotAllowed ErrHandlerTimeout ErrHijacked ErrServerClosed ErrSkipAltProtocol \
	ErrWriteAfterFlush FS File FileServer FileServerFS FileSystem Flusher Handle HandleFunc Handler HandlerFunc \
	Hijacker ListenAndServe ListenAndServeTLS LocalAddrContextKey MaxBytesHandler MaxBytesReader \
	NewCrossOriginProtection NewFileTransport NewFileTransportFS NewResponseController NewServeMux NotFound \
	NotFoundHandler PushOptions Pusher Redirect RedirectHandler ResponseController ResponseWriter Serve \
	ServeContent ServeFile ServeFileFS ServeMux ServeTLS Server ServerContextKey State[A-Za-z]+ StripPrefix \
	TimeoutHandler
empty :=
space := $(empty) $(empty)
GUEST_HTTP_EXCLUDE := ^($(subst $(space),|,$(strip $(GUEST_HTTP_SERVER))))$$
GUEST_SYMBOLS := guestsrc/golang/symbols
# The Go guest is over 20 MB raw, so it is committed gzip-compressed (scripts/gzipwasm.go: fixed level, empty
# header, hence reproducible); guest/golang inflates it once per process.
GO_GUEST := guest/golang/golang.wasm.gz
GO_GUEST_RAW := $(BIN)/golang.wasm

# The script guests are C, built with a pinned wasi-sdk from pinned upstream sources. scripts/fetch-pinned.sh
# downloads each archive once into PLG_BUILD_CACHE, checks its SHA-256 and unpacks it there.
PLG_BUILD_CACHE ?= $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/plg-build
WASI_SDK_VERSION := 34.0
WASI_SDK_URL := https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34/wasi-sdk-$(WASI_SDK_VERSION)-x86_64-linux.tar.gz
WASI_SDK_SHA256 := b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4
WASI_SDK := $(PLG_BUILD_CACHE)/wasi-sdk-$(WASI_SDK_VERSION)-x86_64-linux
WASI_CC := $(WASI_SDK)/bin/clang
# Reproducible C builds: no build paths in the output (the file prefix maps; debug info is stripped) and no
# timestamps (nothing uses __DATE__ or __TIME__).
WASI_CFLAGS := --target=wasm32-wasip1 -O2 -std=gnu23 -ffile-prefix-map=$(CURDIR)=. \
	-ffile-prefix-map=$(PLG_BUILD_CACHE)=plg-build

# Lua 5.4 and the plg glue (guestsrc/lua). Lua raises errors with setjmp/longjmp, compiled to wasm exception
# handling (wazero's experimental feature, which every plg runtime enables). Plugins get the base, coroutine,
# table, string, math and utf8 libraries (io, os, package and debug are not linked) and the glue's json. Lua's
# own sources build as released, configured through LUA_USER_H (guestsrc/lua/luauser.h).
LUA_VERSION := 5.4.9
LUA_URL := https://www.lua.org/ftp/lua-$(LUA_VERSION).tar.gz
LUA_SHA256 := 2335b6c582a52654f94612bf10d2f4672805d05329aa6568b1d8cd9e5c6fb8e6
LUA := $(PLG_BUILD_CACHE)/lua-$(LUA_VERSION)
LUA_CORE := lapi lcode lctype ldebug ldo ldump lfunc lgc llex lmem lobject lopcodes lparser lstate lstring ltable \
	ltm lundump lvm lzio lauxlib lbaselib lcorolib lmathlib lstrlib ltablib lutf8lib
LUA_FLAGS := -I$(LUA)/src -Iguestsrc/lua '-DLUA_USER_H="luauser.h"' -mllvm -wasm-enable-sjlj \
	-mllvm -wasm-use-legacy-eh=false
LUA_GUEST := guest/lua/lua.wasm
# The Lua guest's wasm stack, for the C recursion of Lua (bounded by LUAI_MAXCCALLS) and of the glue's codec
# (about 100 bytes per level of the wire's 10000-level nesting limit). It comes first in memory, so overflowing
# it traps instead of overwriting data.
LUA_STACK := 2097152

# QuickJS-ng's amalgamated release (the engine without its std and os modules, which plugins do not get).
QUICKJS_VERSION := 0.18.0
QUICKJS_URL := https://github.com/quickjs-ng/quickjs/releases/download/v$(QUICKJS_VERSION)/quickjs-amalgam.zip
QUICKJS_SHA256 := 6c05066a6b62b483e2ba0f83d7a16688c6958182b214bfd693529434cef84a9e
QUICKJS := $(PLG_BUILD_CACHE)/quickjs-ng-$(QUICKJS_VERSION)
QUICKJS_FLAGS := -D_GNU_SOURCE -D_WASI_EMULATED_PROCESS_CLOCKS -D_WASI_EMULATED_SIGNAL -I$(QUICKJS)
# The TypeScript guest's wasm stack (wasm-ld's default is 64 KiB): QuickJS allows JS frames 1 MiB of it
# (JS_STACK_LIMIT in glue.c), so deep recursion throws a RangeError with room to spare. Every instance's linear
# memory holds it (a Go heap allocation on the host), so it is no larger: 4 MiB passed the same tests.
TS_STACK := 2097152
TS_GUEST := guest/typescript/typescript.wasm
TS_SRC := guestsrc/typescript
# The TypeScript bundler guest: esbuild (internal/tsbundle) compiled to a wasip1 reactor, which the host runs to
# bundle each plugin inside a sandbox (PLAN.md). It is Go, built with the pinned Go like the Go guest, and
# committed gzip-compressed (the module is about 14 MB).
TS_BUNDLER := guest/typescript/bundler.wasm.gz
TS_BUNDLER_RAW := $(BIN)/bundler.wasm
# The TypeScript checker plg validate runs on TypeScript plugins (internal/tscheck): Microsoft's TypeScript compiler
# written in Go (typescript-go's tsgo command, pinned by the module guestsrc/tscheck, which keeps it out of plg's
# go.mod) compiled to a wasip1 command with the pinned Go, committed gzip-compressed (the module is about 50 MB).
# Only the plg command links it.
TS_CHECKER := internal/tscheck/tsgo.wasm.gz
TS_CHECKER_RAW := $(BIN)/tsgo.wasm

.PHONY: check fmt-check lint warm test coverage gate-test allocs budgets repeat examples bench symbols guests go-guest \
	lua-guest ts-guest ts-bundler ts-checker verify-guests clean

check: fmt-check lint test coverage gate-test allocs budgets repeat examples

# Formatting must leave no diff.
fmt-check: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt ./...
	git diff --exit-code -- '*.go'

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --build-tags integration ./...
	GOOS=wasip1 GOARCH=wasm $(GOLANGCI_LINT) run ./guestsrc/...

$(GOLANGCI_LINT):
	GOFLAGS= GOBIN=$(dir $@) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Compile the linked guests into the default compilation cache (`plg warm`), without the race detector: under it
# wazero's compiler runs about ten times slower (the Go guest: 4 s cold natively, 40-65 s under -race on 4 CPUs),
# and a cold cache is what CI gets whenever a guest artifact changes its cache key. The compiled code is the same,
# so the race-instrumented tests read it from the cache like any other run's. -validate compiles the TypeScript
# compiler plg validate runs too (about 10 s cold), which internal/cli's tests use.
# Then fill the go build cache with what every custom Go guest build shares (`plg build-guest`, and plg validate
# of a plugin naming guest packages: the root package's TestMain, internal/buildguest, internal/cli, cmd/plg),
# built as internal/buildguest builds it: the yaegi command for this machine (-trimpath) and the default guest's
# packages for wasip1 (-trimpath -buildvcs=false -buildmode=c-shared). CI's build cache is stale, not empty:
# setup-go restores it with the modules under a key from go.sum and the Go version only, saved by the first job
# to end once that key is new and never again until it changes, so it holds what that job built from the tree
# of its day and lacks what it did not build or what changed since; each of those tests compiled that itself,
# several at once, while the root package ran: 60 CPU-seconds once here, after which the root package's TestMain
# takes 5 s instead of 26 s (65 CPU-seconds) and internal/buildguest's tests 8 s instead of 44 s, on 4 CPUs.
warm:
	go run ./cmd/plg warm -validate
	go build -trimpath -o /dev/null github.com/traefik/yaegi/cmd/yaegi
	GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -buildmode=c-shared -o /dev/null ./guestsrc/golang

# Unit and integration tests together, race detector on, random order, coverage over every package. -parallel 8
# caps the parallel subtests (each holding wasm instances under the race detector): the root package's run peaks
# at about 6.8 GiB RSS (5.6 GiB at -parallel 2, 8.6 GiB at 16), which leaves room on this 30 GB machine with
# about 12 GB free. Closed containers used to keep their decoded guest modules, so the run grew to about 14 GB
# and was OOM-killed whatever -parallel (PLAN.md, "Closed containers release their compiled guests").
# -timeout: see TEST_TIMEOUT. The previous profile is removed first, so a failed or interrupted run leaves none for
# the gate to pass on.
test: warm
	rm -f $(COVERAGE)
	go test -race -shuffle=on -tags integration -parallel 8 -timeout $(TEST_TIMEOUT) -covermode=atomic -coverpkg=./... -coverprofile=$(COVERAGE) ./...

# The gate checks the profile of this run's tests: it depends on them, so `make -j check` cannot run it first.
coverage: test
	scripts/coverage-gate.sh $(COVERAGE) $(COVERAGE_MIN)

# The gates' own tests: the coverage gate fails closed on unusable profiles and runs after the tests. Also checks
# that every test container is terminated through its test's cleanup (RULES.md §7).
gate-test:
	scripts/gate-test.sh

# Allocation budgets (RULES.md §3). Coverage instrumentation changes inlining, so they run uninstrumented.
allocs: warm
	go test -race -shuffle=on -tags integration -count=1 -timeout $(SHORT_TEST_TIMEOUT) -run 'DoNotAllocate' ./...

# Benchmark budgets (RULES.md §3): Get <= 50 ns/op, Get and All 0 allocs/op, checked by scripts/bench-budget.sh.
# Measured without the race detector and coverage (both slow every operation down), BUDGET_RUNS times: the time
# budget applies to the fastest run, so one run slowed by other work on the machine cannot fail the gate.
budgets:
	mkdir -p $(BIN)
	rm -f $(BUDGET_OUT)
	go test -tags integration -run='^$$' -bench='^Benchmark(Get|All)$$' -benchmem -count=$(BUDGET_RUNS) . | \
		tee $(BUDGET_OUT)
	scripts/bench-budget.sh $(BUDGET_OUT)

# Unit tests repeated to flush out flakiness and state leaking between runs (RULES.md §2).
repeat:
	go test -race -shuffle=on -count=10 -timeout $(SHORT_TEST_TIMEOUT) ./...

# The examples module (examples/go.mod), kept apart so the core's go.mod never gains its dependencies: formatting,
# lint, tidy go.mod/go.sum, its unit and integration tests with the race detector (the storages against MinIO and
# PostgreSQL containers, so Docker is required; the service and the README's quick start with real plugins), the
# same coverage gate, and its unit tests repeated.
examples: $(GOLANGCI_LINT) warm
	cd examples && $(GOLANGCI_LINT) fmt ./...
	git diff --exit-code -- 'examples/*.go'
	cd examples && $(GOLANGCI_LINT) run --build-tags integration ./...
	cd examples && go mod tidy -diff
	rm -f examples/$(COVERAGE)
	cd examples && go test -race -shuffle=on -tags integration -timeout $(SHORT_TEST_TIMEOUT) -covermode=atomic \
		-coverpkg=./... -coverprofile=$(COVERAGE) ./...
	cd examples && ../scripts/coverage-gate.sh $(COVERAGE) $(COVERAGE_MIN)
	cd examples && go test -race -shuffle=on -count=10 -timeout $(SHORT_TEST_TIMEOUT) ./...

bench:
	go test -run='^$$' -bench=. -benchmem -count=6 -tags integration ./... | tee $(BENCH_OUT)

$(BIN)/yaegi: go.mod go.sum
	go build -o $@ github.com/traefik/yaegi/cmd/yaegi

# Regenerate the guest's symbol tables of the allowlist with yaegi extract, for the wasip1 target.
symbols: $(BIN)/yaegi
	find $(GUEST_SYMBOLS) -name '*.go' ! -name symbols.go -delete
	cd $(GUEST_SYMBOLS) && GOOS=wasip1 GOARCH=wasm $(BIN)/yaegi extract -name symbols $(GUEST_EXTRACTED)
	cd $(GUEST_SYMBOLS) && GOOS=wasip1 GOARCH=wasm $(BIN)/yaegi extract -name symbols -exclude '^Options$$' encoding/json
	cd $(GUEST_SYMBOLS) && GOOS=wasip1 GOARCH=wasm $(BIN)/yaegi extract -name symbols -exclude '$(GUEST_HTTP_EXCLUDE)' net/http

guests: go-guest lua-guest ts-guest ts-bundler ts-checker

# Build the Go guest artifact (yaegi + internal/goguest compiled to a wasip1 reactor), then compress it.
go-guest:
	GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -buildmode=c-shared -o $(GO_GUEST_RAW) ./guestsrc/golang
	go run scripts/gzipwasm.go $(GO_GUEST_RAW) $(GO_GUEST)

$(WASI_CC):
	scripts/fetch-pinned.sh $(WASI_SDK_URL) $(WASI_SDK_SHA256) $(WASI_SDK)

$(LUA)/src/lua.h:
	scripts/fetch-pinned.sh $(LUA_URL) $(LUA_SHA256) $(LUA)

# Build the Lua guest: Lua and the plg glue as a wasip1 reactor. The glue is checked with every warning as an
# error first; Lua's own sources are built as released.
lua-guest: $(WASI_CC) $(LUA)/src/lua.h
	$(WASI_CC) $(WASI_CFLAGS) $(LUA_FLAGS) -Wall -Wextra -Werror -fsyntax-only guestsrc/lua/*.c
	$(WASI_CC) $(WASI_CFLAGS) $(LUA_FLAGS) -w -mexec-model=reactor -Wl,-z,stack-size=$(LUA_STACK) -Wl,--stack-first \
		-Wl,--strip-debug \
		-o $(LUA_GUEST) $(LUA_CORE:%=$(LUA)/src/%.c) guestsrc/lua/*.c -lsetjmp
	chmod 644 $(LUA_GUEST)

$(QUICKJS)/quickjs-amalgam.c:
	scripts/fetch-pinned.sh $(QUICKJS_URL) $(QUICKJS_SHA256) $(QUICKJS)

# Build the TypeScript guest: QuickJS-ng and the plg glue (with the prelude embedded) as a wasip1 reactor.
ts-guest: $(WASI_CC) $(QUICKJS)/quickjs-amalgam.c
	mkdir -p $(BIN)
	$(WASI_CC) $(WASI_CFLAGS) $(QUICKJS_FLAGS) -w -c -o $(BIN)/quickjs.o $(QUICKJS)/quickjs-amalgam.c
	$(WASI_CC) $(WASI_CFLAGS) $(QUICKJS_FLAGS) -Wall -Wextra -Werror -c -o $(BIN)/ts-glue.o $(TS_SRC)/glue.c
	$(WASI_CC) $(WASI_CFLAGS) -mexec-model=reactor -Wl,-z,stack-size=$(TS_STACK) -Wl,--strip-debug \
		-o $(TS_GUEST) $(BIN)/quickjs.o $(BIN)/ts-glue.o -lwasi-emulated-process-clocks -lwasi-emulated-signal
	chmod 644 $(TS_GUEST)

# Build the TypeScript bundler guest (esbuild + internal/tsbundle compiled to a wasip1 reactor), then compress it.
ts-bundler:
	mkdir -p $(BIN)
	GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -buildmode=c-shared -o $(TS_BUNDLER_RAW) ./guestsrc/tsbundle
	go run scripts/gzipwasm.go $(TS_BUNDLER_RAW) $(TS_BUNDLER)

# Build the TypeScript checker (typescript-go's tsgo compiled to a wasip1 command), then compress it.
ts-checker:
	mkdir -p $(BIN)
	cd guestsrc/tscheck && GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -o $(TS_CHECKER_RAW) \
		github.com/microsoft/typescript-go/cmd/tsgo
	go run scripts/gzipwasm.go $(TS_CHECKER_RAW) $(TS_CHECKER)

# Rebuild everything generated from source and fail if it differs from what is committed.
verify-guests: symbols guests
	git diff --exit-code -- $(GUEST_SYMBOLS) $(GO_GUEST) $(LUA_GUEST) $(TS_GUEST) $(TS_BUNDLER) $(TS_CHECKER)
	test -z "$$(git status --porcelain -- $(GUEST_SYMBOLS) $(GO_GUEST) $(LUA_GUEST) $(TS_GUEST) $(TS_BUNDLER) \
		$(TS_CHECKER))"

clean:
	rm -rf $(BIN) $(COVERAGE) $(BENCH_OUT) examples/$(COVERAGE)
