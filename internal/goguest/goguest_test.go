package goguest_test

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/contractsrc"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/goallow"
	"github.com/heynemann/go-plg/internal/goguest"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/wire"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func fooerType() reflect.Type { return reflect.TypeFor[testcontract.Fooer]() }

// methodIndex returns the index of the named Fooer method, the order the host sends in the init payload.
func methodIndex(t *testing.T, name string) uint32 {
	t.Helper()

	m, ok := fooerType().MethodByName(name)
	require.True(t, ok, name)

	return uint32(m.Index) //nolint:gosec // G115: a handful of methods
}

func methodNames() []string {
	names := make([]string, fooerType().NumMethod())
	for i := range names {
		names[i] = fooerType().Method(i).Name
	}

	return names
}

// gopath lays out what the host mounts at /gopath: the contract source, the plugin and the rendered dispatcher.
func gopath(t *testing.T, plugin map[string]string) string {
	t.Helper()

	dispatcher, err := goabi.RenderDispatcher(fooerType())
	require.NoError(t, err)

	return gopathWith(t, plugin, string(dispatcher))
}

// gopathWith is gopath with the given dispatcher source, to model a host the guest does not agree with.
func gopathWith(t *testing.T, plugin map[string]string, dispatcher string) string {
	t.Helper()

	root := t.TempDir()

	contract, err := contractsrc.Snapshot(os.DirFS(filepath.Join("..", "testcontract")))
	require.NoError(t, err)
	require.NoError(t, os.CopyFS(filepath.Join(root, "src", fooerType().PkgPath()), contract))

	writeFiles(t, filepath.Join(root, "src", goabi.PluginImportPath), plugin)
	writeFiles(t, filepath.Join(root, "src", goabi.DispatchImportPath), map[string]string{"dispatch.go": dispatcher})

	return root
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(dir, 0o750))

	for name, src := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
	}
}

func testdataPlugin(t *testing.T, name string) map[string]string {
	t.Helper()

	dir := filepath.Join("..", "..", "testdata", "plugins", "go", name)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	files := map[string]string{}

	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: fixed testdata tree
		require.NoError(t, err)

		files[e.Name()] = string(data)
	}

	return files
}

type result struct {
	msg    string
	body   []byte
	status wire.Status
}

func parse(t *testing.T, res []byte) result {
	t.Helper()

	require.NotEmpty(t, res, "a result always carries a status byte")

	parsed := result{status: wire.Status(res[0]), body: bytes.Clone(res[1:])}
	if parsed.status != wire.StatusOK {
		reader := wire.NewReader(parsed.body)

		msg, err := reader.Text()
		require.NoError(t, err)
		require.NoError(t, reader.End())

		parsed.msg = msg
	}

	return parsed
}

func send(guest *goguest.Guest, req []byte) uint32 {
	copy(guest.Alloc(uint32(len(req))), req) //nolint:gosec // G115: small test requests

	return uint32(len(req)) //nolint:gosec // G115: small test requests
}

func initPayload(t *testing.T, root string, methods []string) []byte {
	t.Helper()

	p := goabi.InitPayload{GoPath: root, Dispatch: goabi.DispatchImportPath, Methods: methods}
	b, err := p.Append(nil)
	require.NoError(t, err)

	return b
}

func load(t *testing.T, plugin map[string]string) (*goguest.Guest, result) {
	t.Helper()

	var stdout bytes.Buffer

	guest := goguest.New(&stdout, &stdout)
	res := guest.Init(send(guest, initPayload(t, gopath(t, plugin), methodNames())))

	return guest, parse(t, bytes.Clone(res))
}

func loadOK(t *testing.T, plugin string) *goguest.Guest {
	t.Helper()

	guest, res := load(t, testdataPlugin(t, plugin))
	require.Equal(t, wire.StatusOK, res.status, res.msg)

	return guest
}

func encode(t *testing.T, args ...any) []byte {
	t.Helper()

	var buf []byte

	for _, a := range args {
		codec, err := wire.For(reflect.TypeOf(a))
		require.NoError(t, err)

		buf, err = codec.Append(buf, reflect.ValueOf(a))
		require.NoError(t, err)
	}

	return buf
}

