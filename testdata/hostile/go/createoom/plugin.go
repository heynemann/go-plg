// Package createoom is a hostile-corpus Go plugin whose Create allocates until the instance runs out of
// memory.
package createoom

import "github.com/heynemann/go-plg/internal/testcontract"

// Create never returns: the guest traps when its memory limit is reached.
func Create() (testcontract.Fooer, error) {
	var hoard [][]byte

	for {
		hoard = append(hoard, make([]byte, 1<<20))
	}
}
