package plg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/wire"
)

// defaultValidationTimeout bounds a plugin's Validate when the Register sets no [WithValidationTimeout].
const defaultValidationTimeout = 10 * time.Second

// CodeValidationInput is the [Issue.Code] of the error-severity issue recording that the [WithValidation]
// function failed (returned an error or panicked) for a plugin, which then cannot be validated.
const CodeValidationInput = "plg.validation.input"

// PluginInfo describes a plugin version: the one a [WithValidation] function is asked the validation input of, or
// a [WithSecrets] function a secret of.
type PluginInfo struct {
	Key      string
	Version  string
	Language Language
	Storage  string
	Owner    Owner
}

// WithValidation makes the Register's plugins validate themselves before they go live. Every load (at Register
// and on each reload), before any of the plugin's pool instances starts, plg asks input for the plugin's
// validation input and calls the plugin's Validate with it in a separate, throwaway instance, once its Create
// succeeded. Validate is a method of the instance Create returns, beside the interface's methods, looked up by
// name like Close:
//
//	func (r *rule) Validate(ctx context.Context, in contract.V) ([]plugin.Issue, error) // Go
//	function Rule:Validate(v) return { { severity = "warning", code = "rule.slow", message = "..." } } end -- Lua
//	Validate(v: V): Issue[] | Promise<Issue[]> { return []; } // TypeScript
//
// plugin is github.com/heynemann/go-plg/plugin. Lua and TypeScript receive v as they receive any argument (a
// table, an object) and return a list of issue tables or objects with the fields severity, code, message and
// field (a missing one is empty; nil or undefined is no issue at all). Failing Validate the language's way is a
// failure: a Go error, Lua's nil and a message or error(...), a TypeScript throw or rejected Promise.
//
// The issues it returns go into the load's [Report] and the log; one of severity error (or a severity plg does
// not know) blocks the load, and so do a failing Validate, a panic or trap in it, a Validate that does not return
// (or whose Promise does not settle) within [WithValidationTimeout], an instance without a Validate (for Go, of
// exactly that signature; the error shows the one the plugin declares) and an error from input, which is
// recorded as a [CodeValidationInput] issue. A blocked load fails Register with [ErrLoad] (wrapping
// [ErrBlockingIssue] for issues, [ErrValidation] otherwise); a reload keeps serving the plugin's last good
// version.
//
// The validation instance runs in the same sandbox as the plugin's calls, with the Register's Host and HTTP
// policy; the context its Create, its Validate and their Host methods and HTTP requests get tells them apart:
// [IsValidation]. Its memory limit is charged to the container's [WithMemoryBudget] while it runs, within the
// plugin version's own reservation (its pool's maximum): it runs, and is closed, before the pool's instances
// start, so a budget sized exactly to the plugins' worst case still validates them, and a budget without room
// for the version fails the load with [ErrMemoryBudget] without waiting. input runs on the loading
// goroutine with the load's context; a panic in it is recovered and reported as a [PanicError] in
// [PhaseCallback].
//
// [Validate] and [ValidateAll] run the same validation on demand, on the versions the container serves; what a
// failure then does is the Register's [WithOnDemandValidationPolicy]. Every validation is counted in the
// plg.plugin.validations and plg.plugin.validation.issues metrics ([WithMeterProvider]).
//
// V is the type of the validation input: a named data type (see the interface contract) declared in the
// registered interface's package, whose source Go plugins interpret, so they can name it. Register fails with
// [ErrInvalidOption] for any other V, a nil input or the option given twice.
func WithValidation[V any](input func(ctx context.Context, info PluginInfo) (V, error)) RegisterOption {
	spec := validationSpec{typ: reflect.TypeFor[V](), missing: input == nil}
	spec.input = func(ctx context.Context, info *PluginInfo) (reflect.Value, error) {
		in, err := input(ctx, *info)

		return reflect.ValueOf(&in).Elem(), err
	}

	return func(c *registerConfig) {
		c.validations = append(c.validations, spec)
	}
}

