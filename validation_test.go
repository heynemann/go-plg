package plg_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/plugin"
)

// Validation inputs for the WithValidation checks, declared next to pinger as a contract's must be.
type (
	pingCheck struct {
		Limits []int
		Max    int
	}
	pingChan       chan int
	pingBox[T any] struct{ V T }
)

// validateWith returns a WithValidation option whose function returns V's zero value.
func validateWith[V any]() plg.RegisterOption {
	return plg.WithValidation(func(context.Context, plg.PluginInfo) (V, error) {
		var zero V

		return zero, nil
	})
}

func TestRegisterAcceptsAValidationInputFromTheContractPackage(t *testing.T) {
	t.Parallel()

	err := plg.Register[pinger](t.Context(), newUnitContainer(t), "ping", emptyStorage(), validateWith[pingCheck](),
		plg.WithValidationTimeout(1))
	require.NoError(t, err)
}

func TestRegisterRejectsInvalidValidationOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want string
		opts []plg.RegisterOption
	}{
		{
			name: "unnamed type",
			want: "WithValidation input struct { Max int } is not a named data type declared in " +
				"the contract package github.com/heynemann/go-plg_test",
			opts: []plg.RegisterOption{validateWith[struct{ Max int }]()},
		},
		{
			name: "predeclared type", want: "WithValidation input int is not a named data type",
			opts: []plg.RegisterOption{validateWith[int]()},
		},
		{
			name: "pointer", want: "WithValidation input *plg_test.pingCheck is not a named data type",
			opts: []plg.RegisterOption{validateWith[*pingCheck]()},
		},
		{
			name: "generic type", want: "WithValidation input plg_test.pingBox[int] is not a named data type",
			opts: []plg.RegisterOption{validateWith[pingBox[int]]()},
		},
		{
			name: "not a data type", want: "WithValidation input plg_test.pingChan is not a named data type " +
				"declared in the contract package github.com/heynemann/go-plg_test: plg_test.pingChan is not a data " +
				"type (kind chan)",
			opts: []plg.RegisterOption{validateWith[pingChan]()},
		},
		{
			name: "interface", want: "WithValidation input plg_test.pinger is not a named data type",
			opts: []plg.RegisterOption{validateWith[pinger]()},
		},
		{
			name: "declared in another package",
			want: "WithValidation input testcontract.Check is not a named data type declared in the contract " +
				"package github.com/heynemann/go-plg_test of plg_test.pinger, whose source Go plugins interpret; it is " +
				"in github.com/heynemann/go-plg/internal/testcontract",
			opts: []plg.RegisterOption{validateWith[testcontract.Check]()},
		},
		{
			name: "type the codec rejects", want: "WithValidation input plg_test.clash: ",
			opts: []plg.RegisterOption{validateWith[clash]()},
		},
		{
			name: "nil function", want: "WithValidation with a nil function",
			opts: []plg.RegisterOption{plg.WithValidation[pingCheck](nil)},
		},
		{
			name: "given twice", want: "WithValidation given more than once",
			opts: []plg.RegisterOption{validateWith[pingCheck](), validateWith[pingCheck]()},
		},
		{
			name: "zero timeout", want: "WithValidationTimeout must be positive, got 0s",
			opts: []plg.RegisterOption{validateWith[pingCheck](), plg.WithValidationTimeout(0)},
		},
		{
			name: "negative timeout", want: "WithValidationTimeout must be positive, got -1ns",
			opts: []plg.RegisterOption{plg.WithValidationTimeout(-1)},
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
}

func TestIsValidationOnlyForAValidationContext(t *testing.T) {
	t.Parallel()

	assert.False(t, plg.IsValidation(t.Context()))
	assert.False(t, plg.IsValidation(context.WithValue(t.Context(), hostCtxKey{}, true)))
}

// TestPluginIssueMatchesTheReportsIssue holds plugin.Issue, which Go plugins return, to plg.Issue's layout: the
// issues cross the plugin boundary field by field, so both must keep the same fields in the same order, and the
// severities the same values.
func TestPluginIssueMatchesTheReportsIssue(t *testing.T) {
	t.Parallel()

	host, guest := reflect.TypeFor[plg.Issue](), reflect.TypeFor[plugin.Issue]()
	require.Equal(t, host.NumField(), guest.NumField())

	for i := range host.NumField() {
		assert.Equal(t, host.Field(i).Name, guest.Field(i).Name, "field %d", i)
		assert.Equal(t, host.Field(i).Type.Kind(), guest.Field(i).Type.Kind(), "field %s", host.Field(i).Name)
	}

	assert.Equal(t, string(plg.SeverityError), string(plugin.Error))
	assert.Equal(t, string(plg.SeverityWarning), string(plugin.Warning))
	assert.Equal(t, string(plg.SeverityInfo), string(plugin.Info))
}
