package createfails

import (
	"errors"

	"github.com/heynemann/go-plg/internal/testcontract"
)

// Create always fails.
func Create() (testcontract.Fooer, error) { return nil, errors.New("database unreachable") }
