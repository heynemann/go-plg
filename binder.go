package plg

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/wire"
)

// method is the call plan of one interface method, built once per contract: the codecs of its arguments
// (after the context) and of its results (before the error). Calls never plan a type again.
type method struct {
	typ     reflect.Type
	name    string
	args    []*wire.Codec
	results []*wire.Codec
	zeros   []reflect.Value // the results returned with an error: zero values, then a nil error
	field   int             // index of the adapter's W<name> field
	index   uint32          // the method index of plg_call
}

// newMethod plans the method with the given index; planner is shared by the contract's methods, so each type
// of the contract is planned once.
func newMethod(planner *wire.Planner, iface reflect.Type, index, field int) (*method, error) {
	sig := iface.Method(index)
	fn := sig.Type
	plan := &method{
		typ:     fn,
		name:    sig.Name,
		field:   field,
		index:   uint32(index), //nolint:gosec // G115: a method index is far below 2^32
		args:    make([]*wire.Codec, 0, fn.NumIn()-1),
		results: make([]*wire.Codec, 0, fn.NumOut()-1),
	}

	for k := 1; k < fn.NumIn(); k++ {
		codec, err := planner.For(fn.In(k))
		if err != nil {
			return nil, fmt.Errorf("method %s: parameter %d: %w", sig.Name, k, err)
		}

		plan.args = append(plan.args, codec)
	}

	for k := range fn.NumOut() - 1 {
		codec, err := planner.For(fn.Out(k))
		if err != nil {
			return nil, fmt.Errorf("method %s: result %d: %w", sig.Name, k, err)
		}

		plan.results = append(plan.results, codec)
	}

	for typ := range fn.Outs() {
		plan.zeros = append(plan.zeros, reflect.Zero(typ))
	}

	return plan, nil
}

// encodeArgs appends the encoding of the arguments after the context.
func (m *method) encodeArgs(dst []byte, args []reflect.Value) ([]byte, error) {
	for k, codec := range m.args {
		next, err := codec.Append(dst, args[k+1])
		if err != nil {
			return dst, fmt.Errorf("parameter %d: %w", k+1, err)
		}

		dst = next
	}

	return dst, nil
}

// decodeResults decodes body into the method's results, ending with a nil error.
func (m *method) decodeResults(body []byte) ([]reflect.Value, error) {
	out := make([]reflect.Value, len(m.zeros))
	reader := wire.NewReader(body)

	for k, codec := range m.results {
		out[k] = reflect.New(codec.Type()).Elem()

		err := codec.Decode(&reader, out[k])
		if err != nil {
			return nil, fmt.Errorf("result %d: %w", k, err)
		}
	}

	err := reader.End()
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}

	out[len(out)-1] = m.zeros[len(out)-1]

	return out, nil
}

// decodeArgs decodes the arguments after the context from body, returning the call's arguments with ctx first.
func (m *method) decodeArgs(ctx context.Context, body []byte) ([]reflect.Value, error) {
	args := make([]reflect.Value, len(m.args)+1)
	args[0] = reflect.ValueOf(ctx)
	reader := wire.NewReader(body)

	for k, codec := range m.args {
		args[k+1] = reflect.New(codec.Type()).Elem()

		err := codec.Decode(&reader, args[k+1])
		if err != nil {
			return nil, fmt.Errorf("parameter %d: %w", k+1, err)
		}
	}

	err := reader.End()
	if err != nil {
		return nil, fmt.Errorf("arguments: %w", err)
	}

	return args, nil
}

// encodeResults appends the encoding of the results before the final error.
func (m *method) encodeResults(dst []byte, out []reflect.Value) ([]byte, error) {
	for k, codec := range m.results {
		next, err := codec.Append(dst, out[k])
		if err != nil {
			return dst, fmt.Errorf("result %d: %w", k, err)
		}

		dst = next
	}

	return dst, nil
}

// failed returns the method's zero results with err.
func (m *method) failed(err error) []reflect.Value {
	out := make([]reflect.Value, len(m.zeros))
	copy(out, m.zeros)

	out[len(out)-1] = reflect.ValueOf(&err).Elem()

	return out
}

var errNilContext = errors.New("nil context")