// decode decodes body into values of the types of the given zero values and checks nothing is left over.
func decode(t *testing.T, body []byte, zeros ...any) []any {
	t.Helper()

	reader := wire.NewReader(body)
	out := make([]any, len(zeros))

	for i, z := range zeros {
		c, err := wire.For(reflect.TypeOf(z))
		require.NoError(t, err)

		v := reflect.New(reflect.TypeOf(z)).Elem()
		require.NoError(t, c.Decode(&reader, v))

		out[i] = v.Interface()
	}

	require.NoError(t, reader.End())

	return out
}

func call(t *testing.T, guest *goguest.Guest, method string, args ...any) result {
	t.Helper()

	return parse(t, guest.Call(methodIndex(t, method), send(guest, encode(t, args...))))
}

// The tests interpreting plugins run serially (//nolint:paralleltest below): every one of them interprets the
// testcontract package, whose Tree type is recursive, and yaegi v0.16.1 builds a recursive struct type by
// patching the field types of a reflect.StructOf type in place (unsafe2.SetFieldType). reflect caches StructOf
// types process-wide, so two interpreters in parallel tests patch the same type: a data race in yaegi, not in
// plg. In production each interpreter runs alone in its own wasm instance.

//nolint:paralleltest // interprets testcontract: serial, see above
func TestCallsReachThePlugin(t *testing.T) {
	for _, plugin := range []string{"fooer", "fooermain"} {
		t.Run(plugin, func(t *testing.T) {
			guest := loadOK(t, plugin)
			res := call(t, guest, "Foo", "  bob ")
			require.Equal(t, wire.StatusOK, res.status, res.msg)

			got := decode(t, res.body, "")[0]
			want := testcontract.Greeting("bob")

			if plugin == "fooermain" {
				want = "main says " + want
			}

			assert.Equal(t, want, got, "interpreted contract code behaves like compiled code")
		})
	}
}

//nolint:paralleltest // interprets testcontract: serial, see above
func TestPluginStateLivesInTheInstance(t *testing.T) {
	guest := loadOK(t, "fooer")
	for want := 1; want <= 3; want++ {
		res := call(t, guest, "Count")
		require.Equal(t, wire.StatusOK, res.status, res.msg)
		assert.Equal(t, want, decode(t, res.body, 0)[0])
	}

	other := loadOK(t, "fooer")
	res := call(t, other, "Count")
	assert.Equal(t, 1, decode(t, res.body, 0)[0], "each guest runs its own Create")
}

//nolint:paralleltest // interprets testcontract: serial, see above
func TestScalarsRoundTrip(t *testing.T) {
	guest := loadOK(t, "fooer")

	tests := []struct {
		name string
		args []any
		want []any
	}{
		{
			name: "zero values",
			args: []any{
				false, 0, int8(0), int16(0), int32(0), int64(0), uint(0), uint8(0), uint16(0), uint32(0),
				uint64(0), float32(0), float64(0), "",
			},
			want: []any{
				true, 1, int8(1), int16(1), int32(1), int64(1), uint(1), uint8(1), uint16(1), uint32(1),
				uint64(1), float32(0), float64(0), "",
			},
		},
		{
			name: "extremes wrap like compiled Go, within ±2^53 for 64-bit kinds",
			args: []any{
				true, wire.MaxSafeInt - 1, int8(math.MaxInt8), int16(math.MaxInt16), int32(math.MaxInt32),
				int64(-wire.MaxSafeInt), uint(wire.MaxSafeInt - 1), uint8(math.MaxUint8), uint16(math.MaxUint16),
				uint32(math.MaxUint32), uint64(wire.MaxSafeInt - 1), float32(-1.5), math.SmallestNonzeroFloat64,
				"héllo",
			},
			want: []any{
				false, wire.MaxSafeInt, int8(math.MinInt8), int16(math.MinInt16), int32(math.MinInt32),
				int64(-wire.MaxSafeInt + 1), uint(wire.MaxSafeInt), uint8(0), uint16(0), uint32(0),
				uint64(wire.MaxSafeInt), float32(-3), 2 * math.SmallestNonzeroFloat64, "HÉLLO",
			},
		},
	}

	for _, tc := range tests {
		res := call(t, guest, "Scalars", tc.args...)
		require.Equal(t, wire.StatusOK, res.status, res.msg)
		assert.Equal(t, tc.want, decode(t, res.body, tc.want...), tc.name)
	}
}