// WithValidationTimeout bounds how long a plugin's Validate ([WithValidation]) may run, never past the load's own
// context; a Validate still running then is stopped and the load fails. It must be positive, otherwise Register
// fails with [ErrInvalidOption]. The default is 10 seconds.
func WithValidationTimeout(d time.Duration) RegisterOption {
	return func(c *registerConfig) {
		c.validationTimeout = d
	}
}

// validationKey marks the context of a validation instance; see [IsValidation].
type validationKey struct{}

// IsValidation reports whether ctx is the context of a plugin's validation ([WithValidation]): the context the
// Host methods called while a validation instance runs Create or Validate get, and that of its HTTP requests,
// whose log records carry validation=true.
func IsValidation(ctx context.Context) bool {
	marked, ok := ctx.Value(validationKey{}).(bool)

	return ok && marked
}

// validationSpec is a WithValidation option: the validation input's type and the function returning it.
type validationSpec struct {
	input func(ctx context.Context, info *PluginInfo) (reflect.Value, error)
	typ   reflect.Type
	// described is the input as the Go dispatcher writes it, when plg validate gave it (the Go interpreter's types
	// are unnamed): the contract's source declares it, as checked; nil describes typ.
	described *goabi.Type
	missing   bool // the function is nil
}

// validation is a Register call's validation, checked once: the input function, the codecs of the input and of
// the issues Validate returns, the Validate Go plugins must declare and the guest method index it is called at.
type validation struct {
	input     func(ctx context.Context, info *PluginInfo) (reflect.Value, error)
	typ       reflect.Type
	described *goabi.Type // the input as the Go dispatcher writes it; nil: goabi.TypeOf(typ)
	codec     *wire.Codec // of the input
	issues    *wire.Codec // of []Issue, which Validate's []plugin.Issue encodes as
	// scriptIssues is the codec of []scriptIssue, which describes the issues to the script guests: the same
	// encoding as issues.
	scriptIssues *wire.Codec
	want         string // the method Go plugins declare, for messages
	timeout      time.Duration
	index        uint32           // the number of the interface's methods: guests serve Validate after them
	policy       ValidationPolicy // what a failed on-demand validation does
}

// scriptIssue is an [Issue] as Lua and TypeScript plugins write it, with lower-case field names. It has Issue's
// fields in Issue's order, so it crosses the boundary as Issue does: the script guests learn its names from the
// schema, and the host decodes the issues into []Issue.
type scriptIssue struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Field    string   `json:"field"`
}

var (
	errValidationTwice   = errors.New("WithValidation given more than once")
	errValidationNil     = errors.New("WithValidation with a nil function")
	errValidationType    = errors.New("is not a named data type declared in the contract package")
	errValidationTimeout = errors.New("WithValidationTimeout must be positive")
)

// newValidation checks the Register call's validation options against the contract and prepares its validation;
// it returns nil without one.
func newValidation(contr *contract, cfg *registerConfig) (*validation, error) {
	policy, err := checkValidationOptions(cfg)
	if err != nil {
		return nil, err
	}

	switch len(cfg.validations) {
	case 0:
		return nil, nil //nolint:nilnil // no WithValidation: the plugins do not validate, which is not an error
	case 1:
	default:
		return nil, fmt.Errorf("%w: %w", ErrInvalidOption, errValidationTwice)
	}

	spec := cfg.validations[0]
	if spec.missing {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOption, errValidationNil)
	}

	typ := spec.typ

	name, err := inputName(contr, &spec)
	if err != nil {
		return nil, err
	}

	var planner wire.Planner

	val := &validation{
		input: spec.input, typ: typ, described: spec.described, timeout: cfg.validationTimeout, policy: policy,
		index: uint32(len(contr.methods)), //nolint:gosec // G115: an interface has far fewer than 2^32 methods
	}

	val.codec, err = planner.For(typ)
	if err == nil {
		val.issues, err = planner.For(reflect.TypeFor[[]Issue]())
	}

	if err == nil {
		val.scriptIssues, err = planner.For(reflect.TypeFor[[]scriptIssue]())
	}

	if err != nil { // a data type whose fields share a name for other guest languages
		return nil, fmt.Errorf("%w: WithValidation input %s: %w", ErrInvalidOption, typ, err)
	}

	val.want = "Validate(ctx context.Context, in " + name + ") ([]plugin.Issue, error)"

	return val, nil
}

