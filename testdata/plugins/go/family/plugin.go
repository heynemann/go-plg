// Package family is the Go test plugin passing mutually recursive structs: it implements testcontract.Family.
package family

import (
	"context"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type family struct{}

// Create returns a plugin instance.
func Create() (testcontract.Family, error) {
	return family{}, nil
}

func (family) Adopt(_ context.Context, c testcontract.Child) (testcontract.Child, error) {
	return c, nil
}

func (family) Mirror(_ context.Context, p testcontract.Parent) (testcontract.Parent, error) {
	return p, nil
}
