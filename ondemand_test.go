package plg_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

func TestRegisterRejectsInvalidOnDemandValidationPolicies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want string
		opts []plg.RegisterOption
	}{
		{
			name: "without WithValidation", want: "WithOnDemandValidationPolicy without WithValidation",
			opts: []plg.RegisterOption{plg.WithOnDemandValidationPolicy(plg.Unpublish)},
		},
		{
			name: "unknown policy", want: "WithOnDemandValidationPolicy with an unknown policy: 2",
			opts: []plg.RegisterOption{validateWith[pingCheck](), plg.WithOnDemandValidationPolicy(plg.Unpublish + 1)},
		},
		{
			name: "given twice", want: "WithOnDemandValidationPolicy given more than once",
			opts: []plg.RegisterOption{
				validateWith[pingCheck](), plg.WithOnDemandValidationPolicy(plg.Degrade),
				plg.WithOnDemandValidationPolicy(plg.Unpublish),
			},
		},
	}

	ctr := newUnitContainer(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := plg.Register[pinger](t.Context(), ctr, "ping", append(tc.opts, emptyStorage())...)
			require.ErrorIs(t, err, plg.ErrInvalidOption)
			require.ErrorContains(t, err, tc.want)
		})
	}

	for _, policy := range []plg.ValidationPolicy{plg.Degrade, plg.Unpublish} {
		name := []string{"degrade", "unpublish"}[policy]
		require.NoError(t, plg.Register[pinger](t.Context(), ctr, name, emptyStorage(), validateWith[pingCheck](),
			plg.WithOnDemandValidationPolicy(policy)), "policy %d with WithValidation", policy)
	}
}

func TestValidateNeedsAServedPluginAndAnOpenContainer(t *testing.T) {
	t.Parallel()

	ctr := plg.New()

	report, err := plg.Validate(t.Context(), ctr, "ping/missing")
	require.ErrorIs(t, err, plg.ErrNotFound)
	require.ErrorContains(t, err, "plg: validating ping/missing")
	assert.Zero(t, report)

	for range plg.ValidateAll(t.Context(), ctr) {
		assert.Fail(t, "an empty container has nothing to validate")
	}

	require.NoError(t, ctr.Close(t.Context()))

	_, err = plg.Validate(t.Context(), ctr, "ping/missing")
	require.ErrorIs(t, err, plg.ErrClosed)
}
