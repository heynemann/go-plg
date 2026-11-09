package plugin_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/plugin"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestSecretOutsidePlgFailsWithErrNoGuest(t *testing.T) {
	t.Parallel()

	value, err := plugin.Secret(t.Context(), "api_key")
	require.ErrorIs(t, err, plugin.ErrNoGuest)
	assert.Empty(t, value)
}
