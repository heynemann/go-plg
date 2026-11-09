package plg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/heynemann/go-plg/internal/redact"
	"github.com/heynemann/go-plg/internal/wire"
)

// Bounds of secrets.
const (
	// MaxSecrets is how many secrets a plugin may declare.
	MaxSecrets = 64
	// MaxSecretName is the longest secret name, in bytes.
	MaxSecretName = 64
	// MaxSecretSize is the largest secret value a plugin may read, in bytes: a larger one the resolver returns
	// fails the read.
	MaxSecretSize = 64 << 10
)

// secretTTL is how long a resolved value serves a plugin version's reads before the next read resolves it again:
// short, so that a rotated secret reaches the plugin without a reload.
const secretTTL = 30 * time.Second

// secretsMethod names the WithSecrets function in the PanicError reporting its panic.
const secretsMethod = "WithSecrets"

// WithSecrets makes resolve the source of the secrets the Register's plugins read: credentials for the
// third-party APIs they call, which then are not written into their code. A plugin may read only the secrets its
// plugin.yaml declares ([Descriptor.Secrets]):
//
//	plugin.Secret(ctx, "stripe_key") // Go: (string, error); plugin is github.com/heynemann/go-plg/plugin
//	secrets.get("stripe_key")        -- Lua: the value, or raises an error
//	secrets.get("stripe_key")        // TypeScript: the value, or throws an Error
//
// plg calls resolve on the plugin's read, with the context of the plugin call (or of its Create, or its
// validation: [IsValidation]) and the plugin version info describes. A value then serves the version's reads for
// 30 seconds, after which the next read resolves it again, so a rotated secret reaches the plugins without a
// reload; versions never share values. A read fails, and the plugin gets an error (Go), a Lua error or a thrown
// Error, for a name its plugin.yaml does not declare (resolve is not called), without WithSecrets, and when
// resolve fails, panics or returns a value over [MaxSecretSize] bytes: the plugin then learns that the value could
// not be resolved, not why (resolve's error may describe the service's secret store), and the service's log has
// why, with the plugin's key, version, owner and the secret's name. A panic in resolve is recovered and reported
// as a [PanicError] in [PhaseCallback].
//
// plg scrubs every value resolved for a plugin version (its newest 8 per secret, so values a rotation replaced
// too) from what it reports of the plugin: its output, the errors it returns, its panic values and what it wrote
// to stderr before failing, its Validate issues and the log records of its HTTP requests (URLs and errors). Each
// value becomes "[secret <name>]". Scrubbing guards against accidents, such as a token printed while debugging:
// it matches the exact value, so a plugin transforming a secret (encoding it, slicing it) is not caught. What the
// plugin returns from a successful call is the service's and is never scrubbed.
//
// resolve may be called concurrently, and several times for one value when reads race once it expired. A nil
// resolve or the option given twice fails Register with [ErrInvalidOption].
func WithSecrets(resolve func(ctx context.Context, info PluginInfo, name string) (string, error)) RegisterOption {
	return func(c *registerConfig) {
		c.secrets = append(c.secrets, resolve)
	}
}

// secretResolver is a WithSecrets function.
type secretResolver = func(ctx context.Context, info PluginInfo, name string) (string, error)

// settleSecrets checks the WithSecrets options and sets the Register's resolver and cache duration.
func (c *registerConfig) settleSecrets() error {
	switch {
	case len(c.secrets) > 1:
		return fmt.Errorf("%w: WithSecrets given %d times; a Register has one secret resolver", ErrInvalidOption,
			len(c.secrets))
	case len(c.secrets) == 1 && c.secrets[0] == nil:
		return fmt.Errorf("%w: WithSecrets(nil)", ErrInvalidOption)
	case len(c.secrets) == 1:
		c.resolve = c.secrets[0]
	default: // no resolver: every read of a declared secret fails
	}

	c.secretTTL = secretTTL

	return nil
}

var (
	errSecretName = fmt.Errorf(
		"a secret name is 1 to %d letters, digits, '_', '-' or '.', starting with a letter",
		MaxSecretName,
	)
	errSecretTwice = errors.New("a secret is declared twice")
	errSecretCount = fmt.Errorf("at most %d secrets", MaxSecrets)
)

// checkSecretNames checks the secrets a plugin declares.
func checkSecretNames(names []string) error {
	if len(names) > MaxSecrets {
		return fmt.Errorf("secrets: %w, not %d", errSecretCount, len(names))
	}

	for i, name := range names {
		if !validSecretName(name) {
			return fmt.Errorf("secrets: %w, not %q", errSecretName, name)
		}

		if slices.Contains(names[:i], name) {
			return fmt.Errorf("secrets: %w: %q", errSecretTwice, name)
		}
	}

	return nil
}

// validSecretName reports whether name is a valid secret name.
func validSecretName(name string) bool {
	if name == "" || len(name) > MaxSecretName || !isLetter(name[0]) {
		return false
	}

	for i := range len(name) {
		if !isSecretNameByte(name[i]) {
			return false
		}
	}

	return true
}

