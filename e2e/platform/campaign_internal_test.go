package platform

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"example.com/rewards/cohort"
	"example.com/rewards/delivery"
	"example.com/rewards/eligibility"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/heynemann/go-plg"
)

// errBroken is what the tests' fakes fail with: a plugin's (or the directory's) own error.
var errBroken = errors.New("broken")

func TestRetryable(t *testing.T) {
	t.Parallel()

	live := t.Context()
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	for _, tc := range []struct {
		ctx       context.Context //nolint:containedctx // the campaign's, the case's input
		err       error
		name      string
		retryable bool
	}{
		{name: "the plugin's own error", ctx: live, err: errBroken, retryable: true},
		{name: "the attempt's timeout", ctx: live, err: fmt.Errorf("plg: %w", context.DeadlineExceeded), retryable: true},
		{name: "the plugin is gone", ctx: live, err: fmt.Errorf("plg: %w", plg.ErrNotFound)},
		{name: "the platform is closing", ctx: live, err: fmt.Errorf("plg: %w", plg.ErrClosed)},
		{name: "the values cannot cross", ctx: live, err: fmt.Errorf("plg: %w", plg.ErrConversion)},
		{name: "the campaign ended", ctx: ended, err: errBroken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.retryable, retryable(tc.ctx, tc.err))
		})
	}
}

func TestCheckPage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		seen    []string
		page    cohort.Page
		refused bool
	}{
		{name: "an empty first and last page", seen: []string{""}, page: cohort.Page{}},
		{name: "a full page", seen: []string{""}, page: cohort.Page{Users: users(cohort.MaxPageSize), Next: "u500"}},
		{
			name: "a page over the maximum", seen: []string{""},
			page: cohort.Page{Users: users(cohort.MaxPageSize + 1)}, refused: true,
		},
		{name: "the last page after another", seen: []string{"", "u1"}, page: cohort.Page{Users: users(1)}},
		{name: "a next cursor not seen yet", seen: []string{"", "u1"}, page: cohort.Page{Users: users(1), Next: "u2"}},
		{
			name: "a next cursor repeating the page's own", seen: []string{"", "u1"},
			page: cohort.Page{Users: users(1), Next: "u1"}, refused: true,
		},
		{
			name: "a next cursor repeating an earlier page's", seen: []string{"", "a", "b"},
			page: cohort.Page{Users: users(1), Next: "a"}, refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			seen := map[string]struct{}{}
			for _, cursor := range tc.seen {
				seen[cursor] = struct{}{}
			}

			err := checkPage(tc.page, seen)
			if tc.refused {
				require.ErrorIs(t, err, errPage)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// A cohort that cannot be listed fails its campaign and counts against the cohort once, its members listed
// before it processed: a page the platform refuses, at the first attempt, or a listing failing every attempt.
func TestRunCountsACohortsFailureOnce(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		pages map[string]cohort.Page
		want  Result
		lists int
	}{
		{
			name:  "a page over the maximum",
			pages: map[string]cohort.Page{"": {Users: users(cohort.MaxPageSize + 1)}},
			lists: 1,
			want:  Result{Failures: map[string]int{"cohorts/c": 1}},
		},
		{
			name: "a next cursor repeating the page's own",
			pages: map[string]cohort.Page{
				"": {Users: []string{"u1"}, Next: "u1"}, "u1": {Users: []string{"u2"}, Next: "u1"},
			},
			lists: 2,
			want:  Result{Failures: map[string]int{"cohorts/c": 1}, Members: 1, Eligible: 1, Delivered: 1},
		},
		{
			name: "next cursors cycling over two pages",
			pages: map[string]cohort.Page{
				"": {Users: []string{"u1"}, Next: "a"}, "a": {Users: []string{"u2"}, Next: "b"},
				"b": {Users: []string{"u1"}, Next: "a"},
			},
			lists: 3,
			want:  Result{Failures: map[string]int{"cohorts/c": 1}, Members: 2, Eligible: 2, Delivered: 2},
		},
		{
			name:  "a listing failing every attempt",
			pages: map[string]cohort.Page{"": {Users: []string{"u1"}, Next: "u1"}}, // none at u1
			lists: 1 + defaultAttempts,
			want:  Result{Failures: map[string]int{"cohorts/c": 1}, Members: 1, Eligible: 1, Delivered: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			provider := &pager{pages: tc.pages}
			result, err := testRun(provider, delivered).run(t.Context())

			require.ErrorIs(t, err, ErrCampaign)
			assert.Equal(t, tc.want, result)
			assert.Equal(t, tc.lists, provider.lists)
		})
	}
}

// A member's failure counts once, against what failed it: the directory, a condition, or the deliverer once its
// attempts ran out; a condition saying no is no failure.
func TestRunCountsAMembersFailureAgainstWhatFailedIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		deliverer delivery.Deliverer
		condition eligibility.Condition
		name      string
		member    string
		want      Result
	}{
		{
			name: "the directory fails", member: "broken", condition: accepting, deliverer: delivered,
			want: Result{Failures: map[string]int{Directory: 1}, Members: 1},
		},
		{
			name: "the directory has no such user", member: "ghost", condition: accepting, deliverer: delivered,
			want: Result{Failures: map[string]int{Directory: 1}, Members: 1},
		},
		{
			name: "a condition fails", member: "u1", condition: failing, deliverer: delivered,
			want: Result{Failures: map[string]int{"conditions/0": 1}, Members: 1},
		},
		{
			name: "a condition says no", member: "u1", condition: refusing, deliverer: delivered,
			want: Result{Failures: map[string]int{}, Members: 1},
		},
		{
			name: "the deliverer fails every attempt", member: "u1", condition: accepting, deliverer: undelivered,
			want: Result{
				Failures: map[string]int{"deliveries/d": 1}, Members: 1, Eligible: 1,
				Retries: defaultAttempts - 1,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			provider := &pager{pages: map[string]cohort.Page{"": {Users: []string{tc.member}}}}
			result, err := testRun(provider, tc.deliverer, tc.condition).run(t.Context())

			require.NoError(t, err)
			assert.Equal(t, tc.want, result)
		})
	}
}