// A result the plugin computes beyond ±2^53 cannot cross: the call fails with StatusConvert naming the
// result, and the instance keeps serving.
//
//nolint:paralleltest // interprets testcontract: serial, see above
func TestResultsBeyondTwoToThe53DoNotCross(t *testing.T) {
	guest := loadOK(t, "fooer")

	res := call(t, guest, "Scalars", false, wire.MaxSafeInt, int8(0), int16(0), int32(0), int64(0), uint(0),
		uint8(0), uint16(0), uint32(0), uint64(0), float32(0), float64(0), "")
	require.Equal(t, wire.StatusConvert, res.status, res.msg)
	assert.True(t, strings.HasPrefix(res.msg, "result 1: "), res.msg)
	assert.Contains(t, res.msg, strconv.Itoa(wire.MaxSafeInt+1))

	res = call(t, guest, "Spin", 3)
	require.Equal(t, wire.StatusOK, res.status, "the instance is not broken: %s", res.msg)
	assert.Equal(t, 3, decode(t, res.body, 0)[0])
}

//nolint:paralleltest // interprets testcontract: serial, see above
func TestNamedScalarCrossesByKind(t *testing.T) {
	guest := loadOK(t, "fooer")

	for _, tc := range []struct {
		c    float64
		boil bool
		f    float64
	}{{99.5, false, 211.1}, {100, true, 212}, {-40, false, -40}} {
		res := call(t, guest, "Boil", tc.c)
		require.Equal(t, wire.StatusOK, res.status, res.msg)

		got := decode(t, res.body, false, 0.0)
		assert.Equal(t, tc.boil, got[0])
		assert.InDelta(t, tc.f, got[1], 1e-9)
	}
}

//nolint:paralleltest // interprets testcontract: serial, see above
func TestPluginErrorsAndPanics(t *testing.T) {
	guest := loadOK(t, "fooer")

	res := call(t, guest, "Fail", "boom")
	assert.Equal(t, wire.StatusError, res.status)
	assert.Equal(t, "boom", res.msg)

	res = call(t, guest, "Fail", "")
	assert.Equal(t, wire.StatusOK, res.status)
	assert.Empty(t, res.body, "an error-only method has no result values")

	res = call(t, guest, "Fail", "panic")
	assert.Equal(t, wire.StatusPanic, res.status)
	assert.Contains(t, res.msg, "plugin panicked on purpose")
}

//nolint:paralleltest // interprets testcontract: serial, see above
func TestLoadFailures(t *testing.T) {
	tests := []struct {
		plugin string
		msg    []string
		status wire.Status
	}{
		{"syntaxerror", []string{"loading plugin", "plugin.go"}, wire.StatusLoad},
		{"nocreate", []string{"must declare func Create"}, wire.StatusLoad},
		{"badcreate", []string{"Create has type func(string)"}, wire.StatusLoad},
		{"forbidden", []string{"loading plugin", `"os"`}, wire.StatusLoad},
		{"wronginterface", []string{"loading dispatcher", "half", "testcontract.Fooer"}, wire.StatusLoad},
		{"nilcreate", []string{"Create returned a nil testcontract.Fooer"}, wire.StatusLoad},
		{"createfails", []string{"database unreachable"}, wire.StatusError},
	}

	for _, tc := range tests {
		t.Run(tc.plugin, func(t *testing.T) {
			guest, res := load(t, testdataPlugin(t, tc.plugin))
			assert.Equal(t, tc.status, res.status, res.msg)

			for _, m := range tc.msg {
				assert.Contains(t, res.msg, m)
			}

			rejected := parse(t, guest.Call(0, send(guest, nil)))
			assert.Equal(t, wire.StatusABI, rejected.status, "a failed load leaves the guest unusable")
		})
	}
}

func importingPlugin(body string, imports ...string) map[string]string {
	var src strings.Builder

	src.WriteString("package p\n\nimport (\n\t\"github.com/heynemann/go-plg/internal/testcontract\"\n")

	for _, imp := range imports {
		src.WriteString("\t_ \"" + imp + "\"\n")
	}

	src.WriteString(")\n\ntype embedder struct{ testcontract.Fooer }\n\n" +
		"func Create() (testcontract.Fooer, error) {\n" + body + "\n\treturn embedder{}, nil\n}\n")

	return map[string]string{"p.go": src.String()}
}

