// Package sleepycreate is a hostile-corpus Go plugin whose Create sleeps for 20 s when its Host's Value is
// "sleep", so a test chooses which of its instances sleeps while starting. Relay("panic") panics, discarding the
// instance; its other methods answer without calling the Host.
package sleepycreate

import (
	"context"
	"time"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type sleepy struct{}

// Create sleeps far past every deadline of the tests when the Host says so.
func Create(host testcontract.Host) (testcontract.Hosted, error) {
	v, err := host.Value(context.Background())
	if err != nil {
		return nil, err
	}

	if v == "sleep" {
		time.Sleep(20 * time.Second)
	}

	return sleepy{}, nil
}

func (sleepy) Describe(_ context.Context, id string) (testcontract.User, error) {
	return testcontract.User{ID: id}, nil
}

func (sleepy) Relay(_ context.Context, msg string) (string, error) {
	if msg == "panic" {
		panic("sleepycreate panicked on purpose")
	}

	return msg, nil
}

func (sleepy) Value(context.Context) (string, error) {
	return "", nil
}

func (sleepy) Origin(context.Context) (string, error) {
	return "", nil
}

func (sleepy) Total(_ context.Context, base int, more ...int) (int, error) {
	for _, n := range more {
		base += n
	}

	return base, nil
}
