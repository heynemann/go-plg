package createpanics

import "github.com/heynemann/go-plg/internal/testcontract"

// Create panics: the guest recovers it and the instance is discarded.
func Create() (testcontract.Fooer, error) {
	panic("Create panicked on purpose")
}
