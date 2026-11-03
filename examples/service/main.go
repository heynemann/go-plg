// Command service is a small pricing service whose rules are plugins written in Go, Lua and TypeScript, all
// implementing one interface, pricing.Rule. POST /quote prices an order with every rule and applies the largest
// discount:
//
//	go run ./service -addr localhost:8080
//	curl -s localhost:8080/quote -d '{"customer":"ada","coupon":"WELCOME5",
//	  "items":[{"sku":"tea","quantity":12,"unitCents":450}]}'
//
// By default it serves the rules embedded from examples/plugins. With -plugins <dir> it serves a directory of
// plugins in the same layout instead and, with -poll <interval>, follows it: rules added, changed or removed
// there are served without a restart. SIGINT or SIGTERM stops it cleanly.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// main only wires the process to run; the tests run it as a program (TestServiceProgram) and run in process.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)

	stop()
	os.Exit(code)
}
