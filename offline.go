package plg

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"

	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/directive"
	"github.com/heynemann/go-plg/internal/offline"
)

// offlineName is the registration name plg validate checks plugins under: their keys are "validate/<key>".
const offlineName = "validate"

// offlineStorage is the storage name a plugin plg validate checks has in its report and errors.
const offlineStorage = "plg validate"

// errStubHost fails every call of a Host method under plg validate.
var errStubHost = errors.New("plg validate runs plugins without the service's Host")

// errStubSecrets fails every secret read under plg validate outside a test case.
var errStubSecrets = errors.New("plg validate runs plugins without the service's secrets; test cases fake them " +
	"(secrets)")

//nolint:gochecknoinits // plg validate reaches plg's unexported loading code through offline.Check and offline.Open
func init() {
	offline.Check = checkOffline
	offline.CheckHost = checkOfflineHost
	offline.Defaults = offlineDefaults
	offline.Open = openOffline
	offline.CheckContract = checkOfflineContract
	offline.Ownership = offlineOwnership
	offline.LuaSyntax = checkLuaSyntax
}

// offlineOwnership is [offline.Ownership]: the missing-ownership issue a load adds to the manifest's report, an
// error when required.
//
//nolint:revive // flag-parameter: required is the repository's requireOwnership setting, not a control flag
func offlineOwnership(manifest []byte, required bool) ([]adminjson.Issue, error) {
	desc, err := ParseManifest(manifest)
	if err != nil {
		return nil, err
	}

	severity := SeverityWarning
	if required {
		severity = SeverityError
	}

	var report Report

	checkOwnership(&report, desc.owner(), severity)

	return newAdminIssues(report.Issues), nil
}

// checkOfflineContract is [offline.CheckContract]: it checks spec's interface and Host as checkOffline does.
func checkOfflineContract(spec *offline.Contract) error {
	contr, err := offlineContract(spec)
	if err != nil {
		return err
	}

	_, err = stubHost(contr.pkgPath, contr.described.Type, spec.Host)

	return err
}

// checkOfflineHost is [offline.CheckHost]: it checks spec as checkOffline checks the Host of a contract of pkgPath.
func checkOfflineHost(pkgPath string, spec *offline.Host) error {
	_, err := stubHost(pkgPath, spec.Interface.Type, spec)

	return err
}

// offlineDefaults is [offline.Defaults]: a Register's defaults without WithMemoryLimit and WithPool.
func offlineDefaults(language string) directive.Limits {
	limits := directive.Limits{PoolMin: 1, PoolMax: min(runtime.GOMAXPROCS(0), maxPoolSize)}

	ld, err := loaderFor(Language(language))
	if err == nil {
		limits.Memory = ld.memoryLimit()
	}

	return limits
}

// checkOffline is [offline.Check]: it prepares spec as Register prepares a registered contract, with a Host whose
// methods fail every call, and checks the plugin against it in a container of its own, as [Check] checks a
// candidate (no pool, one throwaway instance running Create), under the limits opts sets and in its Go guest.
func checkOffline(ctx context.Context, spec *offline.Contract, plugin *offline.Plugin, opts *offline.Options,
) (_ *adminjson.Report, err error) {
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

	cfg, err := registerOptions(offlineName, offlineOptions(opts))
	if err != nil {
		return nil, err
	}

	cfg.createTimeout = opts.Limits.Timeout

	//nolint:contextcheck // New takes no context: what the container starts ends with its Close, below
	ctr := New(WithLogger(opts.Logger), WithCompilationCacheDir(opts.CacheDir))

	defer func() {
		err = errors.Join(err, ctr.Close(context.WithoutCancel(ctx)))
	}()

	reg, err := ctr.newRegistration(contr, cfg, offlineName, hst)
	if err != nil {
		return nil, err
	}

	defer reg.http.Close()

	report, err := ctr.checkAgainst(ctx, reg, offlineStorage, &desc, plugin.Files)

	return newAdminReport(&report), err
}

