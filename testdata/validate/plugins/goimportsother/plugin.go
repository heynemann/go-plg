// Package goimportsother imports a package beyond the contract and the standard library.
package goimportsother

import (
	"example.com/other"
	"example.com/rewards/cohort"
)

// Create returns what the other package makes.
func Create() (cohort.Provider, error) {
	return other.Provider(), nil
}
