package plugin

import (
	"context"
	"errors"

	"github.com/heynemann/go-plg/internal/secretabi"
)

// ErrNoGuest is what [Secret] fails with outside a plg guest, such as in a plugin's own go test.
var ErrNoGuest = errors.New("plugin: secrets are read only inside plg")

// Secret returns the value of the secret name, which the plugin's plugin.yaml must declare (secrets: [name]); the
// service resolves it (plg.WithSecrets). It fails, with the host's reason, for a name plugin.yaml does not
// declare, when the service has no resolver for it or when resolving it failed; and with [ErrNoGuest] outside plg.
//
//	key, err := plugin.Secret(ctx, "stripe_key")
//
// A value read once serves the plugin version's reads for a short while, so read it when needed rather than
// keeping it: a rotated secret then reaches the plugin without a reload. plg scrubs the values it resolved from
// the plugin's output, errors and panics. The read is bounded by the plugin call (or Create) it is made in; ctx
// is not consulted.
func Secret(_ context.Context, name string) (string, error) {
	read := secretabi.Read
	if read == nil {
		return "", ErrNoGuest
	}

	return read(name)
}
