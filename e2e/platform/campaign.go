package platform

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/rewards/cohort"
	"example.com/rewards/delivery"
	"example.com/rewards/eligibility"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/heynemann/go-plg"
)

// Directory is the key of a Result's failures that no plugin caused: the platform's user directory failed to
// look a member up, or knows no such user.
const Directory = "directory"

// Campaign is a reward campaign: every member of a cohort for whom every condition holds gets the reward, through
// a deliverer. Plugins are named by their key in the storage (the directory or artifact name: "country"), without
// the registration's name.
type Campaign struct {
	// Conditions are the conditions' keys, asked in this order until one says no; none makes every member
	// eligible.
	Conditions []string
	// ID identifies the campaign; with each member's ID it makes the reward's idempotency key.
	ID string
	// Cohort is the cohort provider's key.
	Cohort string
	// Deliverer is the deliverer's key.
	Deliverer string
	// Reward is what each eligible member is credited with.
	Reward Reward
}

// Reward is what a campaign credits each eligible member with.
type Reward struct {
	// Memo describes the reward to the user.
	Memo string
	// Points is the amount credited.
	Points int64
}

// Result is what a campaign did.
type Result struct {
	// Failures counts the members a plugin's errors kept from a decision or a delivery, by the plugin's key
	// ("conditions/country"; Directory when no plugin caused it); a cohort whose listing failed counts once. A
	// member counts at most once, against the plugin that failed it, and not at all when the campaign's ctx
	// ended first.
	Failures map[string]int
	// Members is how many users the cohort listed.
	Members int
	// Eligible is how many members every condition accepted.
	Eligible int
	// Delivered is how many eligible members the deliverer credited.
	Delivered int
	// Duplicates is how many of those deliveries the destination had already applied: an earlier attempt, cut
	// short (a timeout), had reached it, so the retry's idempotency key kept it from applying the reward again.
	Duplicates int
	// Retries is how many delivery attempts followed a failed one.
	Retries int
}

// ErrCampaign reports a campaign Run cannot start or could not finish.
var ErrCampaign = errors.New("platform: campaign")

// campaignRun is one Run of a campaign: its plugins and what it counts, shared by the goroutines processing its
// members.
type campaignRun struct {
	cohort     cohort.Provider
	deliverer  delivery.Deliverer
	failures   map[string]*atomic.Int64 // immutable: one counter per key a member can fail on
	platform   *Platform
	cohortKey  string
	deliverKey string
	campaign   Campaign
	conditions []condition
	members    atomic.Int64
	eligible   atomic.Int64
	delivered  atomic.Int64
	duplicates atomic.Int64
	retries    atomic.Int64
}

// condition is a campaign's condition and its plugin key.
type condition struct {
	plugin eligibility.Condition
	key    string
}

// Run runs campaign: it pages through the cohort (List), asks the conditions about each member, as many members
// at once as Limits.Concurrency, and delivers the reward to every member they all accept, with the idempotency key
// "<campaign ID>/<member ID>" and retries (Limits.Attempts, backing off from Limits.RetryBackoff): a
// delivery cut short may have reached its destination, so delivery is at least once and the key makes repeating
// it harmless.
//
// The campaign is one trace: a span campaign.run, its child campaign.user per member, and below those plg's spans
// of the plugin calls (plg.call <key>.<method>) and of their Host calls and HTTP requests.
//
// It fails before running when a plugin the campaign names is not served, and returns what it did with an error
// when the cohort cannot be listed (its key's failure counted) or ctx ends.
func (p *Platform) Run(ctx context.Context, campaign *Campaign) (Result, error) {
	ctx, span := p.tracer.Start(ctx, "campaign.run", trace.WithAttributes(
		attribute.String("campaign.id", campaign.ID), attribute.String("campaign.cohort", campaign.Cohort),
		attribute.String("campaign.deliverer", campaign.Deliverer),
		attribute.StringSlice("campaign.conditions", campaign.Conditions)))
	defer span.End()

	run, err := p.newRun(ctx, campaign)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())

		return Result{}, err
	}

	result, err := run.run(ctx)
	span.SetAttributes(attribute.Int("campaign.members", result.Members),
		attribute.Int("campaign.eligible", result.Eligible), attribute.Int("campaign.delivered", result.Delivered))

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}

	return result, err
}

