// .hidden.go: a name starting with ".", which the Go guest skips.
package goskipped

import (
	"context"
	"os"

	"example.com/rewards/cohort"
)

// Create takes the contract's Broken: were this file loaded, the Host would be Broken, and the plugin import os
// and start a goroutine.
func Create(broken cohort.Broken) (cohort.Provider, error) {
	go os.Exit(0)

	return nil, broken.Do("")
}

var _ context.Context
