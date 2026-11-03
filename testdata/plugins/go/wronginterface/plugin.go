package wronginterface

import "context"

type half struct{}

func (half) Foo(context.Context, string) (string, error) { return "", nil }

// Create returns a type that implements only part of testcontract.Fooer.
func Create() (half, error) { return half{}, nil }
