// Package bulk is a pricing rule written in Go: orders of at least 10 items get 10% off. It is an ordinary Go
// package, type-checked and unit-tested natively like any other; plg runs the same source interpreted inside the
// Go guest.
package bulk

import (
	"context"
	"fmt"

	"github.com/heynemann/go-plg/examples/pricing"
)

const (
	minItems = 10
	// divisor takes 10% off: the discount is the subtotal divided by it.
	divisor = 10
)

type rule struct{}

// Create returns the rule. Its parameter is the service's host (plg.WithHost), which this rule does not need.
//
//nolint:ireturn // plg calls Create for the interface the plugin implements
func Create(pricing.Customers) (pricing.Rule, error) {
	return rule{}, nil
}

func (rule) Discount(_ context.Context, order pricing.Order) (pricing.Discount, error) {
	count := 0
	for _, item := range order.Items {
		count += item.Quantity
	}

	if count < minItems {
		return pricing.Discount{}, nil
	}

	return pricing.Discount{
		Cents:  pricing.Subtotal(order) / divisor,
		Reason: fmt.Sprintf("%d items: 10%% off", count),
	}, nil
}
