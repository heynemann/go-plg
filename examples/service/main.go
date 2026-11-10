// Command service is a small pricing service whose rules are plugins written in Go, Lua and TypeScript, all
// implementing one interface, pricing.Rule. POST /quote prices an order with every rule and applies the largest
// discount:
//
//	go run ./service -addr localhost:8080
//	curl -s localhost:8080/quote -d '{"customer":"ada","coupon":"WELCOME5",
//	  "items":[{"sku":"tea","quantity":12,"unitCents":450}]}'
//
// By default it serves the rules embedded from examples/plugins. With -plugins <dir> it serves a directory of
// plugins in the same layout instead, and with -artifacts <dir> the .plg artifacts plg pack wrote there (those of
// the example author repository: examples/rules/ci.sh packs them into examples/rules/dist). With -poll <interval>
// it follows either directory: rules added, changed or removed there are served without a restart. SIGINT or
// SIGTERM stops it cleanly.
//
// Every rule validates itself against the service's limits (pricing.Limits, plg.WithValidation) and names its
// owning team and contact in its plugin.yaml (plg.WithRequiredOwnership) before it goes live; a rule that fails
// either is rejected. Every plugin lifecycle event is logged with the team to tell (plg.WithOnEvent). With
// -admin-token (or $PRICING_ADMIN_TOKEN), requests bearing it get the admin endpoints:
//
//	curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/admin/plg/plugins     # plg.AdminHandler
//	curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/admin/check/new-rule \
//	  --data-binary @rule.zip                                                     # plg.Check of a zipped rule
//
// Rules may call one HTTP API, the coupon API whose host -coupon-api names (plg.WithHTTP; -coupon-api-private
// when it is on a private network), with the secrets their plugin.yaml declares (plg.WithSecrets): the secret
// <name> of the rule pricing/<rule> is the environment variable PRICING_SECRET_<RULE>_<NAME>, upper-cased, every
// character but a letter or a digit becoming '_'. The seasonal rule of examples/rules reads two:
//
//	PRICING_SECRET_SEASONAL_COUPON_API_URL=http://coupons.internal:8080 \
//	PRICING_SECRET_SEASONAL_COUPON_API_TOKEN=... \
//	  go run ./service -artifacts rules/dist -coupon-api coupons.internal:8080 -coupon-api-private
//
// The service never logs a secret's value, and plg replaces any value a rule read with [secret <name>] in what it
// reports of the rule (its output, errors, HTTP log records and spans).
//
// Each quote request is a server span continuing the request's traceparent, each rule call a span below it and
// each request a rule makes one below that, carrying its traceparent on to the API. The spans go to the global
// OpenTelemetry tracer provider: a deployment installs its SDK and exporter there (otel.SetTracerProvider) before
// run; until then, tracing costs nothing.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.opentelemetry.io/otel"
)

// main only wires the process to run; the tests run it as a program (TestServiceProgram) and run in process.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], process{
		stderr: os.Stderr, tracerProvider: otel.GetTracerProvider(), lookupEnv: os.LookupEnv,
	})

	stop()
	os.Exit(code)
}
