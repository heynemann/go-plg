// Package bundle is a pricing rule written in Go: orders of at least three different products get 5% off.
package bundle

import (
	"context"
	"fmt"

	"github.com/heynemann/go-plg/examples/pricing"
	"github.com/heynemann/go-plg/plugin"
)

const (
	minProducts = 3
	percent     = 5
)

type rule struct{}

// Create returns the rule; it does not call the service's Host.
func Create(pricing.Customers) (pricing.Rule, error) {
	return rule{}, nil
}

// Validate rejects the rule when the service lets rules grant less than its percentage.
func (rule) Validate(_ context.Context, limits pricing.Limits) ([]plugin.Issue, error) {
	if percent > limits.MaxPercent {
		return []plugin.Issue{{
			Severity: plugin.Error,
			Code:     "bundle.percent",
			Message:  fmt.Sprintf("grants %d%% off, over the %d%% limit", percent, limits.MaxPercent),
			Field:    "percent",
		}}, nil
	}

	return nil, nil
}

func (rule) Discount(_ context.Context, order pricing.Order) (pricing.Discount, error) {
	products := map[string]bool{}
	for _, item := range order.Items {
		products[item.SKU] = true
	}

	if len(products) < minProducts {
		return pricing.Discount{}, nil
	}

	return pricing.Discount{
		Cents:  pricing.Subtotal(order) * percent / 100,
		Reason: fmt.Sprintf("%d products: %d%% off", len(products), percent),
	}, nil
}
