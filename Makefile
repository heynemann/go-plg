# Gates and guest builds. `make check` runs exactly what CI runs (RULES.md §6).

SHELL := /bin/bash -o pipefail

# Pinned toolchain: guests are rebuilt with exactly this Go, so the committed artifact is reproducible.
export GOTOOLCHAIN := go1.27.3
export GOFLAGS := -mod=readonly

BIN := $(CURDIR)/bin
COVERAGE := coverage.out
COVERAGE_MIN := 95
BENCH_OUT := bench.txt

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

.PHONY: check fmt-check lint test coverage allocs repeat bench symbols guests verify-guests clean

check: fmt-check lint test coverage allocs repeat

# Formatting must leave no diff.
fmt-check:
	golangci-lint fmt ./...
	git diff --exit-code -- '*.go'

lint:
	golangci-lint run --build-tags integration ./...
	GOOS=wasip1 GOARCH=wasm golangci-lint run ./guestsrc/...

# Unit and integration tests together, race detector on, random order, coverage over every package.
test:
	go test -race -shuffle=on -tags integration -covermode=atomic -coverpkg=./... -coverprofile=$(COVERAGE) ./...

coverage:
	scripts/coverage-gate.sh $(COVERAGE) $(COVERAGE_MIN)

# Allocation budgets (RULES.md §3). Coverage instrumentation changes inlining, so they run uninstrumented.
allocs:
	go test -race -shuffle=on -tags integration -count=1 -run 'DoNotAllocate' ./...

# Unit tests repeated to flush out flakiness and state leaking between runs (RULES.md §2).
repeat:
	go test -race -shuffle=on -count=10 ./...

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

# Build the Go guest artifact (yaegi + internal/goguest compiled to a wasip1 reactor), then compress it.
guests:
	GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -buildmode=c-shared -o $(GO_GUEST_RAW) ./guestsrc/golang
	go run scripts/gzipwasm.go $(GO_GUEST_RAW) $(GO_GUEST)

# Rebuild everything generated from source and fail if it differs from what is committed.
verify-guests: symbols guests
	git diff --exit-code -- $(GUEST_SYMBOLS) $(GO_GUEST)
	test -z "$$(git status --porcelain -- $(GUEST_SYMBOLS) $(GO_GUEST))"

clean:
	rm -rf $(BIN) $(COVERAGE) $(BENCH_OUT)