// newRun gets the campaign's plugins.
func (p *Platform) newRun(ctx context.Context, campaign *Campaign) (*campaignRun, error) {
	if campaign.ID == "" {
		return nil, fmt.Errorf("%w: no ID", ErrCampaign)
	}

	provider, err := plg.Get[cohort.Provider](ctx, p.container, Cohorts+"/"+campaign.Cohort)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrCampaign, campaign.ID, err)
	}

	deliverer, err := plg.Get[delivery.Deliverer](ctx, p.container, Deliveries+"/"+campaign.Deliverer)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrCampaign, campaign.ID, err)
	}

	conditions := make([]condition, 0, len(campaign.Conditions))

	for _, name := range campaign.Conditions {
		key := Conditions + "/" + name

		plugin, err := plg.Get[eligibility.Condition](ctx, p.container, key)
		if err != nil {
			return nil, fmt.Errorf("%w %s: %w", ErrCampaign, campaign.ID, err)
		}

		conditions = append(conditions, condition{plugin: plugin, key: key})
	}

	return p.runOf(campaign, provider, deliverer, conditions), nil
}

// runOf returns a run of campaign with its plugins: provider, its cohort, deliverer, its deliverer, and
// conditions, its conditions in order, each with its key.
func (p *Platform) runOf(
	campaign *Campaign, provider cohort.Provider, deliverer delivery.Deliverer, conditions []condition,
) *campaignRun {
	run := &campaignRun{
		platform:   p,
		campaign:   *campaign,
		cohort:     provider,
		deliverer:  deliverer,
		conditions: conditions,
		cohortKey:  Cohorts + "/" + campaign.Cohort,
		deliverKey: Deliveries + "/" + campaign.Deliverer,
	}
	run.failures = map[string]*atomic.Int64{Directory: {}, run.cohortKey: {}, run.deliverKey: {}}

	for _, cond := range conditions {
		run.failures[cond.key] = &atomic.Int64{}
	}

	return run
}

// run lists the cohort's members and processes them, as many at once as Limits.Concurrency, and returns what it
// did, with the error that ended the listing early.
func (r *campaignRun) run(ctx context.Context) (Result, error) {
	members := make(chan string)

	var workers sync.WaitGroup

	for range r.platform.limits.Concurrency {
		workers.Go(func() {
			for member := range members {
				r.member(ctx, member)
			}
		})
	}

	err := r.list(ctx, members)

	close(members)
	workers.Wait()

	return r.result(), err
}

// list sends the cohort's members to members, a page at a time, until the last page, a page it cannot get, or
// ctx's end.
func (r *campaignRun) list(ctx context.Context, members chan<- string) error {
	cursor := ""

	for {
		var page cohort.Page

		_, err := r.retry(ctx, func(ctx context.Context) error {
			var err error

			page, err = r.cohort.List(ctx, cursor)

			return err //nolint:wrapcheck // plg's error, wrapped below
		})
		if err == nil {
			err = checkPage(page, cursor)
		}

		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w %s: %w", ErrCampaign, r.campaign.ID, ctx.Err())
			}

			r.failures[r.cohortKey].Add(1)

			return fmt.Errorf("%w %s: listing %s at %q: %w", ErrCampaign, r.campaign.ID, r.cohortKey, cursor, err)
		}

		for _, member := range page.Users {
			select {
			case members <- member:
				r.members.Add(1)
			case <-ctx.Done():
				return fmt.Errorf("%w %s: %w", ErrCampaign, r.campaign.ID, ctx.Err())
			}
		}

		if page.Next == "" {
			return nil
		}

		cursor = page.Next
	}
}

// errPage is a page of a cohort the platform refuses.
var errPage = errors.New("invalid page")

// checkPage refuses a page larger than cohort.MaxPageSize, and one whose next cursor is the cursor it was asked
// with, which would list the same page forever.
func checkPage(page cohort.Page, cursor string) error {
	switch {
	case len(page.Users) > cohort.MaxPageSize:
		return fmt.Errorf("%w: %d users, over %d", errPage, len(page.Users), cohort.MaxPageSize)
	case page.Next != "" && page.Next == cursor:
		return fmt.Errorf("%w: the next cursor is the page's own", errPage)
	default:
		return nil
	}
}

// member asks the conditions about one member and delivers its reward when they all accept it, in a span
// campaign.user. A failure counts against the plugin that failed, unless the campaign's ctx ended: a member the
// campaign's end cut short counts nowhere.
func (r *campaignRun) member(ctx context.Context, id string) {
	ctx, span := r.platform.tracer.Start(ctx, "campaign.user", trace.WithAttributes(attribute.String("user.id", id)))
	defer span.End()

	key, err := r.decideAndDeliver(ctx, span, id)
	if err == nil {
		return
	}

	span.SetStatus(codes.Error, err.Error())

	if ctx.Err() == nil {
		r.failures[key].Add(1)
		span.SetAttributes(attribute.String("campaign.failed", key))
	}
}