// A failure the campaign's own end causes counts nowhere: a condition, the deliverer or the cohort's listing
// failing as the campaign's context ends (here, ended by the plugin itself).
func TestRunCountsNothingTheCampaignsEndCutShort(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		run  func(cancel context.CancelFunc) *campaignRun
		name string
		want Result
		ends bool // whether Run returns the campaign's end
	}{
		{
			name: "a condition",
			run: func(cancel context.CancelFunc) *campaignRun {
				return testRun(onePage(), delivered, conditionFunc(func(context.Context, eligibility.Subject) (
					eligibility.Decision, error,
				) {
					cancel()

					return eligibility.Decision{}, errBroken
				}))
			},
			want: Result{Failures: map[string]int{}, Members: 1},
		},
		{
			name: "the deliverer",
			run: func(cancel context.CancelFunc) *campaignRun {
				return testRun(onePage(), delivererFunc(func(context.Context, delivery.Reward) (delivery.Receipt, error) {
					cancel()

					return delivery.Receipt{}, errBroken
				}), accepting)
			},
			want: Result{Failures: map[string]int{}, Members: 1, Eligible: 1},
		},
		{
			name: "the cohort's listing",
			run: func(cancel context.CancelFunc) *campaignRun {
				return testRun(&pager{pages: map[string]cohort.Page{}, list: cancel}, delivered)
			},
			want: Result{Failures: map[string]int{}},
			ends: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			result, err := tc.run(cancel).run(ctx)
			if tc.ends {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tc.want, result)
		})
	}
}

