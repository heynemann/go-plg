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

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/examples/pricing"
	_ "github.com/heynemann/go-plg/guest/golang"     // runs the Go rules
	_ "github.com/heynemann/go-plg/guest/lua"        // runs the Lua rules
	_ "github.com/heynemann/go-plg/guest/typescript" // runs the TypeScript rules
)

const (
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
)

// serve registers the rules of cfg.storage and serves the quote API on listener until ctx ends, then shuts down:
// it stops accepting requests, lets those in flight finish and closes the rules' instances.
func serve(ctx context.Context, listener net.Listener, cfg config) error {
	//nolint:contextcheck // plg.New takes no context: it only builds the container (and its metric instruments)
	container := plg.New(plg.WithLogger(cfg.logger), plg.WithMemoryBudget(memoryBudget))

	err := plg.Register[pricing.Rule](ctx, container, registration,
		plg.WithStorage(cfg.storage),
		plg.WithHost[pricing.Customers](customers{"ada": "gold", "bob": "silver"}),
		// Every instance starts here, before serving: starting one runs the rule's Create (a Go rule's source is
		// interpreted then), which takes far longer than a call and, on a slow or busy machine, longer than
		// ruleTimeout. With the pool at its maximum from the start, a request takes an idle instance or waits for
		// one instead of starting it under its own deadline (only a request arriving while plg replaces an instance
		// a failing rule lost may still start one). The price is every instance's memory from the start.
		plg.WithPool(maxInstances, maxInstances),
		plg.WithPollInterval(cfg.poll),
	)
	if err != nil {
		return errors.Join(err, listener.Close(), container.Close(ctx))
	}

	srv := &http.Server{
		Handler:           handler(container, cfg.logger),
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

// handler serves POST /quote: the order in the body, priced by every rule of container.
func handler(container *plg.Container, logger *slog.Logger) http.Handler {
	strictJSON := sonic.Config{DisallowUnknownFields: true, ValidateString: true}.Froze()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /quote", func(writer http.ResponseWriter, req *http.Request) {
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

		err = strictJSON.NewEncoder(writer).Encode(price(req.Context(), container, order))
		if err != nil { // the client is gone: the answer has started, nothing else can be sent
			logger.DebugContext(req.Context(), "writing a quote failed", slog.Any("error", err))
		}
	})

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