// decideAndDeliver asks the conditions about the member and delivers its reward when they all accept it; on
// failure it returns the key of what failed.
func (r *campaignRun) decideAndDeliver(ctx context.Context, span trace.Span, id string) (string, error) {
	user, err := r.platform.users.Lookup(ctx, id)
	if err == nil && user.ID != id {
		err = fmt.Errorf("%w: the directory has no user %q", ErrCampaign, id)
	}

	if err != nil {
		return Directory, err
	}

	subject := eligibility.Subject{Campaign: r.campaign.ID, User: eligibility.User{
		ID: user.ID, Country: user.Country, Tier: user.Tier, SpendCents: user.SpendCents, Orders: user.Orders,
	}}

	for _, cond := range r.conditions {
		var decision eligibility.Decision

		err := r.attempt(ctx, func(ctx context.Context) error {
			var err error

			decision, err = cond.plugin.Eligible(ctx, subject)

			return err //nolint:wrapcheck // plg's error
		})
		if err != nil {
			return cond.key, err
		}

		if !decision.Eligible {
			span.SetAttributes(attribute.Bool("campaign.eligible", false),
				attribute.String("campaign.condition", cond.key), attribute.String("campaign.reason", decision.Reason))

			return "", nil
		}
	}

	r.eligible.Add(1)
	span.SetAttributes(attribute.Bool("campaign.eligible", true))

	return r.deliverKey, r.deliver(ctx, id)
}

// deliver delivers the campaign's reward to the member, retrying with the same idempotency key.
func (r *campaignRun) deliver(ctx context.Context, id string) error {
	reward := delivery.Reward{
		IdempotencyKey: r.campaign.ID + "/" + id,
		Campaign:       r.campaign.ID,
		User:           id,
		Points:         r.campaign.Reward.Points,
		Memo:           r.campaign.Reward.Memo,
	}

	var receipt delivery.Receipt

	attempts, err := r.retry(ctx, func(ctx context.Context) error {
		var err error

		receipt, err = r.deliverer.Deliver(ctx, reward)

		return err //nolint:wrapcheck // plg's error
	})
	r.retries.Add(int64(attempts - 1))

	if err != nil {
		return err
	}

	r.delivered.Add(1)

	if receipt.Duplicate {
		r.duplicates.Add(1)
	}

	return nil
}

// retry calls attempt, each time under Limits.CallTimeout, until it succeeds, fails for good, or has been tried
// Limits.Attempts times, waiting Limits.RetryBackoff before the second attempt and twice as long before
// each later one. It returns the attempts made and the last one's error.
func (r *campaignRun) retry(ctx context.Context, attempt func(context.Context) error) (int, error) {
	backoff := r.platform.limits.RetryBackoff

	for n := 1; ; n++ {
		err := r.attempt(ctx, attempt)
		if err == nil || n == r.platform.limits.Attempts || !retryable(ctx, err) {
			return n, err
		}

		timer := time.NewTimer(backoff)

		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()

			return n, err
		}

		backoff *= 2
	}
}

// attempt calls attempt under Limits.CallTimeout.
func (r *campaignRun) attempt(ctx context.Context, attempt func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, r.platform.limits.CallTimeout)
	defer cancel()

	return attempt(ctx)
}

// retryable reports whether a call that failed with err may succeed when tried again: any failure but the
// campaign's own end and those that repeat whatever the attempt (the plugin is gone, the platform is closing,
// the values cannot cross). A timeout, a trap, a lost instance and the plugin's own error (an API's 503) are
// retried: the plugin's error may be transient, and a call that failed may have had its effect, which the
// idempotency key makes safe to repeat.
func retryable(ctx context.Context, err error) bool {
	return ctx.Err() == nil &&
		!errors.Is(err, plg.ErrNotFound) && !errors.Is(err, plg.ErrClosed) && !errors.Is(err, plg.ErrConversion)
}

// result is what the run counted.
func (r *campaignRun) result() Result {
	result := Result{
		Failures:   map[string]int{},
		Members:    int(r.members.Load()),
		Eligible:   int(r.eligible.Load()),
		Delivered:  int(r.delivered.Load()),
		Duplicates: int(r.duplicates.Load()),
		Retries:    int(r.retries.Load()),
	}

	for key, failures := range r.failures {
		if n := failures.Load(); n > 0 {
			result.Failures[key] = int(n)
		}
	}

	return result
}
