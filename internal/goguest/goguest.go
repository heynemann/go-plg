// Package goguest is the generic Go guest: it interprets a plugin package's source with yaegi and serves the
// plg guest ABI. It is platform independent, so it runs (and is tested) natively as well as inside the wasip1
// artifact, where guestsrc/golang only adapts pointers to these methods.
//
// A Guest is single-threaded, like the wasm instance that owns it.
package goguest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"

	"github.com/traefik/yaegi/interp"

	"github.com/heynemann/go-plg/guestsrc/golang/symbols"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/wire"
)

// The identifiers the guest imports the plugin and dispatcher packages under while loading.
const (
	pluginAlias   = "plgplugin"
	dispatchAlias = "plgdispatch"
)

var (
	errNoCreate   = errors.New("plugin package must declare Create")
	errCreateType = errors.New("plugin Create has the wrong type")
	errCallType   = errors.New("dispatcher function has the wrong type; want func(context.Context, ...) (..., error)")
	errAnswer     = errors.New("malformed host answer")
)

// HostFunc sends the encoded arguments of a call to the Host method with the given index (the order of
// [goabi.InitPayload.HostMethods]) to the host and returns the host's status-prefixed answer: the encoded
// results for [wire.StatusOK], otherwise a message. The answer may reuse dst's storage.
type HostFunc func(method uint32, args, dst []byte) []byte

// Guest serves one plugin instance. The zero value is not usable; call [New].
type Guest struct {
	stdout, stderr io.Writer
	host           HostFunc
	fault          error // a host answer broke the ABI during the current call; the instance must be discarded

	in         []byte // input buffer handed out by Alloc; the host writes requests into it
	out        []byte // result buffer; valid until the next call
	hostArgs   []byte // encoded arguments of the current host call
	hostAnswer []byte // the host's answer to it
	methods    []method
	started    bool // Init ran; it runs at most once
	ready      bool // Init succeeded
	closed     bool
}

type method struct {
	fn       reflect.Value
	name     string
	args     []reflect.Value // reused call arguments; args[0] is the context
	argDec   []*wire.Codec   // one per args[1:]
	results  []*wire.Codec   // one per result except the final error
	variadic bool            // the last argument is the variadic slice
}

// New returns a Guest whose plugin writes to stdout and stderr and calls its Host, if it has one, through host.
func New(stdout, stderr io.Writer, host HostFunc) *Guest {
	return &Guest{stdout: stdout, stderr: stderr, host: host}
}

// Alloc returns the input buffer, grown to at least size bytes. The host writes the next request into it.
func (g *Guest) Alloc(size uint32) []byte {
	if cap(g.in) < int(size) {
		g.in = make([]byte, size)
	}

	g.in = g.in[:size]

	return g.in
}

// Init loads the plugin described by the init payload in the first size bytes of the input buffer and runs
// its Create. It returns a status-prefixed result buffer.
func (g *Guest) Init(size uint32) []byte {
	return g.guarded(func() []byte { return g.initialize(size) })
}

// Call invokes method index with the arguments encoded in the first size bytes of the input buffer. It returns
// a status-prefixed result buffer.
func (g *Guest) Call(index, size uint32) []byte {
	return g.guarded(func() []byte { return g.invoke(index, size) })
}

// Close ends the instance; later calls are rejected.
func (g *Guest) Close() []byte {
	g.closed = true

	return g.ok()
}

// guarded runs fn, turning a panic in the plugin (or the interpreter) into a StatusPanic result.
func (g *Guest) guarded(fn func() []byte) []byte {
	var res []byte

	g.fault = nil // a panic may have left the previous call's

	func() {
		defer func() {
			if v := recover(); v != nil {
				res = g.fail(wire.StatusPanic, fmt.Sprint(v))
			}
		}()

		res = fn()
	}()

	return res
}

// input returns the first size bytes of the input buffer, or false if the host claims more than it allocated.
func (g *Guest) input(size uint32) ([]byte, bool) {
	if int(size) > len(g.in) {
		return nil, false
	}

	return g.in[:size], true
}

