package plg

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/offline"
)

// offlineName is the registration name plg validate checks plugins under: their keys are "validate/<key>".
const offlineName = "validate"

// offlineStorage is the storage name a plugin plg validate checks has in its report and errors.
const offlineStorage = "plg validate"

// errStubHost fails every call of a Host method under plg validate.
var errStubHost = errors.New("plg validate runs plugins without the service's Host")

//nolint:gochecknoinits // plg validate reaches plg's unexported loading code through offline.Check, set here
func init() {
	offline.Check = checkOffline
	offline.CheckHost = checkOfflineHost
}

// checkOfflineHost is [offline.CheckHost]: it checks spec as checkOffline checks the Host of a contract of pkgPath.
func checkOfflineHost(pkgPath string, spec *offline.Host) error {
	_, err := stubHost(pkgPath, spec.Interface.Type, spec)

	return err
}

// checkOffline is [offline.Check]: it prepares spec as Register prepares a registered contract, with a Host whose
// methods fail every call, and checks the plugin against it in a container of its own, as [Check] checks a
// candidate (no pool, one throwaway instance running Create).
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

	if opts.Limits.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, opts.Limits.Timeout)
		defer cancel()
	}

	report, err := ctr.checkAgainst(ctx, reg, offlineStorage, &desc, plugin.Files)

	return newAdminReport(&report), err
}

// offlineOptions are the Register options of a plg validate check: its storage, its limits (a pool of one instance
// by default) and its ownership rule.
func offlineOptions(opts *offline.Options) []RegisterOption {
	low, high := 1, 1
	if opts.Limits.PoolMax != 0 {
		low, high = opts.Limits.PoolMin, opts.Limits.PoolMax
	}

	options := []RegisterOption{WithStorage(NewMemoryStorage(offlineStorage)), WithPool(low, high)}

	if opts.Limits.Memory != 0 {
		options = append(options, WithMemoryLimit(opts.Limits.Memory))
	}

	if opts.RequireOwnership {
		options = append(options, WithRequiredOwnership())
	}

	return options
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

// stubHost returns the Host of spec, checked as [WithHost] checks a Host, whose methods fail every call with
// errStubHost; nil when spec is nil. The Host must be declared in the package pkgPath of the interface owner.
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

	funcs := make([]reflect.Value, len(spec.Methods))
	for i := range funcs {
		funcs[i] = failingFunc(named.String()+"."+spec.Methods[i].Name, spec.Methods[i].Type)
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
