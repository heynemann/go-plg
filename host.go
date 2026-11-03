package plg

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/wire"
)

// WithHost lets the registered plugins call back into the service through impl, a value of the Host interface H.
// H follows the interface contract (every method takes a context.Context first, returns an error last and uses
// only data types) and is declared in the package of the registered interface, whose source Go plugins
// interpret. A Go plugin's Create then takes the Host, func Create(host contract.Host) (contract.Fooer, error);
// without WithHost it takes nothing.
//
// The host calls impl's methods by reflection, with the context of the plugin call in progress (Register's
// context while Create runs). An error impl returns reaches the plugin as an error with the same message; a
// panic in impl is recovered and reaches the plugin as an error too, leaving the service and the plugin's
// instance running. impl is called concurrently by the plugin's instances, so it must be safe for concurrent
// use; while impl runs, the instance calling it is busy, so impl must not wait on a call to the same plugin.
//
// Register fails with [ErrInvalidHost] if H breaks the interface contract (also [ErrInvalidInterface]), is
// declared in another package, impl is nil or WithHost is given more than once.
func WithHost[H any](impl H) RegisterOption {
	spec := hostSpec{iface: reflect.TypeFor[H](), impl: reflect.ValueOf(&impl).Elem()}

	return func(c *registerConfig) {
		c.hosts = append(c.hosts, spec)
	}
}

// hostSpec is a WithHost option's interface type and implementation.
type hostSpec struct {
	iface reflect.Type
	impl  reflect.Value // of kind Interface when iface is an interface, so Method(i) follows iface's order
}

// host is a Register call's Host, prepared once: the call plans of its methods (shared with the plugin
// methods' code: arguments after the context, results before the error), impl's bound methods, and the Go
// dispatcher and init payload rendered for the contract with this Host.
type host struct {
	iface      reflect.Type
	methods    []*method
	funcs      []reflect.Value // impl's method values, in iface's method order
	dispatcher []byte
	payload    []byte
}

var (
	errHostTwice   = errors.New("WithHost given more than once")
	errHostNil     = errors.New("nil implementation")
	errHostPackage = errors.New("must be declared in the contract package")
)

// newHost checks the Register call's WithHost options and prepares its Host; it returns nil without one.
func newHost(contr *contract, specs []hostSpec) (*host, error) {
	switch len(specs) {
	case 0:
		return nil, nil //nolint:nilnil // no WithHost: the plugins have no Host, which is not an error
	case 1:
	default:
		return nil, fmt.Errorf("%w: %w", ErrInvalidHost, errHostTwice)
	}

	spec := specs[0]

	err := validateInterface(spec.iface)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidHost, err)
	}

	if spec.impl.IsNil() {
		return nil, fmt.Errorf("%w %s: %w", ErrInvalidHost, spec.iface, errHostNil)
	}

	prepared, err := planHost(spec)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrInvalidHost, spec.iface, err)
	}

	if spec.iface.PkgPath() != contr.iface.PkgPath() {
		return nil, fmt.Errorf("%w %s %w %s of %s, whose source Go plugins interpret; it is in %s",
			ErrInvalidHost, spec.iface, errHostPackage, contr.iface.PkgPath(), contr.iface, spec.iface.PkgPath())
	}

	err = prepared.renderGo(contr)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrInvalidHost, spec.iface, err)
	}

	return prepared, nil
}

// planHost builds the call plans of the Host's methods and binds impl's methods.
func planHost(spec hostSpec) (*host, error) {
	prepared := &host{
		iface:   spec.iface,
		methods: make([]*method, spec.iface.NumMethod()),
		funcs:   make([]reflect.Value, spec.iface.NumMethod()),
	}

	var planner wire.Planner // shared by the methods, so each type is planned once

	for i := range prepared.methods {
		meth, err := newMethod(&planner, spec.iface, i, -1)
		if err != nil {
			return nil, err
		}

		prepared.methods[i], prepared.funcs[i] = meth, spec.impl.Method(i)
	}

	return prepared, nil
}

