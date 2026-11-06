package plg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/scriptabi"
	"github.com/heynemann/go-plg/internal/wire"
)

// scriptMemoryLimit is the default memory limit of each TypeScript (and Lua) plugin instance; an idle QuickJS
// instance takes about 0.5 MiB.
const scriptMemoryLimit = 64 << 20

// luaPluginPath is where Lua plugin instances find their files: the entrypoint and every module require loads.
const luaPluginPath = "/plugin"

var errLuaEntrypoint = errors.New("a Lua entrypoint is a .lua file")

// luaLoader loads Lua plugins: the plugin's .lua files are copied once into an immutable tree, mounted read-only
// at /plugin in each instance, where the Lua guest runs the entrypoint and resolves require.
type luaLoader struct{}

func (luaLoader) guestImport() string { return "github.com/heynemann/go-plg/guest/lua" }

func (luaLoader) memoryLimit() uint64 { return scriptMemoryLimit }

func (luaLoader) multiplexes() bool { return true }

func (luaLoader) missingValidate(*memfs.FS, string, *validation) string {
	return "the table Create returned has no method Validate: inst:Validate(v) returns a list of issue tables " +
		"({severity, code, message, field}), or nil and a message"
}

func (luaLoader) prepare(_ context.Context, _ *Container, reg *registration, src *source, files *memfs.FS) (
	launchSpec, error,
) {
	entrypoint := src.desc.Entrypoint

	tree, err := luaSourceTree(files, entrypoint)
	if err != nil {
		return launchSpec{}, err
	}

	payload := scriptabi.Payload{
		Name:     entrypoint,
		Methods:  uint32(len(reg.contr.methods)), //nolint:gosec // G115: an interface has far fewer than 2^32 methods
		Schema:   reg.schema,
		Validate: reg.validation != nil,
	}

	return launchSpec{payload: payload.Append(nil), mounts: []engine.Mount{{FS: tree, GuestPath: luaPluginPath}}}, nil
}

// luaSourceTree copies the plugin's regular .lua files (the only files a Lua plugin can load) into the tree its
// instances mount, after checking that the entrypoint is one of them.
func luaSourceTree(files fs.FS, entrypoint string) (*memfs.FS, error) {
	if !fs.ValidPath(entrypoint) || path.Ext(entrypoint) != ".lua" {
		return nil, fmt.Errorf("%w: %q", errLuaEntrypoint, entrypoint)
	}

	info, err := fs.Stat(files, entrypoint)
	if err != nil {
		return nil, fmt.Errorf("entrypoint: %w", err)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %q", errLuaEntrypoint, entrypoint)
	}

	return copyLuaFiles(files)
}

// copyLuaFiles copies every regular .lua file of files into an in-memory tree.
func copyLuaFiles(files fs.FS) (*memfs.FS, error) {
	var tree memfs.Builder

	err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() || path.Ext(name) != ".lua" {
			return err
		}

		data, err := fs.ReadFile(files, name)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}

		err = tree.Add(name, data)
		if err != nil {
			return fmt.Errorf("adding %s: %w", name, err)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("copying sources: %w", err)
	}

	mounted, err := tree.Build()
	if err != nil {
		return nil, fmt.Errorf("copying sources: %w", err)
	}

	return mounted, nil
}

// scriptSchema returns the encoded [wire.Schema] the script guests receive: the interface's methods, then
// Validate when the plugins validate (val is not nil), then the Host's. The sets were planned separately, so a
// type several use is described more than once; the guests do not care.
func scriptSchema(methods []*method, val *validation, hostMethods []*method) []byte {
	var schema wire.Schema

	for _, meth := range methods {
		schema.AddMethod(meth.name, meth.args, meth.results)
	}

	if val != nil {
		schema.AddMethod(goabi.ValidateMethod, []*wire.Codec{val.codec}, []*wire.Codec{val.scriptIssues})
	}

	for _, meth := range hostMethods {
		schema.AddMethod(meth.name, meth.args, meth.results)
	}

	return schema.Append(nil)
}
