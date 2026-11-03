# Gates and guest builds. `make check` runs exactly what CI runs (RULES.md §6).

SHELL := /bin/bash -o pipefail

# Pinned toolchain: guests are rebuilt with exactly this Go, so the committed artifact is reproducible.
export GOTOOLCHAIN := go1.27.3
export GOFLAGS := -mod=readonly

BIN := $(CURDIR)/bin
COVERAGE := coverage.out
COVERAGE_MIN := 95
BENCH_OUT := bench.txt

# The Go guest's standard library allowlist (PLAN.md). encoding/json is extracted separately: its Options
# interface has a method on an internal type, so its wrapper cannot be generated and is excluded.
GUEST_STDLIB := bufio bytes context encoding/base64 encoding/hex errors fmt io maps math math/big math/bits \
	math/cmplx math/rand math/rand/v2 net/url regexp slices sort strconv strings sync sync/atomic time unicode \
	unicode/utf16 unicode/utf8
GUEST_SYMBOLS := guestsrc/golang/symbols
GO_GUEST := guest/golang/golang.wasm

.PHONY: check fmt-check lint test coverage bench symbols guests verify-guests clean

check: fmt-check lint test coverage

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

bench:
	go test -run='^$$' -bench=. -benchmem -count=6 -tags integration ./... | tee $(BENCH_OUT)

$(BIN)/yaegi: go.mod go.sum
	go build -o $@ github.com/traefik/yaegi/cmd/yaegi

# Regenerate the guest's stdlib symbol tables with yaegi extract, for the wasip1 target.
symbols: $(BIN)/yaegi
	find $(GUEST_SYMBOLS) -name '*.go' ! -name symbols.go -delete
	cd $(GUEST_SYMBOLS) && GOOS=wasip1 GOARCH=wasm $(BIN)/yaegi extract -name symbols $(GUEST_STDLIB)
	cd $(GUEST_SYMBOLS) && GOOS=wasip1 GOARCH=wasm $(BIN)/yaegi extract -name symbols -exclude '^Options$$' encoding/json

# Build the Go guest artifact (yaegi + internal/goguest compiled to a wasip1 reactor).
guests:
	GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -buildmode=c-shared -o $(GO_GUEST) ./guestsrc/golang

# Rebuild everything generated from source and fail if it differs from what is committed.
verify-guests: symbols guests
	git diff --exit-code -- $(GUEST_SYMBOLS) $(GO_GUEST)
	test -z "$$(git status --porcelain -- $(GUEST_SYMBOLS) $(GO_GUEST))"

clean:
	rm -rf $(BIN) $(COVERAGE) $(BENCH_OUT)