//nolint:paralleltest // interprets testcontract: serial, see above
func TestStdlibAllowlist(t *testing.T) {
	t.Run("allowed", func(t *testing.T) {
		var stdout bytes.Buffer

		// Every package of the shared allowlist (internal/goallow) is importable.
		plugin := importingPlugin(`fmt.Println("ready")`, goallow.Packages()...)
		plugin["p.go"] = strings.Replace(plugin["p.go"], "\t_ \"fmt\"\n", "\t\"fmt\"\n", 1)

		guest := goguest.New(&stdout, &stdout)
		res := parse(t, guest.Init(send(guest, initPayload(t, gopath(t, plugin), methodNames()))))
		require.Equal(t, wire.StatusOK, res.status, res.msg)
		assert.Equal(t, "ready\n", stdout.String(), "plugin stdout goes to the guest's writer")
	})

	for _, pkg := range []string{"os", "os/exec", "net", "net/http/httputil", "syscall", "unsafe", "reflect", "io/fs"} {
		t.Run(pkg, func(t *testing.T) {
			_, res := load(t, importingPlugin("", pkg))
			assert.Equal(t, wire.StatusLoad, res.status)
			assert.Contains(t, res.msg, `"`+pkg+`"`)
		})
	}
}

// A plugin may implement part of the interface by embedding it, as in compiled Go: its own methods work and
// the others panic only when called (yaegi cannot bind them; the dispatcher forwards them instead).
//
//nolint:paralleltest // interprets testcontract: serial, see above
func TestPartialImplementationPanicsOnlyWhenCalled(t *testing.T) {
	guest, res := load(t, map[string]string{"p.go": `package p

import (
	"context"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type partial struct{ testcontract.Fooer }

func (partial) Foo(_ context.Context, name string) (string, error) { return "only " + name, nil }

func Create() (testcontract.Fooer, error) { return partial{}, nil }
`})
	require.Equal(t, wire.StatusOK, res.status, res.msg)

	res = call(t, guest, "Foo", "me")
	require.Equal(t, wire.StatusOK, res.status, res.msg)
	assert.Equal(t, "only me", decode(t, res.body, "")[0])

	res = call(t, guest, "Count")
	assert.Equal(t, wire.StatusPanic, res.status, "the unimplemented method panics on the call, like Go")
}

//nolint:paralleltest // interprets testcontract: serial, see above
func TestNetHTTPIsClientOnly(t *testing.T) {
	for name, body := range map[string]string{
		"client":         "_ = http.DefaultClient.Get\n\t_, _ = http.NewRequest(http.MethodGet, \"http://a/\", nil)",
		"ListenAndServe": "_ = http.ListenAndServe",
		"Server":         "_ = &http.Server{}",
		"ServeMux":       "_ = http.NewServeMux",
	} {
		t.Run(name, func(t *testing.T) {
			plugin := importingPlugin(body, "net/http")
			plugin["p.go"] = strings.Replace(plugin["p.go"], "\t_ \"net/http\"\n", "\t\"net/http\"\n", 1)

			_, res := load(t, plugin)
			if name == "client" {
				assert.Equal(t, wire.StatusOK, res.status, res.msg)

				return
			}

			assert.Equal(t, wire.StatusLoad, res.status, "server-side symbols are not in the guest")
		})
	}
}

