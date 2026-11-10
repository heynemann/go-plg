//go:build e2e

// Package e2e_test runs the rewards platform end to end (`make complex-e2e`): the teams' repositories through their
// CI, their artifacts published to MinIO, the platform serving them in every guest, and campaigns checked against
// an oracle.
package e2e_test

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
