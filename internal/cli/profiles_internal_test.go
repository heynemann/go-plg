package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAHangingTokenCommandTimesOut: a token command that never prints its token is killed at the timeout (the
// admin requests' own, in `plg status`) and fails saying so, a child holding its output open included.
func TestAHangingTokenCommandTimesOut(t *testing.T) {
	t.Parallel()

	prof := &profile{Name: "p", Token: &tokenSource{Command: []string{"sh", "-c", "sleep 60 & sleep 60"}}}

	token, err := prof.token(t.Context(), &Env{Dir: t.TempDir()}, 50*time.Millisecond)
	require.ErrorIs(t, err, errTokenCommand)
	assert.Empty(t, token)
	assert.EqualError(t, err,
		`profile p: token command failed: ["sh" "-c" "sleep 60 & sleep 60"] did not finish within 50ms`)
}
