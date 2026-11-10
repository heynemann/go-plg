// Package platform is a rewards platform built on plg: growth teams set up campaigns that reward the members of a
// cohort who meet eligibility conditions, and cohort providers, eligibility conditions and reward deliverers are
// plugins written by many teams, in Go, Lua and TypeScript, against the contracts of example.com/rewards. The
// platform serves them from storages it polls ([plg.WithPollInterval]), so a team's newly published plugin goes
// live without a restart, and runs campaigns ([Platform.Run]) with them: one trace per campaign, metrics and
// lifecycle events attributed to each plugin's owning team, idempotent delivery with retries.
//
// It is the end-to-end tests' subject (e2e/, `make complex-e2e`), and a real Go package a reader can study as
// the way to build such a platform on plg.
package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"time"

	"example.com/rewards/cohort"
	"example.com/rewards/delivery"
	"example.com/rewards/eligibility"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/heynemann/go-plg"
	_ "github.com/heynemann/go-plg/guest/golang"     // runs Go plugins
	_ "github.com/heynemann/go-plg/guest/lua"        // runs Lua plugins
	_ "github.com/heynemann/go-plg/guest/typescript" // runs TypeScript plugins
)

// The names of the platform's registrations, one per contract. A plugin's key is "<registration>/<its key in the
// storage>" (the directory or artifact name: "conditions/country"), the key its metrics, spans, events, status
// and the failures of a [Result] carry.
const (
	Cohorts    = "cohorts"
	Conditions = "conditions"
	Deliveries = "deliveries"
)

// WalletURLSecret is the secret through which deliverers read the wallet API's URL ([Config].WalletURL): plugins
// have no other per-environment configuration, so the address is a secret like the token, resolved by the
// platform for any plugin whose plugin.yaml declares it.
const WalletURLSecret = "wallet_api_url" //nolint:gosec // G101: a secret's name, not its value

// Defaults of [Limits].
const (
	defaultConcurrency  = 16
	defaultCallTimeout  = 5 * time.Second
	defaultAttempts     = 3
	defaultRetryBackoff = 10 * time.Millisecond
	defaultPool         = 4
)

// Storages are where each registration's plugins are published: the platform registers each contract with its
// own storages, so a storage holds only plugins of that contract (any other one would be listed and skipped).
type Storages struct {
	Cohorts    []plg.Storage
	Conditions []plg.Storage
	Deliveries []plg.Storage
}

// Limits size the platform to its load and bound what plugins may do; zero fields take the defaults.
type Limits struct {
	// Eligibility is what conditions may require: the input of their Validate, before each version goes live.
	Eligibility eligibility.Limits
	// Concurrency is how many members a campaign processes at once (default 16).
	Concurrency int
	// CallTimeout bounds each plugin call (default 5s).
	CallTimeout time.Duration
	// Attempts is how many times the platform tries a call it retries before giving up (default 3): a reward's
	// delivery, after which its member counts as failed, and a page of the cohort, after which the campaign
	// fails. Attempts: 1 turns off both retries.
	Attempts int
	// RetryBackoff is the wait before the second attempt, doubled before each later one (default 10ms).
	RetryBackoff time.Duration
	// PoolMin and PoolMax bound each plugin's instances (plg.WithPool; default 4 and 4: every instance started
	// before the platform serves, as calls with deadlines need).
	PoolMin, PoolMax int
	// ConcurrentCalls is how many calls a Lua or TypeScript instance serves at once while they wait for HTTP
	// responses (plg.WithConcurrentCalls; default 1).
	ConcurrentCalls int
	// MemoryLimit is each instance's memory (plg.WithMemoryLimit; 0: plg's default per language).
	MemoryLimit uint64
	// MemoryBudget caps every instance's memory together (plg.WithMemoryBudget; 0: no cap).
	MemoryBudget uint64
}

