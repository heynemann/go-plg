// Package aliased is the Go test plugin whose signatures embed an alias in anonymous structs: it implements
// testcontract.Aliased.
package aliased

import (
	"context"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type aliased struct {
	host testcontract.AliasHost
}

// Create keeps the host.
func Create(host testcontract.AliasHost) (testcontract.Aliased, error) {
	return &aliased{host: host}, nil
}

func (a *aliased) Bump(_ context.Context, c struct {
	testcontract.Embedded

	By int
}) (struct {
	testcontract.Embedded

	By int
}, error) {
	c.N += c.By

	return c, nil
}

func (a *aliased) Double(ctx context.Context, c struct {
	testcontract.Embedded

	By int
}) (struct{ *testcontract.Embedded }, error) {
	return a.host.Double(ctx, c)
}
