package plg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/heynemann/go-plg/internal/memfs"
)

// checkStorage is the storage name a checked candidate's [PluginInfo] and errors carry.
const checkStorage = "plg.Check"

// Check dry-runs a candidate plugin version before it is enabled: it loads desc with files against the
// registration name, as a reload of that registration would load it from a storage (its contract, Host, HTTP
// policy, languages, memory limits, ownership rules and [WithValidation]), runs its Create and, with
// [WithValidation], its Validate in one throwaway instance, and returns the load's [Report]. Nothing is
// published: the catalogue, the registration's reports and its plugins stay as they were, and the candidate's
// issues are logged like a load's. files go through the same snapshot limits as a storage's ([MaxPluginSize]
// and the others, [ErrPluginTooLarge]), and a descriptor without a Version gets their content hash, as from a
// storage. The candidate's key need not be listed by the registration's storages.
//
// The instance runs like a validation instance, so its Create, its Validate and their Host methods and HTTP
// requests see [IsValidation]. Its memory limit is charged to the container's [WithMemoryBudget] beyond every
// reservation, as room no plugin reserved: a budget without that room fails the check with [ErrMemoryBudget]
// at once, never waiting for it, and the charge is given back when Check returns.
//
// The report says whether the candidate would load: its Err is why it would not (an invalid descriptor, another
// interface than the registration's ([ErrOtherInterface]), a language the registration does not enable or link,
// files over the limits, source that does not compile, a failing Create, an issue that blocks the load (a
// contract version the program cannot serve among them), a failed validation), as in the report of a load; Check
// itself then succeeds. Check fails when it cannot check: with [ErrNotFound] when name is not registered and
// [ErrClosed] once the container is closed (both without a report), with [ErrMemoryBudget] without room for
// the instance, and with ctx's error when ctx ends; the report's Err is then that error too. It is safe for
// concurrent use; desc is not modified.
func Check(ctx context.Context, c *Container, name string, desc *Descriptor, files fs.FS) (Report, error) {
	return c.checkCandidate(ctx, name, desc, files)
}

// checkCandidate is [Check].
func (c *Container) checkCandidate(ctx context.Context, name string, desc *Descriptor, files fs.FS) (
	Report, error,
) {
	reg, ok := c.regs.Load(name)

	switch {
	case c.closed.Load():
		return Report{}, ErrClosed
	case !ok:
		return Report{}, fmt.Errorf("%w: no interface registered as %q", ErrNotFound, name)
	default:
	}

	return c.checkAgainst(ctx, reg, checkStorage, desc, files)
}

// checkAgainst checks the candidate desc with files against reg, as listed by the storage named storeName.
func (c *Container) checkAgainst(ctx context.Context, reg *registration, storeName string, desc *Descriptor,
	files fs.FS,
) (Report, error) {
	ctx = context.WithValue(ctx, checkKey{}, true) // its PanicErrors emit no events
	src := source{storeName: storeName, desc: *desc}
	key := reg.name + "/" + desc.Key
	report := newReport(key, &src, time.Now())

	err := c.loadStep(ctx, key, &src, func() error { return c.check(ctx, reg, key, &src, files, report) })
	c.finishReport(ctx, report, &src, err)

	if err != nil && (errors.Is(err, ErrMemoryBudget) || errors.Is(err, ErrClosed) || ctx.Err() != nil) {
		return *report, fmt.Errorf("plg: checking %s: %w", key, err)
	}

	return *report, nil
}

// check loads the candidate src, with the files opened, against reg and runs it in a throwaway instance charged
// beyond the reservations, adding what it finds to report.
func (c *Container) check(ctx context.Context, reg *registration, key string, src *source, opened fs.FS,
	report *Report,
) error {
	cand, err := c.prepareCandidate(ctx, reg, key, src, opened, report)
	if err != nil {
		return err
	}

	if reg.validation != nil {
		found := len(report.Issues)
		err = c.validate(ctx, reg, src, cand.files, cand.launch, src.limit, report)
		c.countValidation(ctx, reg, cand.launch, triggerCheck, err, report.Issues[found:])

		return err
	}

	inst, release, err := c.startCharged(ctx, cand.launch, src.limit, src.limit)
	if err != nil {
		return err
	}

	defer release()

	c.closeValidation(ctx, cand.launch, inst, c.closeTimeout)

	return nil
}

// candidate is a plugin version checked against a registration, ready to start instances: their launcher, the
// meters they are measured in and the version's files.
type candidate struct {
	launch *launcher
	meters *pluginMeters
	files  *memfs.FS
}

// prepareCandidate checks the candidate src against reg, copies the files opened and prepares the launcher of
// its instances, adding the issues found to report; no instance starts.
func (c *Container) prepareCandidate(ctx context.Context, reg *registration, key string, src *source,
	opened fs.FS, report *Report,
) (*candidate, error) {
	var err error

	src.ref, err = checkDescriptor(src.storeName, &src.desc)
	if err != nil {
		return nil, err
	}

	_, err = reg.skips(src)
	if err != nil {
		return nil, err
	}

	src.loader, src.guest, err = reg.cfg.guestFor(src.desc.Language)
	if err != nil {
		return nil, err
	}

	src.limit = instanceLimit(reg.cfg.limit(src.loader), &src.desc)

	cand := &candidate{}

	cand.files, err = snapshotPlugin(ctx, src, opened)
	if err != nil {
		return nil, err
	}

	cand.launch, cand.meters, err = c.prepareLaunch(ctx, reg, key, src, cand.files, report)
	if err != nil {
		return nil, err
	}

	return cand, nil
}
