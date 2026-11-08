package plg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/offline"
	"github.com/heynemann/go-plg/internal/scriptabi"
	"github.com/heynemann/go-plg/internal/wire"
)

// luaSyntaxEntrypoint is the chunk the Lua syntax check runs: plg's own, never one of the plugin's files, which it
// embeds.
const luaSyntaxEntrypoint = "plg-syntax-check.lua"

var errLuaSyntaxCheck = errors.New("the Lua syntax check failed")

// checkLuaSyntax is [offline.LuaSyntax]: in an instance of the Lua guest, it runs a chunk of plg's own that compiles
// every .lua file a Lua plugin instance receives (copyLuaFiles) with load, under the name require gives it
// ("@<path>", so errors read <path>:<line>: <message>), without running any; the chunk's Create returns the
// errors. Its Create is bounded by [offline.DefaultTimeout].
func checkLuaSyntax(ctx context.Context, files fs.FS, opts *offline.Options) (_ []string, err error) {
	mounted, err := luaSyntaxMount(files)
	if err != nil || mounted == nil {
		return nil, err
	}

	_, guest, err := guestFor(LanguageLua)
	if err != nil {
		return nil, err
	}

	//nolint:contextcheck // New takes no context: what the container starts ends with its Close, below
	ctr := New(WithLogger(opts.Logger), WithCompilationCacheDir(opts.CacheDir))

	defer func() {
		err = errors.Join(err, ctr.Close(context.WithoutCancel(ctx)))
	}()

	mod, err := ctr.module(ctx, LanguageLua, guest, scriptMemoryLimit)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, offline.DefaultTimeout)
	defer cancel()

	inst, err := mod.Instantiate(ctx, &engine.InstanceOptions{
		Mounts: []engine.Mount{{FS: mounted, GuestPath: luaPluginPath}},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: starting an instance: %w", errLuaSyntaxCheck, err)
	}

	defer func() {
		err = errors.Join(err, inst.Close(context.WithoutCancel(ctx)))
	}()

	payload := scriptabi.Payload{Name: luaSyntaxEntrypoint, Schema: (&wire.Schema{}).Append(nil)}

	res, err := inst.Init(ctx, payload.Append(nil))

	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %w", errLuaSyntaxCheck, err)
	case res.Status == wire.StatusOK:
		return nil, nil
	case res.Status == wire.StatusError:
		return strings.Split(res.Message, "\n"), nil
	default:
		return nil, fmt.Errorf("%w: %v", errLuaSyntaxCheck, guestFailure(res))
	}
}

// luaSyntaxMount returns the plugin directory the syntax check's instance mounts: its chunk alone; nil when files
// hold no .lua file.
func luaSyntaxMount(files fs.FS) (*memfs.FS, error) {
	chunk, err := luaSyntaxChunk(files)
	if err != nil || chunk == nil {
		return nil, err
	}

	var tree memfs.Builder

	err = tree.Add(luaSyntaxEntrypoint, chunk)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errLuaSyntaxCheck, err)
	}

	mounted, err := tree.Build()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errLuaSyntaxCheck, err)
	}

	return mounted, nil
}

// luaSyntaxChunk returns the chunk compiling every .lua file of files, in name order, each embedded as a long
// string; nil when there is none.
func luaSyntaxChunk(files fs.FS) ([]byte, error) {
	sources, err := copyLuaFiles(files)
	if err != nil {
		return nil, err
	}

	var chunk strings.Builder

	//nolint:dupword // the Lua function's end follows the if's
	chunk.WriteString("local errs = {}\nlocal function check(name, src)\n  local _, err = load(src, \"@\" .. name)\n" +
		"  if err then errs[#errs + 1] = err end\nend\n")

	found := false

	err = fs.WalkDir(sources, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		src, err := fs.ReadFile(sources, name)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}

		found = true

		chunk.WriteString("check(" + luaLongString(name) + ", " + luaLongString(string(src)) + ")\n")

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errLuaSyntaxCheck, err)
	}

	if !found {
		return nil, nil
	}

	chunk.WriteString("function Create()\n  if #errs > 0 then return nil, table.concat(errs, \"\\n\") end\n" +
		"  return {}\nend\n")

	return []byte(chunk.String()), nil
}

// luaLongString returns s as a Lua long string, exactly: the level of its brackets is the first that s never
// closes (where it ends included), and the newline after the opening bracket, which Lua skips, keeps a first
// newline of s.
func luaLongString(s string) string {
	level := ""

	for {
		closing := "]" + level + "]"
		if strings.Index(s+closing, closing) == len(s) {
			return "[" + level + "[\n" + s + closing
		}

		level += "="
	}
}
