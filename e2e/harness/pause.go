package harness

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"example.com/rewards/cohort"
	"github.com/stretchr/testify/require"
)

// Paused is a user directory (the platform's Config.Users) whose first lookup of one user waits until Release: a
// campaign processing its members one at a time (Limits.Concurrency 1) stops at that member, with every member
// before it decided and none after it, for as long as the test needs to change something; with more members at
// once, the others go on, but the campaign cannot end. Safe for concurrent use.
type Paused struct {
	cohort.Users

	reached chan struct{}
	release chan struct{}
	user    string
	paused  atomic.Bool
}

var _ cohort.Users = (*Paused)(nil)

// pauseTimeout bounds Await: a campaign started reaches its stop within seconds, so a minute means it never will.
const pauseTimeout = time.Minute

// Pause returns users pausing at the first lookup of user.
func Pause(users cohort.Users, user string) *Paused {
	return &Paused{Users: users, user: user, reached: make(chan struct{}), release: make(chan struct{})}
}

// Reached is closed once the lookup of the user has started waiting.
func (p *Paused) Reached() <-chan struct{} {
	return p.reached
}

// Await waits until the lookup of the user has started waiting; tb fails when it does not within a minute.
func (p *Paused) Await(tb testing.TB) {
	tb.Helper()

	timeout := time.NewTimer(pauseTimeout)
	defer timeout.Stop()

	select {
	case <-p.reached:
	case <-timeout.C:
		require.FailNow(tb, "no campaign reached the stop at "+p.user+" within "+pauseTimeout.String())
	case <-tb.Context().Done():
		require.FailNow(tb, "the test ended waiting for the stop at "+p.user)
	}
}

// Release lets the waiting lookup, and every later one, go on.
func (p *Paused) Release() {
	close(p.release)
}

// Lookup returns the user of the ID, after Release for the first lookup of the paused user; ctx's end stops the
// wait.
func (p *Paused) Lookup(ctx context.Context, id string) (cohort.User, error) {
	if id == p.user && p.paused.CompareAndSwap(false, true) {
		close(p.reached)

		select {
		case <-p.release:
		case <-ctx.Done():
			return cohort.User{}, fmt.Errorf("paused at %s: %w", id, ctx.Err())
		}
	}

	return p.Users.Lookup(ctx, id) //nolint:wrapcheck // the directory's own
}
