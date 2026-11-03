package badcreate

import "github.com/heynemann/go-plg/internal/testcontract"

// Create has the wrong signature: it takes an argument and returns no error.
func Create(name string) testcontract.Fooer { return nil }
