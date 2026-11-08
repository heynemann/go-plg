package plg

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"time"
)

// ValidationPolicy says what an on-demand validation ([Validate], [ValidateAll]) that fails does to the plugin
// version it validated; see [WithOnDemandValidationPolicy].
type ValidationPolicy uint8

// The on-demand validation policies.
const (
	// Degrade keeps serving a version whose on-demand validation failed, logged and marked degraded until a later
	// on-demand validation of it passes. It is the default.
	Degrade ValidationPolicy = iota
	// Unpublish removes a version whose on-demand validation failed from the catalogue, as a reload removes a
	// plugin no longer listed: Get fails with [ErrNotFound], calls in flight on it finish there, then its
	// instances are closed and its memory reservation is given back. A later reload that loads and validates the
	// plugin again publishes it again.
	Unpublish
)

var (
	errPolicyTwice      = errors.New("WithOnDemandValidationPolicy given more than once")
	errPolicyUnknown    = errors.New("WithOnDemandValidationPolicy with an unknown policy")
	errPolicyValidation = errors.New("WithOnDemandValidationPolicy without WithValidation")
)

// WithOnDemandValidationPolicy sets what an on-demand validation ([Validate], [ValidateAll]) of the Register's
// plugins that fails does: an issue of severity error (or of a severity plg does not know), or a Validate that
// failed as [WithValidation] describes. [Degrade], the default, keeps the version serving, marked degraded until a
// later on-demand validation passes; [Unpublish] removes it from the catalogue until a reload loads it again. A
// validation that could not run (no budget room, its context ended, the container closed) changes nothing.
// Register fails with [ErrInvalidOption] for another policy, the option given twice or without [WithValidation].
func WithOnDemandValidationPolicy(policy ValidationPolicy) RegisterOption {
	return func(c *registerConfig) {
		c.policies = append(c.policies, policy)
	}
}

// onDemandPolicy returns the Register's on-demand validation policy, checking its options.
func onDemandPolicy(cfg *registerConfig) (ValidationPolicy, error) {
	switch {
	case len(cfg.policies) == 0:
		return Degrade, nil
	case len(cfg.policies) > 1:
		return 0, fmt.Errorf("%w: %w", ErrInvalidOption, errPolicyTwice)
	case cfg.policies[0] > Unpublish:
		return 0, fmt.Errorf("%w: %w: %d", ErrInvalidOption, errPolicyUnknown, cfg.policies[0])
	case len(cfg.validations) == 0:
		return 0, fmt.Errorf("%w: %w", ErrInvalidOption, errPolicyValidation)
	default:
		return cfg.policies[0], nil
	}
}

// Validate validates now the version of the plugin key ("<name>/<descriptor key>") the container serves, as its
// loads do ([WithValidation]): it asks the Register's function for the plugin's input and calls the plugin's
// Validate with it in a fresh, throwaway instance under [WithValidationTimeout], then closes the instance. The
// instance's memory limit is charged to the container's [WithMemoryBudget] while it runs, beyond every
// reservation (the version's own pool keeps its room): when the budget has no room left Validate fails at once
// with [ErrMemoryBudget], without waiting. The validation counts as a call in flight on the version, so a reload
// replacing it or [Container.Close] waits for it.
//
// The returned [Report] lists the issues found, each logged as a load's are; its Err says why the validation
// failed ([ErrBlockingIssue] for issues of severity error, [ErrValidation] when Validate failed), nil when it
// passed. It is kept with the version as its latest validation, and a failure does what the Register's
// [WithOnDemandValidationPolicy] says. Validate returns an error, and changes nothing, only when the validation
// could not run: [ErrNotFound] for a key the container does not serve, [ErrNoValidation] for a plugin
// registered without WithValidation and [ErrClosed], each with a zero report, or [ErrMemoryBudget] and ctx's
// error, the report's Err then being that error too (as [Check] does). It is safe for concurrent use.
func Validate(ctx context.Context, c *Container, key string) (Report, error) {
	return c.validateKey(ctx, key)
}

// validateKey is [Validate].
func (c *Container) validateKey(ctx context.Context, key string) (Report, error) {
	gen, err := c.serving(key)
	if err != nil {
		return Report{}, fmt.Errorf("plg: validating %s: %w", key, err)
	}

	report, unpublished, err := c.validateServing(ctx, gen)

	gen.leave()

	if unpublished && c.closed.Load() {
		<-gen.closed // Close may have missed the retired version: its closing must not outlive this call
	}

	return report, err
}

// ValidateAll validates every plugin the container serves whose Register has [WithValidation], one at a time in
// key order, as [Validate] does, and yields each one's key and report; plugins registered without validation are
// skipped, and so are those removed before their turn. A validation that could not run yields a report holding
// the version and the error in Err; once ctx ends or the container closes, nothing more runs. The plugins are
// those served when the iteration starts, each validated at the version served when its turn comes.
func ValidateAll(ctx context.Context, c *Container) iter.Seq2[string, Report] {
	return c.validateAll(ctx)
}