// testRun returns a run of the campaign "c" over provider's cohort "c", the conditions "0", "1"… in order and
// the deliverer "d", on a platform with the default limits but a microsecond's backoff, whose directory knows u1
// and u2, fails for "broken" and knows no other user.
func testRun(
	provider cohort.Provider, deliverer delivery.Deliverer, conditions ...eligibility.Condition,
) *campaignRun {
	served := &Platform{
		users:  directory{"u1": {ID: "u1"}, "u2": {ID: "u2"}},
		tracer: noop.NewTracerProvider().Tracer(""),
		limits: (&Limits{RetryBackoff: time.Microsecond}).withDefaults(),
	}
	named := make([]condition, len(conditions))

	for i, plugin := range conditions {
		named[i] = condition{plugin: plugin, key: fmt.Sprintf("%s/%d", Conditions, i)}
	}

	return served.runOf(&Campaign{ID: "campaign", Cohort: "c", Deliverer: "d"}, provider, deliverer, named)
}

// users returns n member IDs.
func users(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("u%d", i+1)
	}

	return ids
}

// directory is a user directory of fixed users, whose Lookup fails for "broken".
type directory map[string]cohort.User

func (d directory) Lookup(_ context.Context, id string) (cohort.User, error) {
	if id == "broken" {
		return cohort.User{}, errBroken
	}

	return d[id], nil
}

func (directory) Page(context.Context, string, int) (cohort.UserPage, error) {
	return cohort.UserPage{}, errBroken
}

// pager is a cohort listing fixed pages by cursor; it fails at a cursor it has no page for, after calling list
// (when set), and once listed maxLists times, so that a platform listing a page forever fails the test instead
// of hanging it. It counts its List calls; a run calls them one at a time.
type pager struct {
	pages map[string]cohort.Page
	list  func()
	lists int
}

// maxLists is how many List calls a pager answers.
const maxLists = 10

// onePage returns a cohort of the member u1.
func onePage() *pager {
	return &pager{pages: map[string]cohort.Page{"": {Users: []string{"u1"}}}}
}

func (*pager) IsMember(context.Context, string) (bool, error) {
	return false, errBroken
}

func (p *pager) List(_ context.Context, cursor string) (cohort.Page, error) {
	p.lists++

	page, ok := p.pages[cursor]
	if !ok || p.lists > maxLists {
		if p.list != nil {
			p.list()
		}

		return cohort.Page{}, errBroken
	}

	return page, nil
}

// conditionFunc is a condition deciding with its function.
type conditionFunc func(ctx context.Context, subject eligibility.Subject) (eligibility.Decision, error)

//nolint:gocritic // hugeParam: eligibility.Condition's signature
func (f conditionFunc) Eligible(ctx context.Context, subject eligibility.Subject) (eligibility.Decision, error) {
	return f(ctx, subject)
}

// verdict is a condition deciding every member alike.
type verdict struct {
	err      error
	decision eligibility.Decision
}

func (v verdict) Eligible(context.Context, eligibility.Subject) (eligibility.Decision, error) {
	return v.decision, v.err
}

// The conditions of the tests: accepting every member, refusing every member, failing for every member.
var (
	accepting = verdict{decision: eligibility.Decision{Eligible: true}} //nolint:gochecknoglobals // a fake
	refusing  = verdict{}                                               //nolint:gochecknoglobals // a fake
	failing   = verdict{err: errBroken}                                 //nolint:gochecknoglobals // a fake
)

// delivererFunc is a deliverer delivering with its function.
type delivererFunc func(ctx context.Context, reward delivery.Reward) (delivery.Receipt, error)

func (f delivererFunc) Deliver(ctx context.Context, reward delivery.Reward) (delivery.Receipt, error) {
	return f(ctx, reward)
}

// outcome is a deliverer ending every attempt alike.
type outcome struct {
	err     error
	receipt delivery.Receipt
}

func (o outcome) Deliver(context.Context, delivery.Reward) (delivery.Receipt, error) {
	return o.receipt, o.err
}

// The deliverers of the tests: delivering every reward, failing every attempt.
var (
	delivered   = outcome{receipt: delivery.Receipt{ID: "receipt"}} //nolint:gochecknoglobals // a fake
	undelivered = outcome{err: errBroken}                           //nolint:gochecknoglobals // a fake
)
