package platform

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

// New refuses a Config it cannot run with before it registers anything: no user directory, or a wallet URL
// without a host to allow.
func TestNewRefusesAnInvalidConfig(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, message string
		cfg           Config
	}{
		{name: "no Users directory", cfg: Config{}, message: "no Users directory"},
		{
			name: "a WalletURL without a host", cfg: Config{Users: directory{}, WalletURL: "wallet/api"},
			message: `WalletURL "wallet/api" is not an absolute URL`,
		},
		{
			name: "a WalletURL that does not parse", cfg: Config{Users: directory{}, WalletURL: "http://[::1"},
			message: "is not an absolute URL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			started, err := New(t.Context(), &tc.cfg)

			require.ErrorIs(t, err, ErrConfig)
			require.ErrorContains(t, err, tc.message)
			assert.Nil(t, started)
		})
	}
}

// A campaign without an ID is refused before the platform looks up any plugin: its idempotency keys would be
// "/<member>", shared with every other campaign without one.
func TestRunRefusesACampaignWithoutAnID(t *testing.T) {
	t.Parallel()

	run, err := (&Platform{}).newRun(t.Context(), &Campaign{Cohort: "c", Deliverer: "d"})

	require.ErrorIs(t, err, ErrCampaign)
	require.ErrorContains(t, err, "no ID")
	assert.Nil(t, run)
}

// The platform's resolver answers the wallet's URL itself and asks Config.Secrets for the others; without
// Config.Secrets, every other read fails, and so does the wallet's URL when there is none.
func TestSecretsResolvesTheWalletURLAndAsksForTheRest(t *testing.T) {
	t.Parallel()

	info := plg.PluginInfo{Key: "deliveries/wallet"}
	asked := func(_ context.Context, got plg.PluginInfo, name string) (string, error) {
		return got.Key + ":" + name, nil
	}

	for _, tc := range []struct {
		resolve   func(context.Context, plg.PluginInfo, string) (string, error)
		name      string
		walletURL string
		secret    string
		want      string
	}{
		{name: "the wallet's URL", walletURL: "http://wallet", secret: WalletURLSecret, want: "http://wallet"},
		{name: "another secret", walletURL: "http://wallet", resolve: asked, secret: "token", want: "deliveries/wallet:token"},
		{name: "the wallet's URL when there is none", resolve: asked, secret: WalletURLSecret, want: "deliveries/wallet:" + WalletURLSecret},
		{name: "another secret without a resolver", walletURL: "http://wallet", secret: "token"},
		{name: "no wallet URL and no resolver", secret: WalletURLSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			value, err := secrets(tc.walletURL, tc.resolve)(t.Context(), info, tc.secret)
			if tc.want == "" {
				require.ErrorIs(t, err, errNoSecret)
				assert.Empty(t, value)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, value)
		})
	}
}

// Zero limits take their defaults; PoolMax defaults to PoolMin when that is above the default pool, so that a
// larger PoolMin alone is a valid pool.
func TestLimitsDefaults(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		limits           Limits
		poolMin, poolMax int
	}{
		{name: "none set", poolMin: defaultPool, poolMax: defaultPool},
		{name: "a PoolMin above the default", limits: Limits{PoolMin: 8}, poolMin: 8, poolMax: 8},
		{name: "a PoolMin below the default", limits: Limits{PoolMin: 2}, poolMin: 2, poolMax: defaultPool},
		{name: "both set", limits: Limits{PoolMin: 2, PoolMax: 6}, poolMin: 2, poolMax: 6},
		{name: "a negative PoolMax", limits: Limits{PoolMin: 8, PoolMax: -1}, poolMin: 8, poolMax: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			limits := tc.limits.withDefaults()

			assert.Equal(t, []int{tc.poolMin, tc.poolMax}, []int{limits.PoolMin, limits.PoolMax})
			assert.Equal(t, defaultConcurrency, limits.Concurrency)
			assert.Equal(t, defaultCallTimeout, limits.CallTimeout)
			assert.Equal(t, defaultAttempts, limits.Attempts)
			assert.Equal(t, defaultRetryBackoff, limits.RetryBackoff)
			assert.Equal(t, 1, limits.ConcurrentCalls)
		})
	}
}
