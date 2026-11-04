// Package hosted is the Go test plugin that calls back into its host: it implements testcontract.Hosted.
package hosted

import (
	"context"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type hosted struct {
	host   testcontract.Host
	origin string
}

// Create keeps the host and calls it once, so host calls work while Create runs.
func Create(host testcontract.Host) (testcontract.Hosted, error) {
	origin, err := host.Value(context.Background())
	if err != nil {
		return nil, err
	}

	return &hosted{host: host, origin: origin}, nil
}

func (h *hosted) Describe(ctx context.Context, id string) (testcontract.User, error) {
	user, err := h.host.Lookup(ctx, id)
	if err != nil {
		return testcontract.User{}, err
	}

	user.Name = "described " + user.Name

	return user, nil
}

func (h *hosted) Relay(ctx context.Context, msg string) (string, error) {
	err := h.host.Fail(ctx, msg)
	if err != nil {
		return err.Error(), nil
	}

	return "", nil
}

func (h *hosted) Value(ctx context.Context) (string, error) {
	return h.host.Value(ctx)
}

func (h *hosted) Origin(context.Context) (string, error) {
	return h.origin, nil
}

func (h *hosted) Total(ctx context.Context, base int, more ...int) (int, error) {
	return h.host.Sum(ctx, base, more...)
}

func (h *hosted) Keys(ctx context.Context, keys map[string]struct{}) (map[string]struct{}, error) {
	return h.host.Keys(ctx, keys)
}