// validateAll is [ValidateAll].
func (c *Container) validateAll(ctx context.Context) iter.Seq2[string, Report] {
	return func(yield func(string, Report) bool) {
		for _, gen := range c.validatingPlugins() {
			report, err := c.validateKey(ctx, gen.key)

			switch {
			case errors.Is(err, ErrNotFound):
				continue
			case err != nil && report.Key == "": // closed meanwhile
				report = Report{
					At: time.Now(), Err: err, Owner: *gen.desc.owner(), Key: gen.key, Version: gen.desc.Version,
				}
			default:
			}

			if !yield(gen.key, report) || ctx.Err() != nil || c.closed.Load() {
				return
			}
		}
	}
}

// validatingPlugins returns the versions c serves whose Register validates its plugins, in key order.
func (c *Container) validatingPlugins() []*plugin {
	var gens []*plugin

	for _, gen := range c.catalogue.Load().plugins {
		if gen.reg != nil && gen.reg.validation != nil {
			gens = append(gens, gen)
		}
	}

	slices.SortFunc(gens, func(a, b *plugin) int { return cmp.Compare(a.key, b.key) })

	return gens
}

// validateServing validates gen, a version c serves, entered by the caller; see [Validate]. It also reports
// whether it unpublished gen.
func (c *Container) validateServing(ctx context.Context, gen *plugin) (Report, bool, error) {
	val := gen.reg.validation
	if val == nil {
		return Report{}, false, fmt.Errorf("plg: validating %s: %w: register it WithValidation", gen.key,
			ErrNoValidation)
	}

	report := &Report{At: time.Now(), Owner: *gen.desc.owner(), Key: gen.key, Version: gen.desc.Version}
	info := PluginInfo{
		Key: gen.key, Version: gen.desc.Version, Language: gen.desc.Language, Storage: gen.storage,
		Owner: report.Owner,
	}

	// The version's pool keeps its reservation: the validation instance needs room beyond it.
	err := c.runValidation(ctx, val, gen.launch, &info, gen.pool.limit, gen.pool.limit, report)
	if errors.Is(err, errNoValidate) { // Create returned an instance of another type than at the version's load
		err = fmt.Errorf("%w: the instance Create returned has no Validate", ErrValidation)
	}

	outcome := c.countValidation(ctx, gen.reg, gen.launch, triggerOnDemand, err, report.Issues)

	if outcome == validationUnavailable {
		err = fmt.Errorf("plg: validating %s: %w", gen.key, err)
		report.Err, report.Duration = err, time.Since(report.At)

		return *report, false, err
	}

	report.Err, report.Duration = err, time.Since(report.At)
	c.logIssues(ctx, report)

	return *report, c.validated(ctx, gen, report), nil
}

// validated keeps report, an on-demand validation of gen, as gen's latest, unless one started later was kept
// already, emits its [EventValidated] event and applies the Register's policy to its outcome. It reports whether it
// unpublished gen.
func (c *Container) validated(ctx context.Context, gen *plugin, report *Report) bool {
	var prev *Report

	for {
		prev = gen.validated.Load()
		if prev != nil && prev.At.After(report.At) {
			return false // a later validation decided
		}

		if gen.validated.CompareAndSwap(prev, report) {
			break
		}
	}

	c.events.generation(ctx, EventValidated, gen, report)

	attrs := []slog.Attr{
		slog.String("key", gen.key), gen.launch.id.owner.log, slog.String("version", gen.desc.Version),
	}

	switch {
	case report.Err == nil:
		if prev != nil && prev.Err != nil {
			c.logger.LogAttrs(ctx, slog.LevelInfo, "plg: plugin validation passed again", attrs...)
		}

		return false
	case gen.reg.validation.policy == Unpublish:
		return c.unpublish(ctx, gen, report, attrs)
	default:
		c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: plugin degraded: its validation failed",
			append(attrs, slog.String("error", report.Err.Error()))...)

		return false
	}
}

// unpublish removes gen, whose on-demand validation failed with report, from the catalogue and retires it,
// unless the catalogue no longer serves it (a reload replaced or removed it, or the container closed). The report
// becomes the key's rejection, as a retirement, until a reload loads the plugin again. It reports whether it
// unpublished gen.
func (c *Container) unpublish(ctx context.Context, gen *plugin, report *Report, attrs []slog.Attr) bool {
	for {
		old := c.catalogue.Load()
		if old.closed || old.plugins[gen.key] != gen {
			return false
		}

		if c.catalogue.CompareAndSwap(old, old.with(gen.iface, nil, []string{gen.key})) {
			break
		}
	}

	if entry, listed := gen.reg.keys.Load(gen.key); listed { // a reload may have stopped listing it meanwhile
		entry.rejection.Store(retireRejection(gen, report))
	}

	c.events.generation(ctx, EventRejected, gen, report)
	c.retire(ctx, gen)

	c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: plugin unpublished: its validation failed",
		append(attrs, slog.String("error", report.Err.Error()))...)

	return true
}
