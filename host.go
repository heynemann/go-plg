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
// methods' code: arguments after the context, results before the error) and impl's bound methods. plg validate's
// Host is a stub (see offline.go) whose methods fail every call.
type host struct {
	described *goabi.Interface // the Host as the Go dispatcher writes it
	name      string           // the Host interface as reflect.Type.String writes it: "pkg.Name"
	pkgPath   string           // the Host interface's package
	methods   []*method
	funcs     []reflect.Value // impl's method values, in the Host's method order
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

	if spec.iface.PkgPath() != contr.pkgPath {
		return nil, fmt.Errorf("%w %s %w %s of %s, whose source Go plugins interpret; it is in %s",
			ErrInvalidHost, spec.iface, errHostPackage, contr.pkgPath, contr.described.Type, spec.iface.PkgPath())
	}

	return prepared, nil
}

// planHost builds the call plans of the Host's methods and binds impl's methods.
func planHost(spec hostSpec) (*host, error) {
	described, err := goabi.InterfaceOf(spec.iface)
	if err != nil {
		return nil, fmt.Errorf("rendering the Go dispatcher: %w", err)
	}

	sigs := make([]reflect.Method, spec.iface.NumMethod())
	funcs := make([]reflect.Value, len(sigs))

	for i := range sigs {
		sigs[i], funcs[i] = spec.iface.Method(i), spec.impl.Method(i)
	}

	return plannedHost(described, spec.iface.PkgPath(), sigs, funcs)
}

// plannedHost builds the call plans of the methods sigs of the Host described, whose package is pkgPath, served
// by funcs (one per method, of the method's type).
func plannedHost(described *goabi.Interface, pkgPath string, sigs []reflect.Method, funcs []reflect.Value) (
	*host, error,
) {
	prepared := &host{
		described: described, name: described.Type.String(), pkgPath: pkgPath,
		methods: make([]*method, len(sigs)), funcs: funcs,
	}

	var planner wire.Planner // shared by the methods, so each type is planned once

	for i := range prepared.methods {
		meth, err := newMethod(&planner, &sigs[i], i, -1)
		if err != nil {
			return nil, err
		}

		prepared.methods[i] = meth
	}

	return prepared, nil
}

var (
	errHostMethod = errors.New("unknown host method")
	errHostArgs   = errors.New("malformed host call arguments")
)

// handler returns the function serving host_call for the instances of the plugin version id, reporting panics to
// panics and tracing each Host method call in spans.
func (h *host) handler(id pluginID, panics *panicReporter, spans *pluginSpans) engine.HostFunc {
	if h == nil {
		return nil
	}

	caller := &hostCaller{host: h, panics: panics, spans: spans, id: id}

	return caller.serve
}

// hostCaller serves one plugin's host calls.
type hostCaller struct {
	host   *host
	panics *panicReporter
	spans  *pluginSpans
	id     pluginID
}

// serve decodes a host call's arguments, calls the Host method and appends its status-prefixed answer to dst:
// the encoded results, the message of the error it returned, of its results' conversion failure or of its
// panic. A panic (in the method, or in plg decoding or encoding its values) is reported as a [PanicError] in
// [PhaseHost], whose message the plugin receives as the method's error; the instance keeps serving. The call has
// its span, a child of the plugin call's, which the Host method's context carries.
func (hc *hostCaller) serve(ctx context.Context, dst []byte, index uint32, raw []byte) ([]byte, error) {
	hst := hc.host
	if int(index) >= len(hst.methods) {
		return dst, fmt.Errorf("%w %d of %s", errHostMethod, index, hst.name)
	}

	meth := hst.methods[index]
	head := len(dst)
	ctx, span := hc.spans.host(ctx, index)

	var (
		answer []byte
		err    error
	)

	func() {
		defer func() {
			if v := recover(); v != nil { // reported here, so the stack is the panicking one
				failure := &PanicError{
					Key: hc.id.key, Language: hc.id.lang, Phase: PhaseHost, Method: hst.name + "." + meth.name,
					Owner: hc.id.owner.owner, tags: hc.id.owner,
				}
				hc.panics.recovered(ctx, failure, v)

				answer, err = wire.AppendMessage(append(dst, byte(wire.StatusPanic)), failure.Error()), nil
			}
		}()

		answer, err = hst.answer(ctx, dst, meth, raw)
	}()

	hc.spans.hostAnswered(ctx, span, answer[head:], err)

	return answer, err
}

// answer decodes a host call's arguments and invokes its method.
func (h *host) answer(ctx context.Context, dst []byte, meth *method, raw []byte) ([]byte, error) {
	args, err := meth.decodeArgs(ctx, raw)
	if err != nil {
		return dst, fmt.Errorf("%w: %s.%s: %w", errHostArgs, h.name, meth.name, err)
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
		msg := fmt.Sprintf("%v: host method %s.%s: %v", ErrConversion, h.name, meth.name, err)

		return wire.AppendMessage(append(dst, byte(wire.StatusConvert)), msg)
	}

	return answer
}
