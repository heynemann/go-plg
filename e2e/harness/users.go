package harness

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"

	"example.com/rewards/cohort"
)

// The users' tiers.
const (
	Gold   = "gold"
	Silver = "silver"
	Bronze = "bronze"
)

// Countries are the countries of the directory's users: those a rig's platform serves.
func Countries() []string {
	return []string{"BR", "PT", "US", "DE", "MX"}
}

// How the directory's attributes are drawn: from a generator of a fixed seed, spend from [0, maxSpendCents),
// orders from [0, maxOrders), and one user in churnOneIn in the vendor's churn-risk segment.
const (
	seed1, seed2  = 2026, 11
	maxSpendCents = 30000
	maxOrders     = 8
	churnOneIn    = 5
)

// User is a user of a test's directory: what the platform's directory knows (cohort.User), and what the CRM
// vendor knows of them.
type User struct {
	cohort.User

	// ChurnRisk is whether the vendor's churn-risk segment holds the user.
	ChurnRisk bool
}

// Directory is the platform's user directory in a test: users u000001, u000002, … whose attributes a generator
// of a fixed seed draws, so every run has the same users. It implements cohort.Users, the cohort providers' Host,
// and is safe for concurrent use (it never changes).
type Directory struct {
	index map[string]int
	users []User // by ID
}

var _ cohort.Users = (*Directory)(nil)

// errLimit is a page size the directory does not serve.
var errLimit = errors.New("the directory serves pages of 1 to cohort.MaxPageSize users")

// NewDirectory returns a directory of n users.
func NewDirectory(n int) *Directory {
	//nolint:gosec // G404: a fixed seed, so that every run's directory is the same; nothing secret is drawn
	draw := rand.New(rand.NewPCG(seed1, seed2))
	dir := &Directory{users: make([]User, n), index: make(map[string]int, n)}
	countries, tiers := Countries(), []string{Gold, Silver, Bronze}

	for i := range n {
		id := fmt.Sprintf("u%06d", i+1)
		dir.users[i] = User{
			ID:         id,
			Country:    countries[draw.IntN(len(countries))],
			Tier:       tiers[draw.IntN(len(tiers))],
			SpendCents: draw.Int64N(maxSpendCents),
			Orders:     draw.IntN(maxOrders),
			ChurnRisk:  draw.IntN(churnOneIn) == 0,
		}
		dir.index[id] = i
	}

	return dir
}

// Users returns the directory's users, by ID.
func (d *Directory) Users() []User {
	return d.users
}

// Lookup returns the user of the ID; a User with an empty ID when there is none.
func (d *Directory) Lookup(_ context.Context, id string) (cohort.User, error) {
	i, ok := d.index[id]
	if !ok {
		return cohort.User{}, nil
	}

	return d.users[i].User, nil
}

// Page returns at most limit users whose IDs come after cursor, and the last one's ID as the next cursor unless
// the page reaches the end.
func (d *Directory) Page(_ context.Context, cursor string, limit int) (cohort.UserPage, error) {
	if limit < 1 || limit > cohort.MaxPageSize {
		return cohort.UserPage{}, fmt.Errorf("%w, not %d", errLimit, limit)
	}

	start := sort.Search(len(d.users), func(i int) bool { return d.users[i].ID > cursor })
	end := min(start+limit, len(d.users))
	page := cohort.UserPage{Users: make([]cohort.User, 0, end-start)}

	for _, user := range d.users[start:end] {
		page.Users = append(page.Users, user.User)
	}

	if end < len(d.users) {
		page.Next = d.users[end-1].ID
	}

	return page, nil
}
