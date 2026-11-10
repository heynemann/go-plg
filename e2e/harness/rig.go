package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"example.com/rewards/eligibility"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/e2e/platform"
	"github.com/heynemann/go-plg/examples/containertest"
)

// The bearer tokens of the fake APIs, which the Vault resolves for the plugins declaring them.
const (
	WalletToken = "wlt-9fQ2mZ7xK4" //nolint:gosec // G101: the fake wallet's made-up token
	VendorToken = "vnd-3Lp8wR1sT6" //nolint:gosec // G101: the fake vendor's made-up token
	// WatchlistToken is the fraud team's watchlist API's token, which its watchlist condition reads (and leaks).
	WatchlistToken = "wch-7Hd4Kx2Mq9" //nolint:gosec // G101: a made-up token
)

// DefaultUsers is how many users a rig's directory holds unless Options say otherwise.
const DefaultUsers = 1000

// CloseTimeout bounds a platform's Close, the calls in flight finishing and plg delivering its queued events: a
// hang detector, so that a test whose platform cannot close fails instead of waiting for the binary's timeout.
const CloseTimeout = time.Minute

// maxRigs is how many rigs a test binary runs at once. Each holds MinIO, its teams' CI and a platform whose pools
// of guest instances take hundreds of MB under the race detector: the 15 scenarios at once, as go test runs
// parallel tests on a 28-CPU machine, peaked at 14.9 GB. Parallel tests beyond it wait in NewRig for a slot.
const maxRigs = 4

// rigSlots holds a token per running rig, which its test's cleanup takes back last.
var rigSlots = make(chan struct{}, maxRigs) //nolint:gochecknoglobals // the test binary's, shared by its tests' rigs

// Vault resolves the plugins' secrets by their owning team: the value of "<team>/<secret name>", so a team reads
// only its own secrets whatever names another team's plugin.yaml declares. It is immutable, hence safe for
// concurrent use.
type Vault map[string]string

// errNoSecret is a secret the vault does not hold.
var errNoSecret = errors.New("the vault holds no such secret")

// Resolver returns the platform's secret resolver (Config.Secrets) reading v.
func (v Vault) Resolver() func(ctx context.Context, info plg.PluginInfo, name string) (string, error) {
	return func(_ context.Context, info plg.PluginInfo, name string) (string, error) {
		value, ok := v[info.Owner.Team.Name+"/"+name]
		if !ok {
			return "", fmt.Errorf("%w: %s of team %q", errNoSecret, name, info.Owner.Team.Name)
		}

		return value, nil
	}
}

// What a rig's platform lets conditions require (EligibilityLimits).
const (
	maxRequiredSpendCents = 20000
	maxRequiredOrders     = 5
)

// EligibilityLimits are what a rig's platform lets conditions require: every country of the directory, 200.00
// of yearly spend and 5 yearly orders, which every team's condition stays within.
func EligibilityLimits() eligibility.Limits {
	return eligibility.Limits{
		Countries: Countries(), MaxSpendCents: maxRequiredSpendCents, MaxOrders: maxRequiredOrders,
	}
}

// Options shape a rig; the zero value is the foundation's: 1,000 users, every team, the default limits, no
// polling.
type Options struct {
	// Configure, when set, has the last word on the platform's config before it starts.
	Configure func(cfg *platform.Config)
	// Teams are the teams whose CI runs and whose artifacts are published (default: every team).
	Teams []string
	// Limits are the platform's (Eligibility defaults to EligibilityLimits()).
	Limits platform.Limits
	// Users is how many users the directory holds (default DefaultUsers).
	Users int
	// PollInterval is how often the platform polls the bucket (0: never).
	PollInterval time.Duration
}