// inputName checks the validation input of spec against the contract, unless plg validate described it from the
// contract's source, which declares it, and returns how Go plugins write its type.
func inputName(contr *contract, spec *validationSpec) (string, error) {
	if spec.described != nil {
		return spec.described.String(), nil
	}

	err := checkValidationInput(contr, spec.typ)
	if err != nil {
		return "", err
	}

	return spec.typ.String(), nil
}

// checkValidationOptions checks the Register call's validation timeout and on-demand policy, returning the
// policy.
func checkValidationOptions(cfg *registerConfig) (ValidationPolicy, error) {
	if cfg.validationTimeout <= 0 {
		return 0, fmt.Errorf("%w: %w, got %s", ErrInvalidOption, errValidationTimeout, cfg.validationTimeout)
	}

	return onDemandPolicy(cfg)
}

// checkValidationInput checks that typ, a WithValidation input type, is a named, non-generic data type declared
// in the contract's package, failing with [ErrInvalidOption] otherwise.
func checkValidationInput(contr *contract, typ reflect.Type) error {
	if typ.Name() == "" || typ.PkgPath() == "" || strings.ContainsRune(typ.Name(), '[') {
		return fmt.Errorf("%w: WithValidation input %s %w %s", ErrInvalidOption, typ, errValidationType,
			contr.pkgPath)
	}

	err := checkData(typ, map[reflect.Type]bool{})
	if err != nil {
		return fmt.Errorf("%w: WithValidation input %s %w %s: %w", ErrInvalidOption, typ, errValidationType,
			contr.pkgPath, err)
	}

	if typ.PkgPath() != contr.pkgPath {
		return fmt.Errorf("%w: WithValidation input %s %w %s of %s, whose source Go plugins interpret; it is in %s",
			ErrInvalidOption, typ, errValidationType, contr.pkgPath, contr.described.Type, typ.PkgPath())
	}

	return nil
}

// validate runs the Validate of a loading plugin version in a throwaway instance started by launch, before the
// version's pool warms, adding the issues it returns to report; it does nothing when the Register does not
// validate. extra is what the instance reserves beyond the reservations (see [Container.startCharged]). It
// fails when an issue blocks the load, the plugin cannot be validated (see [WithValidation]) or its Create fails.
func (c *Container) validate(ctx context.Context, reg *registration, src *source, files *memfs.FS,
	launch *launcher, extra uint64, report *Report,
) error {
	val := reg.validation
	if val == nil {
		return nil
	}

	info := PluginInfo{
		Owner: *src.desc.owner(), Key: launch.id.key, Version: src.desc.Version, Language: launch.id.lang,
		Storage: src.storeName,
	}

	err := c.runValidation(ctx, val, launch, &info, src.limit, extra, report)

	return explainNoValidate(err, src, files, val)
}

// explainNoValidate returns err, or for errNoValidate the [ErrValidation] saying what Validate the plugin src,
// with the files loaded, lacks.
func explainNoValidate(err error, src *source, files *memfs.FS, val *validation) error {
	if errors.Is(err, errNoValidate) {
		return fmt.Errorf("%w: %s", ErrValidation, src.loader.missingValidate(files, src.desc.Entrypoint, val))
	}

	return err
}

// runValidation asks val's input for the input of the plugin version info describes and runs its Validate with
// it in a throwaway instance (see [Container.validateInstance]), adding the issues found to report. It fails when
// an issue of report blocks, or with why the plugin could not be validated (errNoValidate as is).
func (c *Container) runValidation(ctx context.Context, val *validation, launch *launcher, info *PluginInfo,
	limit, extra uint64, report *Report,
) error {
	args, err := c.validationInput(ctx, val, launch, info)
	if err != nil {
		report.Issues = append(report.Issues, Issue{
			Severity: SeverityError, Code: CodeValidationInput,
			Message: "the WithValidation function failed: " + err.Error(),
		})

		return report.blocking()
	}

	issues, err := c.validateInstance(ctx, val, launch, limit, extra, args)

	report.Issues = append(report.Issues, issues...)

	if err != nil {
		return err
	}

	return report.blocking()
}

