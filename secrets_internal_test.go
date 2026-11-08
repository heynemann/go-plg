package plg

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/goguest"
)

// testSecrets returns the secrets of a plugin version declaring names, resolved by resolve and cached for ttl, and
// the log their failures go to.
func testSecrets(t *testing.T, resolve secretResolver, ttl time.Duration, names ...string) (*pluginSecrets,
	*bytes.Buffer,
) {
	t.Helper()

	var logs bytes.Buffer

	cfg := &registerConfig{resolve: resolve, secretTTL: ttl}
	info := PluginInfo{Key: "s/p", Version: "v1", Language: LanguageLua}

	return newPluginSecrets(cfg, &info, names, nil, slog.New(slog.NewJSONHandler(&logs, nil)), nil), &logs
}

// read reads the secret name as a guest does: through serve, decoding its answer.
func read(t *testing.T, s *pluginSecrets, name string) (string, error) {
	t.Helper()

	return goguest.SecretAnswer(s.serve(t.Context(), nil, []byte(name))) //nolint:wrapcheck // the guest's own error
}

func TestOversizedSecretsFailTheRead(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("v", MaxSecretSize+1)
	secrets, logs := testSecrets(t, func(context.Context, PluginInfo, string) (string, error) { return huge, nil },
		secretTTL, "big")

	_, err := read(t, secrets, "big")
	require.EqualError(t, err, `plg: secret "big" could not be resolved (the service's log says why)`)
	assert.Contains(t, logs.String(), `"secret":"big","error":"the resolver returned a value over the 65536-byte `+
		`limit: 65537 bytes"`)
	assert.Equal(t, "x", secrets.redactor().String("x"), "a refused value is not recorded")

	exact := strings.Repeat("v", MaxSecretSize)
	secrets, _ = testSecrets(t, func(context.Context, PluginInfo, string) (string, error) { return exact, nil },
		secretTTL, "big")

	got, err := read(t, secrets, "big")
	require.NoError(t, err, "the limit itself is allowed")
	assert.Equal(t, exact, got)
}

func TestUndeclaredSecretNamesAreRefusedBriefly(t *testing.T) {
	t.Parallel()

	unexpected := func(context.Context, PluginInfo, string) (string, error) {
		t.Error("an undeclared name reached the resolver")

		return "", nil
	}

	secrets, _ := testSecrets(t, unexpected, secretTTL, "a", "b")
	long := strings.Repeat("n", 1<<20)

	_, err := read(t, secrets, long)
	require.EqualError(t, err, "plg: a secret name of 1048576 bytes is not declared in the plugin's plugin.yaml "+
		"(it declares a, b)", "a huge name is not echoed")

	none, _ := testSecrets(t, unexpected, secretTTL)

	_, err = read(t, none, "a")
	require.EqualError(t, err, `plg: secret "a" is not declared in the plugin's plugin.yaml (it declares none)`)
	assert.Nil(t, none.redactor(), "a version declaring no secrets scrubs nothing")
}

func TestWithoutACacheEveryReadResolves(t *testing.T) {
	t.Parallel()

	var n int

	secrets, _ := testSecrets(t, func(context.Context, PluginInfo, string) (string, error) {
		n++

		return strings.Repeat("x", n) + "-value", nil
	}, 0, "k")

	for want := range 3 {
		got, err := read(t, secrets, "k")
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("x", want+1)+"-value", got)
	}

	assert.Equal(t, "[secret k] [secret k]", secrets.redactor().String("x-value xxx-value"),
		"every value read is scrubbed")
}
