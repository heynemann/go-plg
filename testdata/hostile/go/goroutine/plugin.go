// Package goroutine is a hostile-corpus Go plugin starting goroutines, which Register must reject before
// loading it: a go statement here and one in worker.go.
package goroutine

import (
	"context"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type plugin struct {
	testcontract.Fooer
}

// Create starts a background goroutine.
func Create() (testcontract.Fooer, error) {
	done := make(chan struct{})
	go func() { close(done) }()

	return &plugin{}, nil
}

func (*plugin) Foo(ctx context.Context, name string) (string, error) {
	work(ctx)

	return name, nil
}