// offlineOptions are the Register options of a plg validate check: its storage, its limits (a pool of one instance
// by default: the check runs one), its ownership rule, its Go guest and its secrets.
func offlineOptions(opts *offline.Options) []RegisterOption {
	low, high := 1, 1
	if opts.Limits.PoolMax != 0 {
		low, high = opts.Limits.PoolMin, opts.Limits.PoolMax
	}

	options := []RegisterOption{
		WithStorage(NewMemoryStorage(offlineStorage)), WithPool(low, high), WithSecrets(offlineSecrets(opts.Secrets)),
	}

	if opts.Limits.Memory != 0 {
		options = append(options, WithMemoryLimit(opts.Limits.Memory))
	}

	if opts.RequireOwnership {
		options = append(options, WithRequiredOwnership())
	}

	if opts.GoGuest != nil {
		options = append(options, WithGoGuest(opts.GoGuest))
	}

	return options
}

// offlineSecrets returns the WithSecrets resolver answering reads with read, or failing them all without it.
func offlineSecrets(read func(ctx context.Context, name string) (string, error)) secretResolver {
	if read == nil {
		return func(context.Context, PluginInfo, string) (string, error) { return "", errStubSecrets }
	}

	return func(ctx context.Context, _ PluginInfo, name string) (string, error) { return read(ctx, name) }
}

// offlineContract prepares the contract plg validate described, checking its methods as Register checks an
// interface's.
func offlineContract(spec *offline.Contract) (*contract, error) {
	named := spec.Interface.Type

	err := checkMethods(spec.Methods)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrInvalidInterface, named, err)
	}

	contr := &contract{described: spec.Interface, module: spec.Module, pkgPath: named.Path, name: named.Name}

	err = contr.prepare(spec.Source, spec.Methods, nil)
	if err != nil {
		return nil, fmt.Errorf("%w for %s: %w", ErrInvalidContract, named, err)
	}

	return contr, nil
}

// stubHost returns the Host of spec, checked as [WithHost] checks a Host, whose methods are spec's Funcs, or fail
// every call with errStubHost without them; nil when spec is nil. The Host must be declared in the package pkgPath
// of the interface owner.
func stubHost(pkgPath string, owner fmt.Stringer, spec *offline.Host) (*host, error) {
	if spec == nil {
		return nil, nil //nolint:nilnil // the plugin's Create takes no Host, which is not an error
	}

	named := spec.Interface.Type

	err := checkMethods(spec.Methods)
	if err != nil {
		return nil, fmt.Errorf("%w: %w %s: %w", ErrInvalidHost, ErrInvalidInterface, named, err)
	}

	if named.Path != pkgPath {
		return nil, fmt.Errorf("%w %s %w %s of %s, whose source Go plugins interpret; it is in %s",
			ErrInvalidHost, named, errHostPackage, pkgPath, owner, named.Path)
	}

	funcs := spec.Funcs
	if funcs == nil {
		funcs = make([]reflect.Value, len(spec.Methods))
		for i := range funcs {
			funcs[i] = failingFunc(named.String()+"."+spec.Methods[i].Name, spec.Methods[i].Type)
		}
	}

	hst, err := plannedHost(spec.Interface, named.Path, spec.Methods, funcs)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrInvalidHost, named, err)
	}

	return hst, nil
}

// failingFunc returns a func of type fn, a Host method's, that returns zero values and an error naming method.
func failingFunc(method string, fn reflect.Type) reflect.Value {
	failure := fmt.Errorf("%w: %s was called", errStubHost, method)

	return reflect.MakeFunc(fn, func([]reflect.Value) []reflect.Value {
		out := make([]reflect.Value, fn.NumOut())
		for k := range out {
			out[k] = reflect.Zero(fn.Out(k))
		}

		out[len(out)-1] = reflect.ValueOf(&failure).Elem()

		return out
	})
}
