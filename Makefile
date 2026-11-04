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

# The Go guest's standard library allowlist (PLAN.md) has one source, internal/goallow/packages.txt, which
# `plg gen` and the tests read too. Symbol tables are extracted for every package except cmp (yaegi compiles it
# from source) and two extracted separately: encoding/json, whose Options interface has a method on an internal
# type, so its wrapper cannot be generated and is excluded; and net/http, whose client side only is allowlisted
# (requests go through the host transport; the guest has no sockets), so the server-side symbols in
# GUEST_HTTP_SERVER are left out.
GUEST_ALLOWLIST := $(shell sed -e 's/#.*//' internal/goallow/packages.txt)
GUEST_STDLIB := $(filter-out cmp encoding/json net/http,$(GUEST_ALLOWLIST))
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

.PHONY: check fmt-check lint test coverage gate-test allocs budgets repeat examples bench symbols guests go-guest lua-guest \
	ts-guest ts-bundler verify-guests clean

check: fmt-check lint test coverage gate-test allocs budgets repeat examples

# Formatting must leave no diff.
fmt-check:
	golangci-lint fmt ./...
	git diff --exit-code -- '*.go'

lint:
	golangci-lint run --build-tags integration ./...
	GOOS=wasip1 GOARCH=wasm golangci-lint run ./guestsrc/...

# Unit and integration tests together, race detector on, random order, coverage over every package. -parallel 8
# caps the parallel subtests (each holding wasm instances under the race detector): the root package's run peaks
# at about 6.8 GiB RSS (5.6 GiB at -parallel 2, 8.6 GiB at 16), which leaves room on this 30 GB machine with
# about 12 GB free. Closed containers used to keep their decoded guest modules, so the run grew to about 14 GB
# and was OOM-killed whatever -parallel (PLAN.md, "Closed containers release their compiled guests").
# The previous profile is removed first, so a failed or interrupted run leaves none for the gate to pass on.
test:
	rm -f $(COVERAGE)
	go test -race -shuffle=on -tags integration -parallel 8 -covermode=atomic -coverpkg=./... -coverprofile=$(COVERAGE) ./...

# The gate checks the profile of this run's tests: it depends on them, so `make -j check` cannot run it first.
coverage: test
	scripts/coverage-gate.sh $(COVERAGE) $(COVERAGE_MIN)

# The gates' own tests: the coverage gate fails closed on unusable profiles and runs after the tests.
gate-test:
	scripts/gate-test.sh

# Allocation budgets (RULES.md §3). Coverage instrumentation changes inlining, so they run uninstrumented.
allocs:
	go test -race -shuffle=on -tags integration -count=1 -run 'DoNotAllocate' ./...

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
	go test -race -shuffle=on -count=10 ./...

# The examples module (examples/go.mod), kept apart so the core's go.mod never gains its dependencies: formatting,
# lint, tidy go.mod/go.sum, its unit and integration tests with the race detector (the storages against MinIO and
# PostgreSQL containers, so Docker is required; the service and the README's quick start with real plugins), the
# same coverage gate, and its unit tests repeated.
examples:
	cd examples && golangci-lint fmt ./...
	git diff --exit-code -- 'examples/*.go'
	cd examples && golangci-lint run --build-tags integration ./...
	cd examples && go mod tidy -diff
	rm -f examples/$(COVERAGE)
	cd examples && go test -race -shuffle=on -tags integration -covermode=atomic -coverpkg=./... \
		-coverprofile=$(COVERAGE) ./...
	cd examples && ../scripts/coverage-gate.sh $(COVERAGE) $(COVERAGE_MIN)
	cd examples && go test -race -shuffle=on -count=10 ./...

bench:
	go test -run='^$$' -bench=. -benchmem -count=6 -tags integration ./... | tee $(BENCH_OUT)

$(BIN)/yaegi: go.mod go.sum
	go build -o $@ github.com/traefik/yaegi/cmd/yaegi

# Regenerate the guest's stdlib symbol tables with yaegi extract, for the wasip1 target.
symbols: $(BIN)/yaegi
	find $(GUEST_SYMBOLS) -name '*.go' ! -name symbols.go -delete
	cd $(GUEST_SYMBOLS) && GOOS=wasip1 GOARCH=wasm $(BIN)/yaegi extract -name symbols $(GUEST_STDLIB)
	cd $(GUEST_SYMBOLS) && GOOS=wasip1 GOARCH=wasm $(BIN)/yaegi extract -name symbols -exclude '^Options$$' encoding/json
	cd $(GUEST_SYMBOLS) && GOOS=wasip1 GOARCH=wasm $(BIN)/yaegi extract -name symbols -exclude '$(GUEST_HTTP_EXCLUDE)' net/http

guests: go-guest lua-guest ts-guest ts-bundler

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

# Rebuild everything generated from source and fail if it differs from what is committed.
verify-guests: symbols guests
	git diff --exit-code -- $(GUEST_SYMBOLS) $(GO_GUEST) $(LUA_GUEST) $(TS_GUEST) $(TS_BUNDLER)
	test -z "$$(git status --porcelain -- $(GUEST_SYMBOLS) $(GO_GUEST) $(LUA_GUEST) $(TS_GUEST) $(TS_BUNDLER))"

clean:
	rm -rf $(BIN) $(COVERAGE) $(BENCH_OUT) examples/$(COVERAGE)
