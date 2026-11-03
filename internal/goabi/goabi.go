// Package goabi is the contract between the plg host and the generic Go guest: where the guest finds sources,
// the init payload that tells it what to load, and the dispatcher package the host synthesizes for it.
//
// The host mounts one read-only tree at [GoPath]: the contract package's source at its real import path, the
// plugin's source at [PluginImportPath] and the rendered dispatcher at [DispatchImportPath]. The guest
// imports the dispatcher, calls [InitFunc] (which runs the plugin's Create) and resolves one
// [CallPrefix]+<Method> function per interface method, in the order of [InitPayload.Methods].
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
}

// Append appends the encoding of p. It fails only for a string over 4 GiB.
func (p *InitPayload) Append(b []byte) ([]byte, error) {
	b, err := appendStrings(b, p.GoPath, p.Dispatch)
	if err != nil {
		return b, err
	}

	return appendStrings(wire.AppendUint(b, uint64(len(p.Methods))), p.Methods...)
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

var errMethodCount = errors.New("method count exceeds the payload")

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

	methods, err := decodeMethods(reader)
	if err != nil {
		return InitPayload{}, err
	}

	err = reader.End()
	if err != nil {
		return InitPayload{}, fmt.Errorf("after methods: %w", err)
	}

	return InitPayload{GoPath: goPath, Dispatch: dispatch, Methods: methods}, nil
}

func decodeMethods(reader *wire.Reader) ([]string, error) {
	count, err := reader.Uint64()
	if err != nil {
		return nil, fmt.Errorf("method count: %w", err)
	}

	if count > math.MaxInt32 || int(count) > reader.Len()/minStringSize {
		return nil, fmt.Errorf("%w: %d", errMethodCount, count)
	}

	methods := make([]string, count)
	for i := range methods {
		methods[i], err = reader.Text()
		if err != nil {
			return nil, fmt.Errorf("method %d: %w", i, err)
		}
	}

	return methods, nil
}
