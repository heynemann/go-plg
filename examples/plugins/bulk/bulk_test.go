package bulk_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/examples/plugins/bulk"
	"github.com/heynemann/go-plg/examples/pricing"
	"github.com/heynemann/go-plg/plugin"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// A Go plugin is plain Go: its logic is unit-tested natively, without plg. (plg's guest skips _test.go files.)
func TestBulkDiscount(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		items []pricing.Item
		want  pricing.Discount
	}{
		"no items": {},
		"9 items": {
			items: []pricing.Item{{SKU: "a", Quantity: 9, UnitCents: 100}},
		},
		"10 items on one line": {
			items: []pricing.Item{{SKU: "a", Quantity: 10, UnitCents: 250}},
			want:  pricing.Discount{Cents: 250, Reason: "10 items: 10% off"},
		},
		"10 items over several lines": {
			items: []pricing.Item{{SKU: "a", Quantity: 4, UnitCents: 1000}, {SKU: "b", Quantity: 6, UnitCents: 5}},
			want:  pricing.Discount{Cents: 403, Reason: "10 items: 10% off"},
		},
	}

	rule, err := bulk.Create(nil)
	require.NoError(t, err)

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := rule.Discount(t.Context(), pricing.Order{Customer: "c", Items: tc.items})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBulkValidate(t *testing.T) {
	t.Parallel()

	rule, err := bulk.Create(nil)
	require.NoError(t, err)

	validator, ok := rule.(interface {
		Validate(ctx context.Context, limits pricing.Limits) ([]plugin.Issue, error)
	})
	require.True(t, ok, "the rule validates itself")

	issues, err := validator.Validate(t.Context(), pricing.Limits{MaxPercent: 10})
	require.NoError(t, err)
	assert.Empty(t, issues, "10% is within a 10% limit")

	issues, err = validator.Validate(t.Context(), pricing.Limits{MaxPercent: 5})
	require.NoError(t, err)
	assert.Equal(t, []plugin.Issue{{
		Severity: plugin.Error, Code: "bulk.percent", Message: "grants 10% off, over the 5% limit", Field: "percent",
	}}, issues)
}