// Config is what the platform runs with.
type Config struct {
	// Storages are the registrations' storages.
	Storages Storages
	// Users is the platform's user directory: the cohort providers' Host, and where the platform looks up each
	// member it asks conditions about.
	Users cohort.Users
	// TracerProvider and MeterProvider receive the platform's and plg's spans and metrics (nil: the global ones).
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	// Secrets resolves the secrets plugins read (plg.WithSecrets), but WalletURLSecret, which is WalletURL; nil
	// fails every read of another secret.
	Secrets func(ctx context.Context, info plg.PluginInfo, name string) (string, error)
	// Logger receives plg's log (nil: discarded).
	Logger *slog.Logger
	// OnEvent receives plg's lifecycle events (plg.WithOnEvent; nil: none).
	OnEvent func(ctx context.Context, event plg.Event)
	// WalletURL is the wallet API's base URL, which deliverers read as the secret WalletURLSecret; its host is
	// added to HTTP's allowed hosts.
	WalletURL string
	// HTTP is the plugins' HTTP policy (plg.WithHTTP): the vendor APIs they may call. Without allowed hosts and
	// a WalletURL, plugins make no requests.
	HTTP plg.HTTPPolicy
	// Limits size the platform.
	Limits Limits
	// PollInterval is how often each registration lists its storages for new, updated and removed plugins (0:
	// never; Reload then does it).
	PollInterval time.Duration
}

// Platform runs campaigns with the plugins its storages hold. It is safe for concurrent use: campaigns may run at
// once, while the storages are polled.
type Platform struct {
	container *plg.Container
	users     cohort.Users
	tracer    trace.Tracer
	limits    Limits
}

// ErrConfig reports a Config New cannot run with.
var ErrConfig = errors.New("platform: invalid config")

// errNoSecret is a secret read the platform has no resolver for.
var errNoSecret = errors.New("no secret resolver")

// New registers the platform's three contracts with their storages and returns the platform serving them. Every
// plugin loads, validates and starts its instances before New returns; one that cannot fails New (plg.Register
// is all or nothing), unlike on a later poll, which keeps the last good version serving. ctx bounds the
// registrations and, when one fails, closing what they started.
func New(ctx context.Context, cfg *Config) (*Platform, error) {
	if cfg.Users == nil {
		return nil, fmt.Errorf("%w: no Users directory", ErrConfig)
	}

	limits := cfg.Limits.withDefaults()

	containerOptions := []plg.Option{
		plg.WithTracerProvider(cfg.TracerProvider), plg.WithMeterProvider(cfg.MeterProvider),
	}
	if cfg.Logger != nil {
		containerOptions = append(containerOptions, plg.WithLogger(cfg.Logger))
	}

	if cfg.OnEvent != nil {
		containerOptions = append(containerOptions, plg.WithOnEvent(cfg.OnEvent))
	}

	if limits.MemoryBudget > 0 {
		containerOptions = append(containerOptions, plg.WithMemoryBudget(limits.MemoryBudget))
	}

	shared, err := sharedOptions(cfg, &limits)
	if err != nil {
		return nil, err
	}

	//nolint:contextcheck // plg.New takes no context: it only builds the container (and its metric instruments)
	container := plg.New(containerOptions...)

	err = register(ctx, container, cfg, shared)
	if err != nil {
		return nil, errors.Join(err, container.Close(ctx))
	}

	provider := cfg.TracerProvider
	if provider == nil {
		provider = otel.GetTracerProvider()
	}

	return &Platform{
		container: container,
		users:     cfg.Users,
		tracer:    provider.Tracer("github.com/heynemann/go-plg/e2e/platform"),
		limits:    limits,
	}, nil
}

// register registers the three contracts in container, each with its storages and the options every one shares.
func register(ctx context.Context, container *plg.Container, cfg *Config, shared []plg.RegisterOption) error {
	err := plg.Register[cohort.Provider](ctx, container, Cohorts,
		options(shared, cfg.Storages.Cohorts, plg.WithHost[cohort.Users](cfg.Users))...)
	if err != nil {
		return fmt.Errorf("registering the cohort providers: %w", err)
	}

	eligibilityLimits := cfg.Limits.Eligibility
	validation := plg.WithValidation(func(context.Context, plg.PluginInfo) (eligibility.Limits, error) {
		return eligibilityLimits, nil
	})

	err = plg.Register[eligibility.Condition](ctx, container, Conditions,
		options(shared, cfg.Storages.Conditions, validation)...)
	if err != nil {
		return fmt.Errorf("registering the eligibility conditions: %w", err)
	}

	err = plg.Register[delivery.Deliverer](ctx, container, Deliveries, options(shared, cfg.Storages.Deliveries)...)
	if err != nil {
		return fmt.Errorf("registering the deliverers: %w", err)
	}

	return nil
}

