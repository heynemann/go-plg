// Package pricing is a shop's contract for pricing rules: plugins that quote orders, split amounts, look coupons
// up with the shop's coupon service and validate themselves against the shop's limits.
package pricing

import (
	"context"
	"time"
)

// Rule is implemented by pricing rule plugins.
//
//plg:limits timeout=3s
//plg:validation Limits
type Rule interface {
	// Quote prices order: its lines' total, less 10% for the customer's gold tier.
	Quote(ctx context.Context, order Order) (Quote, error)
	// Split splits cents in parts as equal as can be: the parts and what remains.
	Split(ctx context.Context, cents int, parts int) ([]int, int, error)
	// Coupon returns the percentage off of the coupon code, from the coupon service.
	Coupon(ctx context.Context, code string) (int, error)
	// Spin misbehaves as mode says: "panic" panics (or runs out of memory), "loop" never returns.
	Spin(ctx context.Context, mode string) error
}

// Order is an order to price.
type Order struct {
	Customer string            `json:"customer"`
	Lines    []Line            `json:"lines"`
	Placed   time.Time         `json:"placed"`
	Gift     *Gift             `json:"gift"`
	Tags     map[string]string `json:"tags"`
}

// Gift is the gift wrapping an order asks for.
type Gift struct {
	Message string `json:"message"`
}

// Line is an order's line.
type Line struct {
	SKU   string `json:"sku"`
	Qty   int    `json:"qty"`
	Cents int    `json:"cents"`
}

// Quote is an order's price.
type Quote struct {
	Total    int      `json:"total"`
	Discount int      `json:"discount"`
	Reasons  []string `json:"reasons"`
}

// Limits is the validation input: the largest discount the shop allows.
type Limits struct {
	MaxPercent int `json:"maxPercent"`
}

// Customers is the Host: the shop's customer directory.
type Customers interface {
	// Tier returns the customer's loyalty tier.
	Tier(ctx context.Context, customer string) (string, error)
}