// validateInstance runs Validate with the encoded input args in a throwaway instance started by launch under the
// [IsValidation] context, charging limit to the budget without waiting and reserving extra bytes beyond the
// reservations ([ErrMemoryBudget] when there is no room; see [Container.startCharged]), then closes it. It
// returns the issues and the failure (errNoValidate as is, the caller maps it).
func (c *Container) validateInstance(ctx context.Context, val *validation, launch *launcher, limit uint64,
	extra uint64, args []byte,
) ([]Issue, error) {
	inst, release, err := c.startCharged(ctx, launch, limit, extra)
	if err != nil {
		return nil, err
	}

	defer release()

	issues, discard, err := c.callValidate(validating(ctx), val, launch, inst, args)

	closeTimeout := c.closeTimeout
	if discard { // whatever failed may leave Close unable to end
		closeTimeout = 0
	}

	c.closeValidation(ctx, launch, inst, closeTimeout)

	return issues, err
}

// startCharged starts a throwaway instance by launch under the [IsValidation] context, running the plugin's
// Create, after charging limit to the budget without waiting. extra is what it reserves beyond the
// reservations: zero within a version's own reservation (at load), the limit for room no plugin reserved
// (Check, on-demand validation). Without room it fails with [ErrMemoryBudget]; a failing Create fails it as it
// would in the pool. release gives the charge and the reservation back once the instance is closed.
//
//nolint:ireturn // the instance launcher.start returns, the pool's instance interface
func (c *Container) startCharged(ctx context.Context, launch *launcher, limit, extra uint64) (
	instance, func(), error,
) {
	if !c.budget.reserve(extra) {
		return nil, nil, fmt.Errorf("%w: the instance needs %d bytes beyond the reservations, and %d of the "+
			"%d-byte budget are reserved", ErrMemoryBudget, extra, c.budget.reserved.Load(), c.budget.total)
	}

	// At load, the version's reservation covers its pool's maximum and no pool instance runs yet, so there is
	// room for this one; a budget whose room is gone all the same fails rather than waits.
	if !c.budget.acquire(limit) {
		c.budget.unreserve(extra)

		return nil, nil, fmt.Errorf("%w: the instance needs %d bytes, and %d of the %d-byte budget are in use",
			ErrMemoryBudget, limit, c.budget.used.Load(), c.budget.total)
	}

	release := func() {
		c.budget.release(limit)
		c.budget.unreserve(extra)
	}

	inst, err := launch.start(validating(ctx))
	if err != nil {
		release()

		return nil, nil, err // the plugin's Create failed, as it would in its pool
	}

	return inst, release, nil
}

// validating returns ctx marked for [IsValidation].
func validating(ctx context.Context) context.Context {
	return context.WithValue(ctx, validationKey{}, true)
}

// validationInput asks the WithValidation function for the input of the plugin info describes and encodes it.
// It fails with why that failed instead: the function's error, its panic (reported as a [PanicError] in
// [PhaseCallback]) or an input that cannot be encoded.
func (c *Container) validationInput(ctx context.Context, val *validation, launch *launcher, info *PluginInfo) (
	[]byte, error,
) {
	var (
		input    reflect.Value
		err      error
		panicked any
	)

	func() {
		defer func() { panicked = recover() }()

		input, err = val.input(ctx, info)
	}()

	if panicked != nil {
		failure := launch.id.panicked(PhaseCallback)
		failure.Method = "WithValidation"
		c.panics.recovered(ctx, failure, panicked)

		return nil, fmt.Errorf("%w: %v", errInputPanic, panicked)
	}

	if err != nil {
		return nil, err
	}

	args, err := val.codec.Append(nil, input)
	if err != nil {
		return nil, fmt.Errorf("its input cannot cross the plugin boundary: %w", err)
	}

	return args, nil
}

// errInputPanic reports a WithValidation function that panicked.
var errInputPanic = errors.New("panic")

// errNoValidate reports a validation instance whose plugin instance has no Validate of the expected type.
var errNoValidate = errors.New("no Validate")

