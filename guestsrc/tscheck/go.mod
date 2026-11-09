// The TypeScript checker plg validate runs: Microsoft's TypeScript compiler in Go (typescript-go), whose command
// `make ts-checker` compiles to wasip1 into internal/tscheck. A module of its own, so plg's go.mod never requires
// the compiler; it holds nothing but this pin.
module github.com/heynemann/go-plg/guestsrc/tscheck

go 1.27

toolchain go1.27.3

tool github.com/microsoft/typescript-go/cmd/tsgo

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/go-json-experiment/json v0.0.0-20260623181947-01eb4420fa68 // indirect
	github.com/klauspost/cpuid/v2 v2.2.10 // indirect
	github.com/mackerelio/go-osstat v0.2.7 // indirect
	github.com/microsoft/typescript-go v0.0.0-20260820064610-89d5d5b2849a // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/term v0.44.0 // indirect
	golang.org/x/text v0.38.0 // indirect
)
