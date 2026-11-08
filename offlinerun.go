package plg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"sync/atomic"
	"time"

	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/offline"
)

// errOneInstance refuses a second instance in a test case's pool, which serves one call in the instance the case
// started.
var errOneInstance = errors.New("a test case runs in one instance")

// offlineRunner is [offline.Runner]: the plugin plg validate runs test cases of, checked against the contract as
// [checkOffline] checks it, in a container of its own. Each call runs in a fresh instance started by the launcher
// prepared for the call's HTTP policy (one registration per policy, prepared on first use). It is used by one
// goroutine at a time, so prepared is not shared.
type offlineRunner struct {
	ctr *Container
	// options are the Register options of every registration: offlineOptions' with a pool of one instance.
	options    []RegisterOption
	contr      *contract
	hst        *host
	validation *offline.Validation
	files      fs.FS
	prepared   map[*offline.HTTP]*offlineLaunch
	desc       Descriptor
	timeout    time.Duration
}

// offlineLaunch is the plugin prepared under one HTTP policy, or why it could not be.
type offlineLaunch struct {
	err  error
	reg  *registration
	cand *candidate
	src  source
}

// openOffline is [offline.Open]: it prepares spec as [checkOffline] does, with spec's Host Funcs, in a container of
// its own, which the runner's Close closes.
//
//nolint:ireturn // it is offline.Open, which returns the offline.Runner interface
func openOffline(_ context.Context, spec *offline.Contract, plugin *offline.Plugin, opts *offline.Options,
) (offline.Runner, error) {
	desc, err := ParseManifest(plugin.Manifest)
	if err != nil {
		return nil, err
	}

	desc.Key = plugin.Key

	contr, err := offlineContract(spec)
	if err != nil {
		return nil, err
	}

	hst, err := stubHost(contr.pkgPath, contr.described.Type, spec.Host)
	if err != nil {
		return nil, err
	}

	timeout := opts.Limits.Timeout
	if timeout <= 0 {
		timeout = offline.DefaultTimeout
	}

	single := *opts
	single.Limits.PoolMin, single.Limits.PoolMax = 0, 0 // a pool of one instance: each call starts its own

	return &offlineRunner{
		//nolint:contextcheck // New takes no context: what the container starts ends with the runner's Close
		ctr:   New(WithLogger(opts.Logger), WithCompilationCacheDir(opts.CacheDir)),
		contr: contr, hst: hst, validation: spec.Validation, files: plugin.Files, desc: desc, timeout: timeout,
		options: offlineOptions(&single), prepared: map[*offline.HTTP]*offlineLaunch{},
	}, nil
}

// Run implements [offline.Runner].
func (r *offlineRunner) Run(ctx context.Context, call *offline.Call) offline.Outcome {
	launch := r.launch(ctx, call.HTTP)
	if launch.err != nil {
		return offline.Outcome{Err: launch.err}
	}

	if call.Method == len(r.contr.methods) {
		return r.validate(ctx, launch, call.Args[0])
	}

	return r.call(ctx, launch, r.contr.methods[call.Method], call.Args)
}

// Secrets implements [offline.Runner].
func (r *offlineRunner) Secrets() []string {
	return r.desc.Secrets
}

// Close implements [offline.Runner].
func (r *offlineRunner) Close(ctx context.Context) error {
	for _, launch := range r.prepared {
		if launch.reg != nil {
			launch.reg.http.Close()
		}
	}

	return r.ctr.Close(ctx)
}

// launch returns the plugin prepared under policy (nil: HTTP refused), preparing it on first use: a registration
// of the contract with the runner's Host, policy and validation, against which the plugin is checked as plg.Check
// checks a candidate.
func (r *offlineRunner) launch(ctx context.Context, policy *offline.HTTP) *offlineLaunch {
	launch, ok := r.prepared[policy]
	if !ok {
		launch = &offlineLaunch{src: source{storeName: offlineStorage, desc: r.desc}}
		launch.reg, launch.cand, launch.err = r.prepare(ctx, policy, &launch.src)
		r.prepared[policy] = launch
	}

	return launch
}

// prepare registers the contract under policy and prepares src against it.
func (r *offlineRunner) prepare(ctx context.Context, policy *offline.HTTP, src *source) (
	*registration, *candidate, error,
) {
	opts := append(slices.Clip(r.options), WithValidationTimeout(r.timeout))
	if policy != nil {
		opts = append(opts, WithHTTP(HTTPPolicy{
			AllowedHosts: policy.AllowedHosts, Timeout: policy.Timeout, MaxRequestBytes: policy.MaxRequestBytes,
			MaxResponseBytes: policy.MaxResponseBytes, MaxRequestsPerCall: policy.MaxRequestsPerCall,
		}))
	}

	cfg, err := registerOptions(offlineName, opts)
	if err != nil {
		return nil, nil, err
	}

	cfg.createTimeout = r.timeout
	cfg.secretTTL = 0 // each case fakes its own values: none may serve another case's reads

	if policy != nil {
		cfg.httpTransport = policy.Transport
	}

	if r.validation != nil {
		cfg.validations = []validationSpec{{typ: r.validation.Input, described: r.validation.Type}}
	}

	reg, err := r.ctr.newRegistration(r.contr, cfg, offlineName, r.hst)
	if err != nil {
		return nil, nil, err
	}

	key := offlineName + "/" + r.desc.Key

	cand, err := r.ctr.prepareCandidate(ctx, reg, key, src, r.files, newReport(key, src, time.Now()))
	if err != nil {
		reg.http.Close()

		return nil, nil, err
	}

	return reg, cand, nil
}

