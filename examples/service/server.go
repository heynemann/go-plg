package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/bytedance/sonic"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/examples/pricing"
	_ "github.com/heynemann/go-plg/guest/golang"     // runs the Go rules
	_ "github.com/heynemann/go-plg/guest/lua"        // runs the Lua rules
	_ "github.com/heynemann/go-plg/guest/typescript" // runs the TypeScript rules
)

const (
	// tracerName is the instrumentation scope of the service's own spans.
	tracerName = "github.com/heynemann/go-plg/examples/service"
	// registration is the name the rules are registered under: their keys are "pricing/<plugin directory>".
	registration = "pricing"
	// memoryBudget caps the memory of every rule instance together; Register refuses rules beyond it.
	memoryBudget = 2 << 30
	// maxInstances is how many calls each rule serves at once.
	maxInstances = 4
	// ruleTimeout bounds each rule call, including its wait for an idle instance: a rule looping forever is
	// stopped there, and its instance replaced.
	ruleTimeout = 2 * time.Second
	// shutdownTimeout bounds the wait for requests and rule calls in flight once the service is asked to stop.
	shutdownTimeout = 10 * time.Second
	readTimeout     = 10 * time.Second
	maxBodyBytes    = 1 << 20
	// maxPercent and maxCents are what every rule may grant (pricing.Limits): each rule's Validate checks itself
	// against them before it goes live.
	maxPercent = 10
	maxCents   = 1000
)

// serve registers the rules of cfg.storage and serves the quote API on listener until ctx ends, then shuts down:
// it stops accepting requests, lets those in flight finish and closes the rules' instances.
func serve(ctx context.Context, listener net.Listener, cfg *config) error {
	//nolint:contextcheck // plg.New takes no context: it only builds the container (and its metric instruments)
	container := plg.New(
		plg.WithLogger(cfg.logger),
		plg.WithMemoryBudget(memoryBudget),
		plg.WithOnEvent(logEvent(cfg.logger)),
		// Each rule call is a span plg.call <key>.<method>, a child of the quote request's span (see handler), with
		// the rule's HTTP requests (plg.http <method>) below it, each carrying its span's traceparent.
		plg.WithTracerProvider(cfg.tracerProvider),
	)

	err := plg.Register[pricing.Rule](ctx, container, registration, registerOptions(cfg)...)
	if err != nil {
		return errors.Join(err, listener.Close(), container.Close(ctx))
	}

	srv := &http.Server{
		Handler:           handler(container, cfg),
		ReadHeaderTimeout: readTimeout,
		ReadTimeout:       readTimeout,
		ErrorLog:          slog.NewLogLogger(cfg.logger.Handler(), slog.LevelWarn),
	}
	served := make(chan error, 1)

	go func() { served <- srv.Serve(listener) }()

	cfg.logger.InfoContext(ctx, "serving", slog.String("addr", listener.Addr().String()))

	select {
	case <-ctx.Done():
	case err = <-served:
		return errors.Join(err, container.Close(ctx))
	}

	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	err = srv.Shutdown(stopCtx)

	<-served // http.ErrServerClosed once Shutdown began

	return errors.Join(err, container.Close(stopCtx))
}

// registerOptions returns the options the rules of cfg are registered with.
func registerOptions(cfg *config) []plg.RegisterOption {
	opts := []plg.RegisterOption{
		plg.WithStorage(cfg.storage),
		plg.WithHost[pricing.Customers](customers{"ada": "gold", "bob": "silver"}),
		// Every rule validates itself against the service's limits before it goes live, at startup and on each
		// reload: a rule that could grant more is rejected (startup fails; a reload keeps its last good version).
		plg.WithValidation(func(context.Context, plg.PluginInfo) (pricing.Limits, error) {
			return pricing.Limits{MaxPercent: maxPercent, MaxCents: maxCents}, nil
		}),
		// Every rule names its owning team and a way to reach it in its plugin.yaml, or it does not go live.
		plg.WithRequiredOwnership(),
		// Every instance starts here, before serving: starting one runs the rule's Create (a Go rule's source is
		// interpreted then), which takes far longer than a call and, on a slow or busy machine, longer than
		// ruleTimeout. With the pool at its maximum from the start, a request takes an idle instance or waits for
		// one instead of starting it under its own deadline (only a request arriving while plg replaces an instance
		// a failing rule lost may still start one). The price is every instance's memory from the start.
		plg.WithPool(maxInstances, maxInstances),
		plg.WithPollInterval(cfg.poll),
		// The secrets each rule's plugin.yaml declares come from the environment, by rule (see envSecrets).
		plg.WithSecrets(envSecrets(cfg.lookupEnv)),
	}

	if cfg.couponAPI != "" { // the only host the rules may reach; without it, every request is refused
		opts = append(opts, plg.WithHTTP(plg.HTTPPolicy{
			AllowedHosts:         []string{cfg.couponAPI},
			BlockPrivateNetworks: new(!cfg.couponAPIPrivate),
		}))
	}

	return opts
}