// options returns a registration's options: shared, one plg.WithStorage per storage, then extra.
func options(shared []plg.RegisterOption, list []plg.Storage, extra ...plg.RegisterOption) []plg.RegisterOption {
	all := slices.Clone(shared)
	for _, storage := range list {
		all = append(all, plg.WithStorage(storage))
	}

	return append(all, extra...)
}

// sharedOptions returns the Register options of every registration: owners required, pools, polling, secrets
// and HTTP.
func sharedOptions(cfg *Config, limits *Limits) ([]plg.RegisterOption, error) {
	options := []plg.RegisterOption{
		plg.WithRequiredOwnership(),
		plg.WithPool(limits.PoolMin, limits.PoolMax),
		plg.WithConcurrentCalls(limits.ConcurrentCalls),
		plg.WithSecrets(secrets(cfg.WalletURL, cfg.Secrets)),
	}

	if cfg.PollInterval > 0 {
		options = append(options, plg.WithPollInterval(cfg.PollInterval))
	}

	if limits.MemoryLimit > 0 {
		options = append(options, plg.WithMemoryLimit(limits.MemoryLimit))
	}

	policy := cfg.HTTP
	policy.AllowedHosts = slices.Clone(policy.AllowedHosts)

	if cfg.WalletURL != "" {
		wallet, err := url.Parse(cfg.WalletURL)
		if err != nil || wallet.Host == "" {
			return nil, fmt.Errorf("%w: WalletURL %q is not an absolute URL", ErrConfig, cfg.WalletURL)
		}

		policy.AllowedHosts = append(policy.AllowedHosts, wallet.Host)
	}

	if len(policy.AllowedHosts) > 0 {
		options = append(options, plg.WithHTTP(policy))
	}

	return options, nil
}

// secrets returns the platform's secret resolver: walletURL for WalletURLSecret, resolve for the others.
func secrets(
	walletURL string, resolve func(context.Context, plg.PluginInfo, string) (string, error),
) func(context.Context, plg.PluginInfo, string) (string, error) {
	return func(ctx context.Context, info plg.PluginInfo, name string) (string, error) {
		switch {
		case name == WalletURLSecret && walletURL != "":
			return walletURL, nil
		case resolve == nil:
			return "", errNoSecret
		default:
			return resolve(ctx, info, name)
		}
	}
}

// withDefaults returns l with its zero fields set to their defaults.
func (l *Limits) withDefaults() Limits {
	limits := *l
	limits.Concurrency = positiveOr(limits.Concurrency, defaultConcurrency)
	limits.CallTimeout = time.Duration(positiveOr(int(limits.CallTimeout), int(defaultCallTimeout)))
	limits.Attempts = positiveOr(limits.Attempts, defaultAttempts)
	limits.RetryBackoff = time.Duration(positiveOr(int(limits.RetryBackoff), int(defaultRetryBackoff)))
	limits.PoolMin = positiveOr(limits.PoolMin, defaultPool)
	limits.PoolMax = positiveOr(limits.PoolMax, max(limits.PoolMin, defaultPool))
	limits.ConcurrentCalls = positiveOr(limits.ConcurrentCalls, 1)

	return limits
}

// positiveOr returns value when it is positive, else fallback.
func positiveOr(value, fallback int) int {
	if value <= 0 {
		return fallback
	}

	return value
}

// Container is the plg container serving the platform's plugins, for what plg offers beyond the platform's own
// API: plg.Info, plg.Plugins, plg.Events, plg.Validate, plg.Check.
func (p *Platform) Container() *plg.Container {
	return p.container
}

// AdminHandler is plg's admin handler of the platform's plugins (plg.AdminHandler), which `plg status` and `plg
// events` read. Mount it behind the platform's own authentication.
func (p *Platform) AdminHandler() http.Handler {
	return plg.AdminHandler(p.container)
}

// Reload lists every registration's storages now, as a poll does (plg's Container.Reload).
func (p *Platform) Reload(ctx context.Context) error {
	return p.container.Reload(ctx) //nolint:wrapcheck // plg's errors name the registration and the plugin
}

// Close stops serving: calls in flight finish (ended with plg.ErrClosed when ctx ends first), then every instance
// closes.
func (p *Platform) Close(ctx context.Context) error {
	return p.container.Close(ctx) //nolint:wrapcheck // plg's error
}