//nolint:paralleltest // interprets testcontract: serial, see above
func TestABIViolations(t *testing.T) {
	root := gopath(t, testdataPlugin(t, "fooer"))
	payload := initPayload(t, root, methodNames())

	t.Run("init larger than the input buffer", func(t *testing.T) {
		guest := goguest.New(nil, nil)
		res := parse(t, guest.Init(send(guest, payload)+1))
		assert.Equal(t, wire.StatusABI, res.status)
	})

	t.Run("malformed init payload", func(t *testing.T) {
		guest := goguest.New(nil, nil)
		res := parse(t, guest.Init(send(guest, payload[:len(payload)-1])))
		assert.Equal(t, wire.StatusABI, res.status)
		assert.Contains(t, res.msg, goabi.ErrPayload.Error())
	})

	t.Run("unknown method in payload", func(t *testing.T) {
		guest := goguest.New(nil, nil)
		res := parse(t, guest.Init(send(guest, initPayload(t, root, []string{"Missing"}))))
		assert.Equal(t, wire.StatusABI, res.status)
		assert.Contains(t, res.msg, "PlgCall_Missing")
	})

	guest := goguest.New(nil, nil)
	assert.Equal(t, wire.StatusABI, parse(t, guest.Call(0, send(guest, nil))).status, "call before init")
	require.Equal(t, wire.StatusOK, parse(t, guest.Init(send(guest, payload))).status)
	assert.Equal(t, wire.StatusABI, parse(t, guest.Init(send(guest, payload))).status, "init twice")

	foo := methodIndex(t, "Foo")
	args := encode(t, "x")

	size := uint32(len(args))             //nolint:gosec // G115: a few bytes
	methods := uint32(len(methodNames())) //nolint:gosec // G115: a few methods

	tests := []struct {
		name   string
		msg    string
		req    []byte
		method uint32
		size   uint32
	}{
		{"unknown method index", "unknown method", args, methods, size},
		{"size beyond the input buffer", "exceed the input buffer", args, foo, size + 1},
		{"truncated arguments", wire.ErrTruncated.Error(), args[:3], foo, 3},
		{"trailing bytes", wire.ErrTrailing.Error(), append(bytes.Clone(args), 0), foo, size + 1},
	}
	for _, tc := range tests {
		copy(guest.Alloc(uint32(len(tc.req))), tc.req) //nolint:gosec // G115: small test requests

		res := parse(t, guest.Call(tc.method, tc.size))
		assert.Equal(t, wire.StatusABI, res.status, tc.name)
		assert.Contains(t, res.msg, tc.msg, tc.name)
	}

	ok := call(t, guest, "Foo", "still works")
	assert.Equal(t, wire.StatusOK, ok.status, "ABI rejections leave the plugin intact")

	assert.Equal(t, wire.StatusOK, parse(t, guest.Close()).status)
	assert.Equal(t, wire.StatusABI, call(t, guest, "Foo", "x").status, "call after close")
	assert.Equal(t, wire.StatusABI, parse(t, guest.Init(send(guest, payload))).status, "init after close")
}

func TestAllocReusesTheInputBuffer(t *testing.T) {
	t.Parallel()

	guest := goguest.New(nil, nil)
	big := guest.Alloc(64)
	small := guest.Alloc(8)

	assert.Len(t, small, 8)
	assert.Same(t, &big[0], &small[0], "a smaller request reuses the buffer")
	assert.Len(t, guest.Alloc(128), 128)
}

// TestRejectsADispatcherItDoesNotAgreeWith models a host whose dispatcher does not match the guest's ABI: the
// guest reports it instead of crashing.
//
//nolint:paralleltest // interprets testcontract: serial, see above
func TestRejectsADispatcherItDoesNotAgreeWith(t *testing.T) {
	const header = "package dispatch\n\nimport (\n\t\"context\"\n\n\t_ \"plg.local/plugin\"\n)\n\n" +
		"var _ context.Context\n\n"

	const init = "func PlgInit() (string, error) { return \"\", nil }\n"

	tests := []struct {
		name       string
		dispatcher string
		msg        string
	}{
		{"no init function", header, "dispatcher"},
		{
			"call without context",
			header + init + "func PlgCall_Foo(s string) (string, error) { return s, nil }\n",
			"Foo has type func(string)",
		},
		{
			"call without error",
			header + init + "func PlgCall_Foo(ctx context.Context) string { return \"\" }\n",
			"want func(context.Context",
		},
		{"call of a non-function", header + init + "var PlgCall_Foo = 3\n", "want func(context.Context"},
		{
			"unsupported argument",
			header + init + "func PlgCall_Foo(ctx context.Context, c chan int) error { return nil }\n",
			"argument 1",
		},
		{
			"unsupported result",
			header + init + "func PlgCall_Foo(ctx context.Context) (map[int]string, error) { return nil, nil }\n",
			"result 0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := gopathWith(t, testdataPlugin(t, "fooer"), tc.dispatcher)
			guest := goguest.New(nil, nil)
			res := parse(t, guest.Init(send(guest, initPayload(t, root, []string{"Foo"}))))
			assert.Equal(t, wire.StatusABI, res.status, res.msg)
			assert.Contains(t, res.msg, tc.msg)
		})
	}
}