// start starts a fresh instance of launch under ctx, its Create bounded by the runner's timeout.
//
//nolint:ireturn // the instance launcher.start returns, the pool's instance interface
func (r *offlineRunner) start(ctx context.Context, launch *offlineLaunch) (instance, error) {
	inst, err := launch.cand.launch.start(ctx) // its Create bounded by the registration's createTimeout

	return inst, r.explain(ctx, errors.Is(err, context.DeadlineExceeded), goabi.CreateFunc, err)
}

// explain says that a step (subject: Create, or "it" for the call) failed by running past the timeout (timedOut:
// its own deadline passed), or by panicking or trapping; it returns other failures as they are.
func (r *offlineRunner) explain(ctx context.Context, timedOut bool, subject string, err error) error {
	var failure *PanicError

	switch {
	case err == nil:
		return nil
	case timedOut && ctx.Err() == nil:
		return fmt.Errorf("%s did not return within %s: %w", subject, r.timeout, err)
	case errors.As(err, &failure):
		return fmt.Errorf("%s panicked or trapped: %w", subject, err)
	default:
		return err
	}
}

// call calls meth with args in a fresh instance, through a pool serving that one instance, as a plugin's calls go
// through its pool: the instance is discarded when the call breaks it and closed (running the plugin's Close)
// otherwise.
func (r *offlineRunner) call(ctx context.Context, launch *offlineLaunch, meth *method, args []reflect.Value,
) offline.Outcome {
	inst, err := r.start(ctx, launch)
	if err != nil {
		return offline.Outcome{Err: err}
	}

	var taken atomic.Bool

	gen := newGeneration(launch.cand.launch.key, nil, &launch.src.desc, newPool(&poolConfig{
		panics: r.ctr.panics,
		spawn: func(context.Context) (instance, error) {
			if !taken.CompareAndSwap(false, true) {
				return nil, errOneInstance
			}

			return inst, nil
		},
		http: launch.cand.launch.opts.HTTP, budget: &r.ctr.budget, meters: launch.cand.meters,
		owner: launch.cand.launch.owner, key: launch.cand.launch.key, lang: launch.src.desc.Language,
		limit: launch.src.limit, maxSize: 1, closeTimeout: r.ctr.closeTimeout,
	}))
	gen.spans = launch.cand.launch.spans

	_ = gen.pool.warm(ctx) //nolint:errcheck // a pool of no minimum starts no instance: it cannot fail

	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	call := reflectCall{meth: meth, args: append([]reflect.Value{reflect.ValueOf(callCtx)}, args...)}
	err = gen.call(callCtx, meth, &call)
	err = r.explain(ctx, callCtx.Err() != nil, "it", err)

	cancel()

	closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), r.ctr.closeTimeout)
	defer cancelClose()

	err = errors.Join(err, gen.pool.close(closeCtx))
	if taken.CompareAndSwap(false, true) { // the call ended before it took the instance
		err = errors.Join(err, inst.Close(closeCtx))
	}

	outcome := offline.Outcome{Err: err, Returned: returned(err)}
	if err == nil {
		outcome.Results = call.out[:len(call.out)-1]
	}

	return outcome
}

// validate calls the plugin's Validate with input in a fresh instance, as a load validates a plugin version.
func (r *offlineRunner) validate(ctx context.Context, launch *offlineLaunch, input reflect.Value) offline.Outcome {
	val := launch.reg.validation

	args, err := val.codec.Append(nil, input)
	if err != nil {
		return offline.Outcome{Err: fmt.Errorf("%w: Validate's input: %w", ErrConversion, err)}
	}

	ctx = validating(ctx)

	inst, err := r.start(ctx, launch)
	if err != nil {
		return offline.Outcome{Err: err}
	}

	issues, discard, err := r.ctr.callValidate(ctx, val, launch.cand.launch, inst, args)

	closeTimeout := r.ctr.closeTimeout
	if discard { // whatever failed may leave Close unable to end
		closeTimeout = 0
	}

	r.ctr.closeValidation(ctx, launch.cand.launch, inst, closeTimeout)

	err = explainNoValidate(err, &launch.src, launch.cand.files, val)

	return offline.Outcome{Err: err, Returned: returned(err), Issues: newAdminIssues(issues)}
}

// returned is the message of the error the plugin returned that err wraps; empty when err wraps none.
func returned(err error) string {
	if failed, ok := errors.AsType[*PluginError](err); ok {
		return failed.Message
	}

	return ""
}
