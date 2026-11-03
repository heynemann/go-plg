// Package goabi is the contract between the plg host and the generic Go guest: where the guest finds sources,
// the init payload that tells it what to load, and the dispatcher package the host synthesizes for it.
//
// The host mounts one read-only tree at [GoPath]: the contract package's source at its real import path, the
// plugin's source at [PluginImportPath] and the rendered dispatcher at [DispatchImportPath]. The guest
// imports the dispatcher, fills its [HostCallPrefix]+<Method> variables with calls into the host (when the
// plugins have a Host), calls [InitFunc] (which runs the plugin's Create, passing it the Host stubs) and resolves
// one [CallPrefix]+<Method> function per interface method, in the order of [InitPayload.Methods].
package goabi

import (
	"errors"
	"fmt"
	"math"

	"github.com/heynemann/go-plg/internal/wire"
)

// Names shared by the host-side renderer and the guest.
const (
	// GoPath is where the host mounts the source tree inside the guest.
	GoPath = "/gopath"
	// PluginImportPath is the synthetic import path of the plugin package, whatever its package name.
	PluginImportPath = "plg.local/plugin"
	// DispatchImportPath is the import path of the rendered dispatcher package.
	DispatchImportPath = "plg.local/dispatch"
	// InitFunc is the dispatcher function that creates the plugin instance.
	InitFunc = "PlgInit"
	// CreateFunc is the function every Go plugin package declares.
	CreateFunc = "Create"
	// CallPrefix prefixes the dispatcher function forwarding each interface method.
	CallPrefix = "PlgCall_"
	// HostCallPrefix prefixes the dispatcher variable each Host stub method calls: the guest sets it to a function
	// that encodes the arguments, calls the plg.host_call import and decodes the answer.
	HostCallPrefix = "PlgHostCall_"
)

// ErrPayload reports a malformed init payload.
var ErrPayload = errors.New("goabi: malformed init payload")

// InitPayload tells the guest what to load.
type InitPayload struct {
	// GoPath is the GOPATH the interpreter resolves imports from.
	GoPath string
	// Dispatch is the import path of the dispatcher package.
	Dispatch string
	// Methods lists the interface's method names; a call's method index refers to this order.
	Methods []string
	// Host names the Host interface the plugin's Create takes as Go source refers to it, such as
	// "contract.Host", or is empty when the plugins have no Host and Create takes nothing.
	Host string
	// HostPath is the import path of the Host interface's package.
	HostPath string
	// HostMethods lists the Host interface's method names; a host_call's method index refers to this order.
	HostMethods []string
}

// Append appends the encoding of p. It fails only for a string over 4 GiB.
func (p *InitPayload) Append(dst []byte) ([]byte, error) {
	dst, err := appendStrings(dst, p.GoPath, p.Dispatch)
	if err == nil {
		dst, err = appendStrings(wire.AppendUint(dst, uint64(len(p.Methods))), p.Methods...)
	}

	if err == nil {
		dst, err = appendStrings(dst, p.Host, p.HostPath)
	}

	if err != nil {
		return dst, err
	}

	return appendStrings(wire.AppendUint(dst, uint64(len(p.HostMethods))), p.HostMethods...)
}

func appendStrings(dst []byte, strs ...string) ([]byte, error) {
	for _, s := range strs {
		next, err := wire.AppendString(dst, s)
		if err != nil {
			return dst, fmt.Errorf("goabi: %w", err)
		}

		dst = next
	}

	return dst, nil
}

// minStringSize is the encoded size of an empty string; it bounds how many methods a payload can describe.
const minStringSize = 4

var errCount = errors.New("count exceeds the payload")

// DecodeInitPayload decodes what [InitPayload.Append] wrote. It never allocates more than the input justifies.
func DecodeInitPayload(b []byte) (InitPayload, error) {
	reader := wire.NewReader(b)

	payload, err := decodePayload(&reader)
	if err != nil {
		return InitPayload{}, fmt.Errorf("%w: %w", ErrPayload, err)
	}

	return payload, nil
}

func decodePayload(reader *wire.Reader) (InitPayload, error) {
	goPath, err := reader.Text()
	if err != nil {
		return InitPayload{}, fmt.Errorf("go path: %w", err)
	}

	dispatch, err := reader.Text()
	if err != nil {
		return InitPayload{}, fmt.Errorf("dispatch path: %w", err)
	}

	methods, err := decodeMethods(reader, "method")
	if err != nil {
		return InitPayload{}, err
	}

	host, err := reader.Text()
	if err != nil {
		return InitPayload{}, fmt.Errorf("host: %w", err)
	}

	hostPath, err := reader.Text()
	if err != nil {
		return InitPayload{}, fmt.Errorf("host path: %w", err)
	}

	hostMethods, err := decodeMethods(reader, "host method")
	if err != nil {
		return InitPayload{}, err
	}

	err = reader.End()
	if err != nil {
		return InitPayload{}, fmt.Errorf("after host methods: %w", err)
	}

	return InitPayload{
		GoPath: goPath, Dispatch: dispatch, Methods: methods, Host: host, HostPath: hostPath, HostMethods: hostMethods,
	}, nil
}

// decodeMethods decodes a list of method names; what names them in errors.
func decodeMethods(reader *wire.Reader, what string) ([]string, error) {
	count, err := reader.Uint64()
	if err != nil {
		return nil, fmt.Errorf("%s count: %w", what, err)
	}

	if count > math.MaxInt32 || int(count) > reader.Len()/minStringSize {
		return nil, fmt.Errorf("%s %w: %d", what, errCount, count)
	}

	methods := make([]string, count)
	for i := range methods {
		methods[i], err = reader.Text()
		if err != nil {
			return nil, fmt.Errorf("%s %d: %w", what, i, err)
		}
	}

	return methods, nil
}
