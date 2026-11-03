// Package pricingtest is what the storage examples' tests share: a host for the pricing rules and helpers that
// register the rules of examples/plugins through a storage and call them, so each storage is proven with real
// plugins in every language.
package pricingtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/examples/pricing"
	_ "github.com/heynemann/go-plg/guest/golang"     // runs the Go rule
	_ "github.com/heynemann/go-plg/guest/lua"        // runs the Lua rule
	_ "github.com/heynemann/go-plg/guest/typescript" // runs the TypeScript rule
)

// Name is the name the helpers register the rules under.
const Name = "pricing"

// Tiers is a pricing.Customers host: the tier of each customer.
type Tiers map[string]string

// Tier returns the customer's tier, "" for none.
func (t Tiers) Tier(_ context.Context, customer string) (string, error) {
	return t[customer], nil
}

// Register registers the rules of storage in a new container, closed when the test ends.
func Register(t *testing.T, storage plg.Storage) *plg.Container {
	t.Helper()

	container := plg.New()

	t.Cleanup(func() { require.NoError(t, container.Close(context.WithoutCancel(t.Context()))) })

	err := plg.Register[pricing.Rule](t.Context(), container, Name,
		plg.WithStorage(storage),
		plg.WithHost[pricing.Customers](Tiers{"ada": "gold"}),
		plg.WithPool(1, 1),
	)
	require.NoError(t, err)

	return container
}

// The order of [Order] and what each rule grants it: its 12 items × 450 cents (5400) get 10% off from bulk, the
// WELCOME5 coupon's 500 cents, and 5% off for ada, a gold customer.
const (
	quantity     = 12
	unitCents    = 450
	bulkCents    = 540
	couponCents  = 500
	loyaltyCents = 270
)

// Order is an order on which every rule of examples/plugins grants a discount.
func Order() pricing.Order {
	return pricing.Order{
		Customer: "ada",
		Coupon:   "WELCOME5",
		Items:    []pricing.Item{{SKU: "tea", Quantity: quantity, UnitCents: unitCents}},
	}
}

// Discounts returns what each rule of examples/plugins grants [Order], by plugin key.
func Discounts() map[string]pricing.Discount {
	return map[string]pricing.Discount{
		Name + "/bulk":    {Cents: bulkCents, Reason: "12 items: 10% off"},
		Name + "/coupon":  {Cents: couponCents, Reason: "coupon WELCOME5"},
		Name + "/loyalty": {Cents: loyaltyCents, Reason: "gold customer: 5% off"},
	}
}

// Quote calls every rule registered in container with [Order] and returns their discounts by plugin key.
func Quote(t *testing.T, container *plg.Container) map[string]pricing.Discount {
	t.Helper()

	got := make(map[string]pricing.Discount)

	for key, rule := range plg.All[pricing.Rule](t.Context(), container) {
		discount, err := rule.Discount(t.Context(), Order())
		require.NoError(t, err, key)

		got[key] = discount
	}

	return got
}
