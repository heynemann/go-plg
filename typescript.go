package plg

import (
	"io/fs"

	"github.com/heynemann/go-plg/internal/scriptabi"
	"github.com/heynemann/go-plg/internal/tsbundle"
)

// tsLoader loads TypeScript plugins: esbuild bundles the entrypoint and the files it imports, from the plugin's
// fs.FS, on the host, and the QuickJS guest evaluates the bundle. The guest needs no mount: the bundle is the
// whole plugin.
type tsLoader struct{}

func (tsLoader) guestImport() string { return "github.com/heynemann/go-plg/guest/typescript" }

func (tsLoader) memoryLimit() uint64 { return scriptMemoryLimit }

func (tsLoader) prepare(reg *registration, files fs.FS, entrypoint string) (launchSpec, error) {
	bundle, err := tsbundle.Bundle(files, entrypoint)
	if err != nil {
		return launchSpec{}, err //nolint:wrapcheck // tsbundle's error names the plugin's files; the caller adds the key
	}

	payload := scriptabi.Payload{
		Name:    entrypoint,
		Source:  bundle,
		Methods: uint32(len(reg.contr.methods)), //nolint:gosec // G115: an interface has far fewer than 2^32 methods
		Schema:  reg.schema,
	}

	return launchSpec{payload: payload.Append(nil)}, nil
}