func (g *Guest) initialize(size uint32) []byte {
	payload, ok := g.input(size)
	if !ok {
		return g.fail(wire.StatusABI, "init payload exceeds the input buffer")
	}

	if g.started || g.closed {
		return g.fail(wire.StatusABI, "guest already initialized")
	}

	g.started = true

	decoded, err := goabi.DecodeInitPayload(payload)
	if err != nil {
		return g.fail(wire.StatusABI, err.Error())
	}

	initFn, status, err := g.load(&decoded)
	if err != nil {
		return g.fail(status, err.Error())
	}

	out := initFn.Call(nil)

	if fault := g.takeFault(); fault != "" {
		return g.fail(wire.StatusABI, fault)
	}

	if createErr, failed := reflect.TypeAssert[error](out[1]); failed {
		return g.fail(wire.StatusError, createErr.Error())
	}

	if problem := out[0].String(); problem != "" {
		return g.fail(wire.StatusLoad, "plugin "+problem)
	}

	g.ready = true

	return g.ok()
}

func (g *Guest) invoke(index, size uint32) []byte {
	payload, ok := g.input(size)
	if !ok {
		return g.fail(wire.StatusABI, "call arguments exceed the input buffer")
	}

	if !g.ready || g.closed {
		return g.fail(wire.StatusABI, "guest not initialized")
	}

	if int(index) >= len(g.methods) {
		return g.fail(wire.StatusABI, "unknown method index "+strconv.FormatUint(uint64(index), 10))
	}

	meth := &g.methods[index]

	err := meth.decodeArgs(payload)
	if err != nil {
		return g.fail(wire.StatusABI, meth.name+": "+err.Error())
	}

	out := meth.call()

	if fault := g.takeFault(); fault != "" {
		return g.fail(wire.StatusABI, fault)
	}

	if callErr, failed := reflect.TypeAssert[error](out[len(out)-1]); failed {
		return g.fail(wire.StatusError, callErr.Error())
	}

	g.out = append(g.out[:0], byte(wire.StatusOK))

	g.out, err = meth.appendResults(g.out, out)
	if err != nil { // the plugin returned what cannot cross, such as an integer beyond ±2^53; the host names the method
		return g.fail(wire.StatusConvert, err.Error())
	}

	return g.out
}

// load interprets the plugin and the dispatcher and resolves the dispatcher's functions.
func (g *Guest) load(payload *goabi.InitPayload) (reflect.Value, wire.Status, error) {
	interpreter := interp.New(interp.Options{
		GoPath:               payload.GoPath,
		SourcecodeFilesystem: sourceFS{},
		Args:                 []string{"plugin"},
		Stdout:               g.stdout,
		Stderr:               g.stderr,
	})

	err := interpreter.Use(symbols.Symbols)
	if err != nil {
		return reflect.Value{}, wire.StatusLoad, fmt.Errorf("loading the standard library: %w", err)
	}

	err = checkCreate(interpreter, payload)
	if err != nil {
		return reflect.Value{}, wire.StatusLoad, err
	}

	_, err = interpreter.Eval(`import ` + dispatchAlias + ` "` + payload.Dispatch + `"`)
	if err != nil {
		return reflect.Value{}, wire.StatusLoad, fmt.Errorf("loading dispatcher: %w", err)
	}

	initFn, err := interpreter.Eval(dispatchAlias + "." + goabi.InitFunc)
	if err != nil {
		return reflect.Value{}, wire.StatusABI, fmt.Errorf("dispatcher: %w", err)
	}

	var planner wire.Planner // shared by the methods, so each type is planned once

	for i, name := range payload.HostMethods {
		err = g.bindHost(interpreter, &planner, i, name)
		if err != nil {
			return reflect.Value{}, wire.StatusABI, fmt.Errorf("dispatcher: %w", err)
		}
	}

	g.methods = make([]method, len(payload.Methods))
	for i, name := range payload.Methods {
		err = g.methods[i].resolve(interpreter, &planner, name)
		if err != nil {
			return reflect.Value{}, wire.StatusABI, fmt.Errorf("dispatcher: %w", err)
		}
	}

	return initFn, 0, nil
}