// callValidate calls Validate on inst, the validation instance, with the encoded input args, under the
// validation timeout, and decodes the issues it returns. The boolean reports an instance that must be discarded
// rather than closed. It fails with errNoValidate when the instance has no Validate of the expected type.
func (c *Container) callValidate(ctx context.Context, val *validation, launch *launcher, inst instance,
	args []byte,
) ([]Issue, bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, val.timeout)
	defer cancel()

	res, err := inst.Call(callCtx, val.index, args)

	switch {
	case errors.Is(err, engine.ErrStopped):
		return nil, true, fmt.Errorf("%w: %w", ErrValidation, ErrClosed)
	case err != nil && ctx.Err() == nil && callCtx.Err() != nil:
		failure := c.validateFailed(ctx, launch, inst, err)

		return nil, true, fmt.Errorf("%w: Validate did not return within the validation timeout (%s, "+
			"WithValidationTimeout): %w", ErrValidation, val.timeout, failure)
	case err != nil:
		return nil, true, fmt.Errorf("%w: %w", ErrValidation, c.validateFailed(ctx, launch, inst, err))
	default:
		return c.validateResult(ctx, val, launch, inst, &res)
	}
}

// validateResult decodes the issues of a Validate call that returned res, or says why it failed, like
// callValidate.
func (c *Container) validateResult(ctx context.Context, val *validation, launch *launcher, inst instance,
	res *engine.Result,
) ([]Issue, bool, error) {
	switch res.Status { //nolint:exhaustive // ABI statuses (or worse) fail the validation and break the instance
	case wire.StatusOK:
		var issues []Issue

		reader := wire.NewReader(res.Body)

		err := val.issues.Decode(&reader, reflect.ValueOf(&issues).Elem())
		if err == nil {
			err = reader.End()
		}

		if err != nil {
			return nil, true, fmt.Errorf("%w: %w", ErrValidation,
				c.validateFailed(ctx, launch, inst, fmt.Errorf("%w: issues: %w", engine.ErrABI, err)))
		}

		scrub := launch.secrets.redactor()

		for i := range issues {
			issue := &issues[i]
			issue.Code, issue.Message, issue.Field = scrub.String(issue.Code), scrub.String(issue.Message),
				scrub.String(issue.Field)
		}

		return issues, false, nil
	case wire.StatusLoad:
		return nil, false, errNoValidate
	case wire.StatusError:
		return nil, false, fmt.Errorf("%w: Validate returned an error: %w", ErrValidation, &PluginError{
			Key: launch.id.key, Method: goabi.ValidateMethod, Message: res.Message, Owner: launch.id.owner.owner,
		})
	case wire.StatusConvert:
		return nil, false, fmt.Errorf("%w: Validate returned issues that cannot cross the plugin boundary: %s",
			ErrValidation, res.Message)
	case wire.StatusPanic:
		return nil, true, fmt.Errorf("%w: %w", ErrValidation, c.validateFailed(ctx, launch, inst, res.Message))
	default:
		return nil, true, fmt.Errorf("%w: %w", ErrValidation, c.validateFailed(ctx, launch, inst,
			fmt.Errorf("%w %s: %s", errGuestStatus, res.Status, res.Message)))
	}
}

// validateFailed reports a Validate call the guest failed (a panic, a trap, its deadline, a broken ABI), with
// what the guest wrote to stderr meanwhile.
func (c *Container) validateFailed(ctx context.Context, launch *launcher, inst instance, value any) *PanicError {
	failure := launch.id.panicked(PhaseCall)
	failure.Method, failure.Value, failure.Guest = goabi.ValidateMethod, value, inst.Guest()
	c.panics.report(ctx, failure)

	return failure
}

// closeValidation closes the validation instance, running the plugin's Close within timeout (zero: not at all);
// a failure closing is logged.
func (c *Container) closeValidation(ctx context.Context, launch *launcher, inst instance, timeout time.Duration) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	err := inst.Close(closeCtx)

	cancel()

	if err != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "plg: closing a validation instance failed",
			slog.String("key", launch.id.key), launch.id.owner.log, slog.String("error", err.Error()))
	}
}
