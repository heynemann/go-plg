package forbidden

import (
	"os"

	"github.com/heynemann/go-plg/internal/testcontract"
)

// Create would read the host environment if os were linked into the guest; it is not.
func Create() (testcontract.Fooer, error) {
	_ = os.Getenv("HOME")

	return nil, nil
}