// checkCreate imports the plugin package and checks that it declares func Create() (T, error), or
// func Create(H) (T, error) when the plugins have the Host interface H. yaegi checks the exact types when it
// compiles the dispatcher; this check gives the common mistakes a clear message.
func checkCreate(interpreter *interp.Interpreter, payload *goabi.InitPayload) error {
	host := payload.Host

	_, err := interpreter.Eval(`import ` + pluginAlias + ` "` + goabi.PluginImportPath + `"`)
	if err != nil {
		return fmt.Errorf("loading plugin: %w", err)
	}

	create, err := interpreter.Eval(pluginAlias + "." + goabi.CreateFunc)
	if err != nil || create.Kind() != reflect.Func {
		return fmt.Errorf("%w: %s", errNoCreate, wantCreate(host))
	}

	params := 0
	if host != "" {
		params = 1
	}

	typ := create.Type()
	if typ.NumIn() != params || typ.NumOut() != 2 || typ.Out(1) != reflect.TypeFor[error]() {
		return fmt.Errorf("%w: Create has type %s; want %s", errCreateType, typ, wantCreate(host))
	}

	if host != "" {
		return checkHostParam(payload.GoPath, host, payload.HostPath)
	}

	return nil
}

// wantCreate describes the Create a plugin must declare.
func wantCreate(host string) string {
	if host == "" {
		return "func Create() (<interface>, error) (Create takes a Host only when Register passes plg.WithHost)"
	}

	return "func Create(" + host + ") (<interface>, error) (Register passes plg.WithHost[" + host + "])"
}

// resolve looks up the dispatcher's variable holding the named method of the plugin instance (bound by
// [goabi.InitFunc]; the value Eval returns refers to the variable, so it sees that binding) and builds its
// codecs.
func (m *method) resolve(interpreter *interp.Interpreter, planner *wire.Planner, name string) error {
	fn, err := interpreter.Eval(dispatchAlias + "." + goabi.CallPrefix + name)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", name, err)
	}

	typ := fn.Type()
	if !methodShaped(typ) {
		return fmt.Errorf("%w: %s has type %s", errCallType, name, typ)
	}

	m.name, m.fn, m.variadic = name, fn, typ.IsVariadic()

	return m.buildCodecs(planner, typ)
}

// methodShaped reports whether typ is the type of an interface method of the contract:
// func(context.Context, ...) (..., error).
func methodShaped(typ reflect.Type) bool {
	return typ.Kind() == reflect.Func && typ.NumIn() > 0 && typ.In(0) == reflect.TypeFor[context.Context]() &&
		typ.NumOut() > 0 && typ.Out(typ.NumOut()-1) == reflect.TypeFor[error]()
}

func (m *method) buildCodecs(planner *wire.Planner, typ reflect.Type) error {
	m.args = make([]reflect.Value, typ.NumIn())
	m.args[0] = reflect.ValueOf(context.Background())
	m.argDec = make([]*wire.Codec, typ.NumIn()-1)

	for k := 1; k < typ.NumIn(); k++ {
		m.args[k] = reflect.New(typ.In(k)).Elem()

		codec, err := planner.For(typ.In(k))
		if err != nil {
			return fmt.Errorf("%s argument %d: %w", m.name, k, err)
		}

		m.argDec[k-1] = codec
	}

	m.results = make([]*wire.Codec, typ.NumOut()-1)
	for k := range m.results {
		codec, err := planner.For(typ.Out(k))
		if err != nil {
			return fmt.Errorf("%s result %d: %w", m.name, k, err)
		}

		m.results[k] = codec
	}

	return nil
}

// call calls the plugin's method with the decoded arguments; a variadic method gets its last argument as
// the slice it was encoded from.
func (m *method) call() []reflect.Value {
	if m.variadic {
		return m.fn.CallSlice(m.args)
	}

	return m.fn.Call(m.args)
}

// decodeArgs decodes payload into the reusable call arguments.
func (m *method) decodeArgs(payload []byte) error {
	reader := wire.NewReader(payload)

	for k, codec := range m.argDec {
		err := codec.Decode(&reader, m.args[k+1])
		if err != nil {
			return fmt.Errorf("argument %d: %w", k+1, err)
		}
	}

	err := reader.End()
	if err != nil {
		return fmt.Errorf("arguments: %w", err)
	}

	return nil
}

// appendResults encodes the results before the final error.
func (m *method) appendResults(dst []byte, out []reflect.Value) ([]byte, error) {
	for k, codec := range m.results {
		next, err := codec.Append(dst, out[k])
		if err != nil {
			return dst, fmt.Errorf("result %d: %w", k, err)
		}

		dst = next
	}

	return dst, nil
}

func (g *Guest) ok() []byte {
	g.out = append(g.out[:0], byte(wire.StatusOK))

	return g.out
}

func (g *Guest) fail(status wire.Status, msg string) []byte {
	g.out = wire.AppendMessage(append(g.out[:0], byte(status)), msg)

	return g.out
}