// Rig is a rewards platform in a test, on the real path: each team's repository, its CI (plg validate and plg
// pack, built from this checkout) and its artifacts published to MinIO, served by the platform from there with
// an OpenTelemetry SDK, a user directory, the fake wallet and vendor APIs, and a vault holding their tokens.
type Rig struct {
	Telemetry *Telemetry
	S3        *s3.Client
	Users     *Directory
	Wallet    *Wallet
	Vendor    *Vendor
	Platform  *platform.Platform
	// Teams are the teams' repositories, by name.
	Teams map[string]*Team
	// Vault is the platform's secret resolver.
	Vault Vault
	// Bucket is where the artifacts are published.
	Bucket string
	// Environ is the environment of the teams' commands (Environ's), to run their CI again after a change.
	Environ []string
	// Published are the platform keys of the plugins published.
	Published []string
}

// NewRig sets up a rig for tb, shaped by opts (nil: the defaults): MinIO, the teams' CI and their artifacts
// published, the fakes and the platform, every piece stopped when tb ends. It waits while maxRigs rigs run.
func NewRig(tb testing.TB, opts *Options) *Rig {
	tb.Helper()

	select {
	case rigSlots <- struct{}{}:
		tb.Cleanup(func() { <-rigSlots })
	case <-tb.Context().Done():
		require.FailNow(tb, "the test ended waiting for a rig")
	}

	options := withDefaults(opts)

	rig := &Rig{Users: NewDirectory(options.Users), Telemetry: NewTelemetry(tb), Teams: map[string]*Team{}}
	rig.Wallet = NewWallet(tb, WalletToken)
	rig.Vendor = NewVendor(tb, VendorToken, rig.Users)
	rig.Vault = Vault{
		CRM + "/vendor_api_url": rig.Vendor.URL(), CRM + "/vendor_api_token": VendorToken,
		Payments + "/wallet_api_token": WalletToken, Fraud + "/watchlist_token": WatchlistToken,
	}
	rig.S3 = containertest.StartMinIO(tb)
	rig.Bucket = containertest.NewBucket(tb, rig.S3)
	rig.Environ = Environ(tb)

	for _, name := range options.Teams {
		team := CopyTeam(tb, name)
		rig.Teams[name] = team
		rig.Published = append(rig.Published, Publish(tb, rig.S3, rig.Bucket, team.CI(tb, rig.Environ))...)
	}

	rig.Platform = rig.NewPlatform(tb, &options)

	return rig
}

// NewPlatform starts another platform on the rig's bucket and fakes, with its vault, directory and telemetry,
// shaped by opts (nil: the defaults): its Limits, PollInterval and Configure (which may replace the directory or
// the telemetry); its Teams and Users are the rig's and left out. The platform is closed when tb ends.
func (r *Rig) NewPlatform(tb testing.TB, opts *Options) *platform.Platform {
	tb.Helper()

	options := withDefaults(opts)
	cfg := platform.Config{
		Storages:       Storages(r.S3, r.Bucket),
		Users:          r.Users,
		TracerProvider: r.Telemetry.TracerProvider,
		MeterProvider:  r.Telemetry.MeterProvider,
		Secrets:        r.Vault.Resolver(),
		Logger:         slog.New(slog.NewTextHandler(tb.Output(), &slog.HandlerOptions{Level: slog.LevelWarn})),
		WalletURL:      r.Wallet.URL(),
		HTTP:           plg.HTTPPolicy{AllowedHosts: []string{r.Vendor.Host()}, BlockPrivateNetworks: new(false)},
		Limits:         options.Limits,
		PollInterval:   options.PollInterval,
	}

	if options.Configure != nil {
		options.Configure(&cfg)
	}

	started, err := platform.New(&cfg)
	require.NoError(tb, err)

	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(tb.Context()), CloseTimeout)
		defer cancel()

		require.NoError(tb, started.Close(ctx), "the platform closed within %v", CloseTimeout)
	})

	return started
}

// withDefaults returns opts (nil: none) with the defaults of what it leaves unset.
func withDefaults(opts *Options) Options {
	var options Options

	if opts != nil {
		options = *opts
	}

	if options.Users == 0 {
		options.Users = DefaultUsers
	}

	if options.Teams == nil {
		options.Teams = Teams()
	}

	if options.Limits.Eligibility.Countries == nil {
		options.Limits.Eligibility = EligibilityLimits()
	}

	return options
}
