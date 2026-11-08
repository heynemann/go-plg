package plugin_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/secretabi"
	"github.com/heynemann/go-plg/plugin"
)

// errRefused is the host's refusal in TestSecretAsksTheGuestsHook.
var errRefused = errors.New(`plg: secret "other" is not declared`)

//nolint:paralleltest // it reads the guest's hook, which TestSecretAsksTheGuestsHook sets
func TestSecretOutsidePlgFailsWithErrNoGuest(t *testing.T) {
	require.Nil(t, secretabi.Read, "no guest set the hook")

	value, err := plugin.Secret(t.Context(), "api_key")
	require.ErrorIs(t, err, plugin.ErrNoGuest)
	assert.Empty(t, value)
}

//nolint:paralleltest // it sets the guest's hook, a package variable
func TestSecretAsksTheGuestsHook(t *testing.T) {
	secretabi.Read = func(name string) (string, error) {
		if name == "api_key" {
			return "k", nil
		}

		return "", errRefused
	}

	t.Cleanup(func() { secretabi.Read = nil })

	value, err := plugin.Secret(t.Context(), "api_key")
	require.NoError(t, err)
	assert.Equal(t, "k", value)

	_, err = plugin.Secret(t.Context(), "other")
	require.ErrorIs(t, err, errRefused, "the host's reason reaches the plugin")
}
