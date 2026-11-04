package tsbundle

import (
	"io/fs"

	"github.com/heynemann/go-plg/internal/wire"
)

// PluginPath is where the bundler guest finds the plugin's files, which the host mounts there read-only.
const PluginPath = "/plugin"

// Guest is the bundler guest's side of the plg guest ABI; guestsrc/tsbundle exports it from a wasip1 reactor.
// plg_init's payload is the entrypoint, a path inside the plugin's files, and its result the bundle
// ([wire.StatusOK], the body being the script) or the reason it could not be bundled ([wire.StatusLoad], esbuild's
// errors as [Bundle] lists them after [ErrBundle]). The bundler has no methods: plg_call breaks the ABI, and
// plg_close does nothing.
type Guest struct {
	files   fs.FS
	in, out []byte
}

// NewGuest returns the guest bundling the plugin whose files are files.
func NewGuest(files fs.FS) *Guest {
	return &Guest{files: files}
}

// Alloc returns the input buffer, grown to at least size bytes.
func (g *Guest) Alloc(size uint32) []byte {
	if uint64(cap(g.in)) < uint64(size) {
		g.in = make([]byte, size)
	}

	g.in = g.in[:size]

	return g.in
}

// Init bundles the plugin whose entrypoint is the first size bytes of the input buffer and returns the
// status-prefixed result.
func (g *Guest) Init(size uint32) []byte {
	if uint64(size) > uint64(len(g.in)) {
		return g.fail(wire.StatusABI, "init payload exceeds the input buffer")
	}

	bundle, err := run(g.files, string(g.in[:size]))
	if err != nil {
		return g.fail(wire.StatusLoad, err.Error())
	}

	g.out = append(append(g.out[:0], byte(wire.StatusOK)), bundle...)

	return g.out
}

// Call refuses every call: the bundler has no methods.
func (g *Guest) Call() []byte {
	return g.fail(wire.StatusABI, "the bundler has no methods")
}

// Close ends the instance; there is nothing to end.
func (g *Guest) Close() []byte {
	g.out = append(g.out[:0], byte(wire.StatusOK))

	return g.out
}

func (g *Guest) fail(status wire.Status, msg string) []byte {
	g.out = wire.AppendMessage(append(g.out[:0], byte(status)), msg)

	return g.out
}