// renderGo renders the dispatcher of the contract's Go plugins with this Host's stubs, and their init payload.
func (h *host) renderGo(contr *contract) error {
	var err error

	h.dispatcher, err = goabi.RenderDispatcher(contr.iface, h.iface)
	if err != nil {
		return fmt.Errorf("rendering the Go dispatcher: %w", err)
	}

	payload := goabi.InitPayload{
		GoPath: goabi.GoPath, Dispatch: goabi.DispatchImportPath, Host: h.iface.String(), HostPath: h.iface.PkgPath(),
	}
	for _, meth := range contr.methods {
		payload.Methods = append(payload.Methods, meth.name)
	}

	for _, meth := range h.methods {
		payload.HostMethods = append(payload.HostMethods, meth.name)
	}

	h.payload, err = payload.Append(nil)
	if err != nil {
		return fmt.Errorf("encoding the Go init payload: %w", err)
	}

	return nil
}

var (
	errHostMethod = errors.New("unknown host method")
	errHostArgs   = errors.New("malformed host call arguments")
)

// handler returns the function serving host_call for the instances of the plugin with the given key and
// language, reporting panics to panics.
func (h *host) handler(key string, lang Language, panics *panicReporter) engine.HostFunc {
	if h == nil {
		return nil
	}

	caller := &hostCaller{host: h, panics: panics, key: key, lang: lang}

	return caller.serve
}

// hostCaller serves one plugin's host calls.
type hostCaller struct {
	host   *host
	panics *panicReporter
	key    string
	lang   Language
}

// serve decodes a host call's arguments, calls the Host method and appends its status-prefixed answer to dst:
// the encoded results, the message of the error it returned, of its results' conversion failure or of its
// panic. A panic (in the method, or in plg decoding or encoding its values) is reported as a [PanicError] in
// [PhaseHost], whose message the plugin receives as the method's error; the instance keeps serving.
func (hc *hostCaller) serve(ctx context.Context, dst []byte, index uint32, raw []byte) ([]byte, error) {
	hst := hc.host
	if int(index) >= len(hst.methods) {
		return dst, fmt.Errorf("%w %d of %s", errHostMethod, index, hst.iface)
	}

	meth := hst.methods[index]

	var (
		answer []byte
		err    error
	)

	func() {
		defer func() {
			if v := recover(); v != nil { // reported here, so the stack is the panicking one
				failure := &PanicError{
					Key: hc.key, Language: hc.lang, Phase: PhaseHost, Method: hst.iface.String() + "." + meth.name,
				}
				hc.panics.recovered(ctx, failure, v)

				answer, err = wire.AppendMessage(append(dst, byte(wire.StatusPanic)), failure.Error()), nil
			}
		}()

		answer, err = hst.answer(ctx, dst, meth, raw)
	}()

	return answer, err
}

// answer decodes a host call's arguments and invokes its method.
func (h *host) answer(ctx context.Context, dst []byte, meth *method, raw []byte) ([]byte, error) {
	args, err := meth.decodeArgs(ctx, raw)
	if err != nil {
		return dst, fmt.Errorf("%w: %s.%s: %w", errHostArgs, h.iface, meth.name, err)
	}

	return h.invoke(dst, meth, args), nil
}

// invoke calls the Host method and appends its answer to dst, as serve does, except for panics.
func (h *host) invoke(dst []byte, meth *method, args []reflect.Value) []byte {
	fn := h.funcs[meth.index]

	var out []reflect.Value

	if meth.typ.IsVariadic() {
		out = fn.CallSlice(args)
	} else {
		out = fn.Call(args)
	}

	if err, failed := reflect.TypeAssert[error](out[len(out)-1]); failed {
		return wire.AppendMessage(append(dst, byte(wire.StatusError)), err.Error())
	}

	answer, err := meth.encodeResults(append(dst, byte(wire.StatusOK)), out)
	if err != nil {
		msg := fmt.Sprintf("%v: host method %s.%s: %v", ErrConversion, h.iface, meth.name, err)

		return wire.AppendMessage(append(dst, byte(wire.StatusConvert)), msg)
	}

	return answer
}
