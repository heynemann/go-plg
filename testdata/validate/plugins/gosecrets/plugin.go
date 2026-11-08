// Package gosecrets is gopricing whose Coupon authenticates to the coupon service with the secret coupon_token,
// like luasecrets and tssecrets: the three run the same test cases (testdata/validate/secretcases).
package gosecrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"example.com/shop/pricing"
	"github.com/heynemann/go-plg/plugin"
)

type rule struct{ customers pricing.Customers }

// Create keeps the customer directory.
func Create(customers pricing.Customers) (pricing.Rule, error) {
	return &rule{customers: customers}, nil
}

func (r *rule) Quote(ctx context.Context, order pricing.Order) (pricing.Quote, error) {
	if len(order.Lines) == 0 {
		return pricing.Quote{}, errors.New("empty order")
	}

	total := 0
	for _, line := range order.Lines {
		total += line.Qty * line.Cents
	}

	tier, err := r.customers.Tier(ctx, order.Customer)
	if err != nil {
		return pricing.Quote{}, err
	}

	quote := pricing.Quote{Total: total}
	if tier == "gold" {
		quote.Discount = total / 10
		quote.Reasons = append(quote.Reasons, "gold")
	}

	if order.Gift != nil {
		quote.Reasons = append(quote.Reasons, "gift")
	}

	quote.Total -= quote.Discount

	return quote, nil
}

func (r *rule) Split(_ context.Context, cents int, parts int) ([]int, int, error) {
	if parts <= 0 {
		return nil, 0, errors.New("no parts")
	}

	split := make([]int, parts)
	for i := range split {
		split[i] = cents / parts
	}

	return split, cents % parts, nil
}

func (r *rule) Coupon(ctx context.Context, code string) (int, error) {
	token, err := plugin.Secret(ctx, "coupon_token")
	if err != nil {
		return 0, err
	}

	url := "https://coupons.example.com/codes/" + code + "?token=" + token

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return strconv.Atoi(string(body))
	case http.StatusNotFound:
		return 0, errors.New("unknown coupon " + code)
	default:
		return 0, fmt.Errorf("coupon service: %d", resp.StatusCode)
	}
}

func (r *rule) Spin(_ context.Context, mode string) error {
	switch mode {
	case "panic":
		panic("spun out")
	case "loop":
		for {
		}
	default:
		return nil
	}
}

func (r *rule) Validate(_ context.Context, in pricing.Limits) ([]plugin.Issue, error) {
	if in.MaxPercent < 10 {
		return []plugin.Issue{{
			Severity: plugin.Error, Code: "gold.percent", Field: "maxPercent",
			Message: fmt.Sprintf("the gold discount (10%%) is over the limit (%d%%)", in.MaxPercent),
		}}, nil
	}

	return nil, nil
}