// isSecretNameByte reports whether c may appear in a secret name.
func isSecretNameByte(c byte) bool {
	return isLetter(c) || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// pluginSecrets serves the secret reads of one plugin version's instances (its pool's and its validation's): it
// refuses undeclared names, caches the values the Register's resolver returns and records them in the version's
// redactor. It is safe for concurrent use.
type pluginSecrets struct {
	resolve secretResolver   // nil: the Register has no WithSecrets
	redact  *redact.Redactor // the version's resolved values; nil when it declares none
	panics  *panicReporter
	logger  *slog.Logger // the version's guarded logger
	id      pluginID
	slots   map[string]*atomic.Pointer[resolvedSecret] // by declared name; immutable map
	names   []string                                   // declared, in plugin.yaml's order
	info    PluginInfo
	ttl     time.Duration // how long a value serves reads; 0 resolves every read
}

// resolvedSecret is a value and when it stops serving reads.
type resolvedSecret struct {
	expires time.Time
	value   string
}

// newPluginSecrets returns the secrets of the plugin version id, which info describes and which declares names.
func newPluginSecrets(cfg *registerConfig, info *PluginInfo, names []string, panics *panicReporter,
	logger *slog.Logger, id pluginID,
) *pluginSecrets {
	secrets := &pluginSecrets{
		resolve: cfg.resolve, panics: panics, logger: logger, id: id, names: names, info: *info,
		ttl: cfg.secretTTL, slots: make(map[string]*atomic.Pointer[resolvedSecret], len(names)),
	}

	for _, name := range names {
		secrets.slots[name] = new(atomic.Pointer[resolvedSecret])
	}

	if len(names) > 0 {
		secrets.redact = &redact.Redactor{}
	}

	return secrets
}

// serve is the version's [engine.SecretFunc]: it appends the status-prefixed answer to a read of the secret name.
func (s *pluginSecrets) serve(ctx context.Context, dst, name []byte) []byte {
	value, err := s.get(ctx, name)
	if err != nil {
		return wire.AppendMessage(append(dst, byte(wire.StatusError)), err.Error())
	}

	return wire.AppendMessage(append(dst, byte(wire.StatusOK)), value)
}

// redactor returns the version's redactor: nil, which scrubs nothing, when it declares no secrets or s is nil (a
// launcher built without secrets).
func (s *pluginSecrets) redactor() *redact.Redactor {
	if s == nil {
		return nil
	}

	return s.redact
}

// get returns the value of the secret name: the cached one while it serves reads, else the resolver's.
func (s *pluginSecrets) get(ctx context.Context, name []byte) (string, error) {
	slot, declared := s.slots[string(name)]
	if !declared {
		return "", s.undeclared(name)
	}

	if cached := slot.Load(); cached != nil && time.Now().Before(cached.expires) {
		return cached.value, nil
	}

	key := string(name)

	if s.resolve == nil {
		return "", fmt.Errorf("plg: secret %q %w", key, errNoResolver)
	}

	value, err := s.call(ctx, key)
	if err == nil && len(value) > MaxSecretSize {
		err = fmt.Errorf("%w: %d bytes", errSecretTooLarge, len(value))
	}

	if err != nil { // the logger is the version's, which logs its owner
		s.logger.LogAttrs(ctx, slog.LevelWarn, "plg: resolving a secret failed", slog.String("key", s.info.Key),
			slog.String("version", s.info.Version), slog.String("secret", key),
			slog.String("error", s.redact.String(err.Error())))

		return "", fmt.Errorf("plg: secret %q %w", key, errUnresolved)
	}

	s.redact.Add(key, value)

	if s.ttl > 0 {
		slot.Store(&resolvedSecret{value: value, expires: time.Now().Add(s.ttl)})
	}

	return value, nil
}

// Why a read fails, as the plugin is told.
var (
	errNoResolver = errors.New("cannot be resolved: the service registered no secret resolver (plg.WithSecrets)")
	errUnresolved = errors.New("could not be resolved (the service's log says why)")
	errUndeclared = errors.New("not declared in the plugin's plugin.yaml")
)

// errSecretTooLarge reports a value over MaxSecretSize.
var errSecretTooLarge = fmt.Errorf("the resolver returned a value over the %d-byte limit", MaxSecretSize)

// errSecretPanic reports a resolver that panicked.
var errSecretPanic = errors.New("the resolver panicked")

// call calls the resolver for the secret name, recovering and reporting its panic.
func (s *pluginSecrets) call(ctx context.Context, name string) (string, error) {
	var (
		value string
		err   error
	)

	func() {
		defer func() {
			if v := recover(); v != nil { // reported here, so the stack is the panicking one
				failure := s.id.panicked(PhaseCallback)
				failure.Method = secretsMethod
				s.panics.recovered(ctx, failure, v)

				value, err = "", errSecretPanic
			}
		}()

		value, err = s.resolve(ctx, s.info, name)
	}()

	return value, err
}

// undeclared returns the error refusing a read of the secret name, which the plugin does not declare. A name
// over [MaxSecretName] bytes is not quoted, so that a plugin cannot make the host format a huge message.
func (s *pluginSecrets) undeclared(name []byte) error {
	declared := "it declares none"
	if len(s.names) > 0 {
		declared = "it declares " + strings.Join(s.names, ", ")
	}

	if len(name) > MaxSecretName {
		return fmt.Errorf("plg: a secret name of %d bytes is %w (%s)", len(name), errUndeclared, declared)
	}

	return fmt.Errorf("plg: secret %q is %w (%s)", name, errUndeclared, declared)
}