// bind returns a new adapter for the plugin: a pointer to the contract's adapter struct whose W<Method> fields
// call the plugin, so it implements the contract's interface.
func (ct *contract) bind(plug *plugin) any {
	adapter := reflect.New(ct.adapter)
	fields := adapter.Elem()

	for _, meth := range ct.methods {
		fields.Field(meth.field).Set(reflect.MakeFunc(meth.typ, func(args []reflect.Value) []reflect.Value {
			out, err := plug.call(meth, args)
			if err != nil {
				return meth.failed(err)
			}

			return out
		}))
	}

	return adapter.Interface()
}

// call runs one method call on an instance of the plugin, waiting for one while all are busy. Its context is
// the call's first argument.
func (p *plugin) call(meth *method, args []reflect.Value) ([]reflect.Value, error) {
	ctx, ok := reflect.TypeAssert[context.Context](args[0])
	if !ok {
		return nil, fmt.Errorf("plg: %s.%s: %w", p.key, meth.name, errNilContext)
	}

	held, err := p.pool.acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("plg: %s.%s: %w", p.key, meth.name, err)
	}

	out, discard, err := p.exchange(ctx, held, meth, args)
	if discard || held.inst.Broken() { // never reuse a broken instance: wazero would answer with garbage
		p.pool.discard(ctx, held)
	} else {
		p.pool.release(held)
	}

	return out, err
}

// exchange encodes the arguments, calls the instance and decodes its answer. The boolean reports an instance
// that must not serve another call. A guest that traps, panics, is stopped by ctx or breaks the ABI, and a
// panic on the host meanwhile, fail the call with a reported [PanicError] in [PhaseCall].
func (p *plugin) exchange(ctx context.Context, held *slot, meth *method, args []reflect.Value) (
	[]reflect.Value, bool, error,
) {
	var (
		out     []reflect.Value
		discard bool
		err     error
	)

	func() {
		defer func() {
			if v := recover(); v != nil {
				failure := &PanicError{Key: p.key, Language: p.lang, Phase: PhaseCall, Method: meth.name}
				p.pool.panics.recovered(ctx, failure, v)

				out, discard, err = nil, true, failure
			}
		}()

		out, discard, err = p.roundTrip(ctx, held, meth, args)
	}()

	return out, discard, err
}

// roundTrip is exchange without its panic boundary.
func (p *plugin) roundTrip(ctx context.Context, held *slot, meth *method, args []reflect.Value) (
	[]reflect.Value, bool, error,
) {
	var err error

	held.buf, err = meth.encodeArgs(held.buf[:0], args)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %s.%s: %w", ErrConversion, p.key, meth.name, err)
	}

	res, err := held.inst.Call(ctx, meth.index, held.buf)
	if err != nil {
		return nil, true, p.failed(ctx, held, meth, err)
	}

	switch res.Status { //nolint:exhaustive // load and ABI statuses (or worse) fail the call and break the instance
	case wire.StatusOK:
		out, err := meth.decodeResults(res.Body)
		if err != nil {
			return nil, true, p.failed(ctx, held, meth, fmt.Errorf("%w: %w", engine.ErrABI, err))
		}

		return out, false, nil
	case wire.StatusError:
		return nil, false, &PluginError{Key: p.key, Method: meth.name, Message: res.Message}
	case wire.StatusConvert:
		return nil, false, fmt.Errorf("%w: %s.%s: %s", ErrConversion, p.key, meth.name, res.Message)
	case wire.StatusPanic:
		return nil, true, p.failed(ctx, held, meth, res.Message)
	default:
		return nil, true, p.failed(ctx, held, meth, fmt.Errorf("%w %s: %s", errGuestStatus, res.Status, res.Message))
	}
}

// failed reports a call the guest failed, with what the guest wrote to stderr meanwhile.
func (p *plugin) failed(ctx context.Context, held *slot, meth *method, value any) *PanicError {
	failure := &PanicError{
		Key: p.key, Language: p.lang, Phase: PhaseCall, Method: meth.name, Value: value, Guest: held.inst.Guest(),
	}
	p.pool.panics.report(ctx, failure)

	return failure
}
