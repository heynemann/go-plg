package multifile

import (
	"context"

	"github.com/heynemann/go-plg/internal/testcontract"
)

func (m *multi) Foo(_ context.Context, name string) (string, error) {
	return testcontract.Greeting(name) + " (" + m.label + ")", nil
}
