// Package multifile is a Go plugin spread over several files of one package.
package multifile

import "github.com/heynemann/go-plg/internal/testcontract"

type multi struct {
	label string
}

// Create returns a fresh plugin instance; its label comes from another file of the package.
func Create() (testcontract.Fooer, error) {
	return &multi{label: defaultLabel()}, nil
}
