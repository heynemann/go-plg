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

// Paused is a user directory (the platform's Config.Users) whose first lookup of one user (Pause), or first read
// of the page at one cursor (PausePage), waits until Release, for as long as the test needs to change something.
// Safe for concurrent use.
type Paused struct {
	cohort.Users

	reached chan struct{}
	release chan struct{}
	// at is the paused lookup's user, or the paused read's cursor when page is set.
	at     string
	page   bool
	paused atomic.Bool
}

var _ cohort.Users = (*Paused)(nil)

// pauseTimeout bounds Await: a campaign started reaches its stop within seconds, so a minute means it never will.
const pauseTimeout = time.Minute

// Pause returns users pausing at the first lookup of user: a campaign processing its members one at a time
// (Limits.Concurrency 1) stops at that member, with every member before it decided and none after it; with more
// members at once, the others go on, but the campaign cannot end.
func Pause(users cohort.Users, user string) *Paused {
	return &Paused{Users: users, at: user, reached: make(chan struct{}), release: make(chan struct{})}
}

// PausePage returns users pausing at the first read of the page at cursor: the cohort's List call reading it,
// through its Host, stays in flight in its plugin.
func PausePage(users cohort.Users, cursor string) *Paused {
	return &Paused{Users: users, at: cursor, page: true, reached: make(chan struct{}), release: make(chan struct{})}
}

// Reached is closed once the lookup or the read has started waiting.
func (p *Paused) Reached() <-chan struct{} {
	return p.reached
}

// Await waits until the lookup or the read has started waiting; tb fails when it does not within a minute.
func (p *Paused) Await(tb testing.TB) {
	tb.Helper()

	timeout := time.NewTimer(pauseTimeout)
	defer timeout.Stop()

	select {
	case <-p.reached:
	case <-timeout.C:
		require.FailNow(tb, "no campaign reached the stop at "+p.at+" within "+pauseTimeout.String())
	case <-tb.Context().Done():
		require.FailNow(tb, "the test ended waiting for the stop at "+p.at)
	}
}

// Release lets the waiting lookup or read, and every later one, go on.
func (p *Paused) Release() {
	close(p.release)
}

// Lookup returns the user of the ID, after Release for the first lookup of the paused user; ctx's end stops the
// wait.
func (p *Paused) Lookup(ctx context.Context, id string) (cohort.User, error) {
	if !p.page && id == p.at {
		err := p.wait(ctx)
		if err != nil {
			return cohort.User{}, err
		}
	}

	return p.Users.Lookup(ctx, id) //nolint:wrapcheck // the directory's own
}

// Page returns the page at cursor, after Release for the first read of the paused page; ctx's end stops the wait.
func (p *Paused) Page(ctx context.Context, cursor string, limit int) (cohort.UserPage, error) {
	if p.page && cursor == p.at {
		err := p.wait(ctx)
		if err != nil {
			return cohort.UserPage{}, err
		}
	}

	return p.Users.Page(ctx, cursor, limit) //nolint:wrapcheck // the directory's own
}

// wait waits, the first time only, for Release or ctx's end.
func (p *Paused) wait(ctx context.Context) error {
	if !p.paused.CompareAndSwap(false, true) {
		return nil
	}

	close(p.reached)

	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("paused at %s: %w", p.at, ctx.Err())
	}
}
