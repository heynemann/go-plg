package goguest

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/traefik/yaegi/interp"

	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/wire"
)

// hostMethod is one method of the plugin's Host, as the guest calls it through [HostFunc]. The dispatcher's
// Host stubs (rendered by the host, see [goabi.RenderDispatcher]) forward each method to a variable of the
// method's type; bindHost sets it to a function built with reflect.MakeFunc, so interpreted code calls straight
// into compiled code with its own typed values: no symbol package, no boxing.
type hostMethod struct {
	name    string
	args    []*wire.Codec   // one per parameter after the context
	results []*wire.Codec   // one per result before the final error
	zeros   []reflect.Value // the results returned with an error: zero values, then a nil error
	index   uint32          // the method index of host_call
}

// bindHost fills the dispatcher's variable for the Host method with the given index.
func (g *Guest) bindHost(interpreter *interp.Interpreter, planner *wire.Planner, index int, name string) error {
	fn, err := interpreter.Eval(dispatchAlias + "." + goabi.HostCallPrefix + name)
	if err != nil {
		return fmt.Errorf("resolving host method %s: %w", name, err)
	}

	typ := fn.Type()
	if !methodShaped(typ) || !fn.CanSet() {
		return fmt.Errorf("%w: host method %s has type %s", errCallType, name, typ)
	}

	meth := &hostMethod{
		name:    name,
		index:   uint32(index), //nolint:gosec // G115: a method index is far below 2^32
		args:    make([]*wire.Codec, typ.NumIn()-1),
		results: make([]*wire.Codec, typ.NumOut()-1),
		zeros:   make([]reflect.Value, typ.NumOut()),
	}

	for k := range meth.args {
		meth.args[k], err = planner.For(typ.In(k + 1))
		if err != nil {
			return fmt.Errorf("host method %s parameter %d: %w", name, k+1, err)
		}
	}

	for k := range meth.results {
		meth.results[k], err = planner.For(typ.Out(k))
		if err != nil {
			return fmt.Errorf("host method %s result %d: %w", name, k, err)
		}
	}

	for k := range meth.zeros {
		meth.zeros[k] = reflect.Zero(typ.Out(k))
	}

	fn.Set(reflect.MakeFunc(typ, func(in []reflect.Value) []reflect.Value { return g.callHost(meth, in) }))

	return nil
}

// callHost encodes the arguments (after the context: the host passes the context of the plugin call in
// progress), calls the host and decodes its answer. An argument that cannot cross fails the call before it
// reaches the host. Every error the host reports, from the Host method or about it, reaches the plugin as an
// error with the host's message; a malformed answer is an ABI fault, which discards the instance once the
// plugin call returns.
func (g *Guest) callHost(meth *hostMethod, in []reflect.Value) []reflect.Value {
	var err error

	g.hostArgs = g.hostArgs[:0]
	for k, codec := range meth.args {
		g.hostArgs, err = codec.Append(g.hostArgs, in[k+1])
		if err != nil {
			return meth.failed(fmt.Errorf("plg: host method %s: parameter %d: %w", meth.name, k+1, err))
		}
	}

	g.hostAnswer = g.host(meth.index, g.hostArgs, g.hostAnswer[:0])

	out, err := meth.decodeAnswer(g.hostAnswer)
	if err != nil {
		err = fmt.Errorf("host method %s: %w: %w", meth.name, errAnswer, err)
		g.fault = errors.Join(g.fault, err)

		return meth.failed(err)
	}

	return out
}

// decodeAnswer decodes a status-prefixed answer into the method's results.
func (m *hostMethod) decodeAnswer(answer []byte) ([]reflect.Value, error) {
	if len(answer) == 0 {
		return nil, errEmptyAnswer
	}

	reader := wire.NewReader(answer[1:])

	switch status := wire.Status(answer[0]); status { //nolint:exhaustive // the host answers with these only
	case wire.StatusOK:
		out := make([]reflect.Value, len(m.zeros))
		for k, codec := range m.results {
			out[k] = reflect.New(codec.Type()).Elem()

			err := codec.Decode(&reader, out[k])
			if err != nil {
				return nil, fmt.Errorf("result %d: %w", k, err)
			}
		}

		out[len(out)-1] = m.zeros[len(out)-1]

		return out, reader.End() //nolint:wrapcheck // wrapped by callHost with the method name
	case wire.StatusError, wire.StatusPanic, wire.StatusConvert:
		msg, err := reader.Text()
		if err == nil {
			err = reader.End()
		}

		if err != nil {
			return nil, fmt.Errorf("%s message: %w", status, err)
		}

		return m.failed(errors.New(msg)), nil //nolint:err113 // only the host's message crosses the boundary
	default:
		return nil, fmt.Errorf("%w %d", errAnswerStatus, answer[0])
	}
}

var (
	errEmptyAnswer  = errors.New("empty answer")
	errAnswerStatus = errors.New("invalid status")
)

// failed returns the method's zero results with err.
func (m *hostMethod) failed(err error) []reflect.Value {
	out := make([]reflect.Value, len(m.zeros))
	copy(out, m.zeros)

	out[len(out)-1] = reflect.ValueOf(&err).Elem()

	return out
}

// takeFault returns and clears the ABI fault of the current call, or "" without one.
func (g *Guest) takeFault() string {
	if g.fault == nil {
		return ""
	}

	msg := g.fault.Error()
	g.fault = nil

	return msg
}