// customers is the service's side of pricing.Customers, the host the rules call: a fixed table of tiers.
type customers map[string]string

func (cs customers) Tier(_ context.Context, customer string) (string, error) {
	return cs[customer], nil
}

// Limits of an order, so that every amount stays an exact integer in Lua and TypeScript too (below 2^53).
const (
	maxLines     = 1000
	maxQuantity  = 1_000_000
	maxUnitCents = 1_000_000_000
	maxSubtotal  = 1_000_000_000_000_000
)

// quote is the answer to POST /quote: every rule's discount, in key order, and the one applied.
type quote struct {
	Applied       string       `json:"applied"`
	Discounts     []ruleResult `json:"discounts"`
	SubtotalCents int64        `json:"subtotalCents"`
	TotalCents    int64        `json:"totalCents"`
}

// ruleResult is one rule's verdict: a discount, or the error the rule failed with.
type ruleResult struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	Cents  int64  `json:"cents"`
}

// handler serves POST /quote: the order in the body, priced by every rule of container; and, with an admin token,
// the admin endpoints (see mountAdmin).
//
// Each quote request is a server span "POST /quote" of cfg.tracerProvider, continuing the trace of the request's
// W3C traceparent header when it has one: the rules' calls are its children, so a quote is one trace from the
// client through the service and its rules to the APIs they call.
func handler(container *plg.Container, cfg *config) http.Handler {
	logger := cfg.logger
	tracer := cfg.tracerProvider.Tracer(tracerName)
	strictJSON := sonic.Config{DisallowUnknownFields: true, ValidateString: true}.Froze()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /quote", func(writer http.ResponseWriter, req *http.Request) {
		ctx := propagation.TraceContext{}.Extract(req.Context(), propagation.HeaderCarrier(req.Header))
		ctx, span := tracer.Start(ctx, "POST /quote", trace.WithSpanKind(trace.SpanKindServer))

		defer span.End()

		var order pricing.Order

		err := strictJSON.NewDecoder(http.MaxBytesReader(writer, req.Body, maxBodyBytes)).Decode(&order)
		if err == nil {
			err = validate(order)
		}

		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)

			return
		}

		writer.Header().Set("Content-Type", "application/json")

		err = strictJSON.NewEncoder(writer).Encode(price(ctx, container, order))
		if err != nil { // the client is gone: the answer has started, nothing else can be sent
			logger.DebugContext(ctx, "writing a quote failed", slog.Any("error", err))
		}
	})

	if cfg.adminToken != "" {
		mountAdmin(mux, container, cfg)
	}

	return mux
}

var errOrder = errors.New("invalid order")

// validate checks the order's limits.
func validate(order pricing.Order) error {
	if len(order.Items) == 0 || len(order.Items) > maxLines {
		return fmt.Errorf("%w: it needs 1 to %d items", errOrder, maxLines)
	}

	for i, item := range order.Items {
		if item.Quantity < 1 || item.Quantity > maxQuantity || item.UnitCents < 0 || item.UnitCents > maxUnitCents {
			return fmt.Errorf("%w: item %d: quantity must be 1 to %d and unitCents 0 to %d",
				errOrder, i, maxQuantity, maxUnitCents)
		}
	}

	if pricing.Subtotal(order) > maxSubtotal {
		return fmt.Errorf("%w: the subtotal exceeds %d cents", errOrder, maxSubtotal)
	}

	return nil
}

// price asks every rule for its discount and applies the largest one. A rule that fails is reported and skipped:
// its error is the plugin's own message (a *plg.PluginError), or a *plg.PanicError if it crashed or ran out of
// time, in which case plg has already replaced its instance.
func price(ctx context.Context, container *plg.Container, order pricing.Order) quote {
	subtotal := pricing.Subtotal(order)
	answer := quote{SubtotalCents: subtotal, TotalCents: subtotal, Discounts: []ruleResult{}}

	var best int64

	for key, rule := range plg.All[pricing.Rule](ctx, container) {
		discount, err := discount(ctx, rule, order)
		if err != nil {
			answer.Discounts = append(answer.Discounts, ruleResult{Rule: key, Error: err.Error()})

			continue
		}

		answer.Discounts = append(
			answer.Discounts,
			ruleResult{Rule: key, Cents: discount.Cents, Reason: discount.Reason},
		)

		if discount.Cents > best && discount.Cents <= subtotal {
			best = discount.Cents
			answer.Applied = key
			answer.TotalCents = subtotal - best
		}
	}

	return answer
}

func discount(ctx context.Context, rule pricing.Rule, order pricing.Order) (pricing.Discount, error) {
	ctx, cancel := context.WithTimeout(ctx, ruleTimeout)
	defer cancel()

	return rule.Discount(ctx, order) //nolint:wrapcheck // the rule's error is reported as it is
}
