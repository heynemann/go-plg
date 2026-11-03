package nilcreate

import "github.com/heynemann/go-plg/internal/testcontract"

// Create returns no instance and no error.
func Create() (testcontract.Fooer, error) { return nil, nil }
