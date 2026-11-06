package goguest_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/goguest"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/wire"
)

func hostType() reflect.Type   { return reflect.TypeFor[testcontract.Host]() }
func hostedType() reflect.Type { return reflect.TypeFor[testcontract.Hosted]() }

func names(iface reflect.Type) []string {
	out := make([]string, iface.NumMethod())
	for i := range out {
		out[i] = iface.Method(i).Name
	}

	return out
}

func index(t *testing.T, iface reflect.Type, name string) uint32 {
	t.Helper()

	m, ok := iface.MethodByName(name)
	require.True(t, ok, name)

	return uint32(m.Index) //nolint:gosec // G115: a handful of methods
}

// hostCall is one call the guest made to its host.
type hostCall struct {
	args   []byte
	method uint32
}

// fakeHost answers the guest's host calls with answer and records them.
type fakeHost struct {
	answer func(method uint32) []byte
	calls  []hostCall
}

func (f *fakeHost) call(method uint32, args, dst []byte) []byte {
	f.calls = append(f.calls, hostCall{method: method, args: bytes.Clone(args)})

	return append(dst, f.answer(method)...)
}

func okAnswer(t *testing.T, values ...any) []byte {
	t.Helper()

	return append([]byte{byte(wire.StatusOK)}, encode(t, values...)...)
}

func msgAnswer(status wire.Status, msg string) []byte {
	return wire.AppendMessage([]byte{byte(status)}, msg)
}

// hostedPayload is the init payload of a Hosted plugin whose Host is testcontract.Host.
func hostedPayload(t *testing.T, root string, hostMethods []string) []byte {
	t.Helper()

	p := goabi.InitPayload{
		GoPath: root, Dispatch: goabi.DispatchImportPath, Methods: names(hostedType()),
		Host: "testcontract.Host", HostPath: hostType().PkgPath(), HostMethods: hostMethods,
	}
	b, err := p.Append(nil)
	require.NoError(t, err)

	return b
}

func hostedDispatcher(t *testing.T) string {
	t.Helper()

	src, err := goabi.RenderDispatcher(hostedType(), hostType(), nil)
	require.NoError(t, err)

	return string(src)
}

// loadHosted loads plugin with the Host stubs, answering its host calls with host.
func loadHosted(t *testing.T, plugin map[string]string, host *fakeHost) (*goguest.Guest, result) {
	t.Helper()

	guest := goguest.New(nil, nil, host.call)
	root := gopathWith(t, plugin, hostedDispatcher(t))
	res := guest.Init(send(guest, hostedPayload(t, root, names(hostType()))))

	return guest, parse(t, bytes.Clone(res))
}

// originHost answers Value (which the hosted plugin's Create calls) with "origin" and every other method
// with answer.
func originHost(t *testing.T, answer []byte) *fakeHost {
	t.Helper()

	value := index(t, hostType(), "Value")
	origin := okAnswer(t, "origin")

	return &fakeHost{answer: func(method uint32) []byte {
		if method == value {
			return origin
		}

		return answer
	}}
}

func callHosted(t *testing.T, guest *goguest.Guest, method string, args ...any) result {
	t.Helper()

	return parse(t, guest.Call(index(t, hostedType(), method), send(guest, encode(t, args...))))
}

//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestHostStubsCallTheHost(t *testing.T) {
	user := testcontract.User{
		ID: "ada", Name: "Ada", Tags: []string{"math"}, Since: time.Unix(1700000000, 5).UTC(),
		Manager: &testcontract.User{ID: "grace", Name: "Grace", Since: time.Unix(0, 0).UTC()},
	}
	host := originHost(t, okAnswer(t, user))

	guest, res := loadHosted(t, testdataPlugin(t, "hosted"), host)
	require.Equal(t, wire.StatusOK, res.status, res.msg)
	require.Len(t, host.calls, 1, "Create called the host")
	assert.Equal(t, index(t, hostType(), "Value"), host.calls[0].method)
	assert.Empty(t, host.calls[0].args, "the context does not cross")

	res = callHosted(t, guest, "Origin")
	require.Equal(t, wire.StatusOK, res.status, res.msg)
	assert.Equal(t, "origin", decode(t, res.body, "")[0], "Create got the host's answer")

	res = callHosted(t, guest, "Describe", "ada")
	require.Equal(t, wire.StatusOK, res.status, res.msg)

	want := user
	want.Name = "described Ada"
	assert.Equal(t, want, decode(t, res.body, testcontract.User{})[0], "the plugin got a typed struct")

	require.Len(t, host.calls, 2)
	assert.Equal(t, index(t, hostType(), "Lookup"), host.calls[1].method)
	assert.Equal(t, encode(t, "ada"), host.calls[1].args)

	host.answer = func(uint32) []byte { return okAnswer(t, 42) }
	res = callHosted(t, guest, "Total", 1, []int{2, 3})
	require.Equal(t, wire.StatusOK, res.status, res.msg)
	assert.Equal(t, 42, decode(t, res.body, 0)[0])
	assert.Equal(t, encode(t, 1, []int{2, 3}), host.calls[2].args, "the variadic parameter crosses as its slice")
}

//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestHostFailuresReachThePluginAsErrors(t *testing.T) {
	host := originHost(t, nil)
	guest, res := loadHosted(t, testdataPlugin(t, "hosted"), host)
	require.Equal(t, wire.StatusOK, res.status, res.msg)

	for _, status := range []wire.Status{wire.StatusError, wire.StatusPanic, wire.StatusConvert} {
		msg := "host says " + status.String()
		host.answer = func(uint32) []byte { return msgAnswer(status, msg) }

		res = callHosted(t, guest, "Relay", "x")
		require.Equal(t, wire.StatusOK, res.status, res.msg)
		assert.Equal(t, msg, decode(t, res.body, "")[0], "the plugin got an error with the host's message")
	}

	host.answer = func(uint32) []byte { return okAnswer(t) }
	res = callHosted(t, guest, "Relay", "")
	require.Equal(t, wire.StatusOK, res.status, res.msg)
	assert.Empty(t, decode(t, res.body, "")[0], "an OK answer is a nil error")

	host.answer = func(uint32) []byte { return msgAnswer(wire.StatusError, `no user "x"`) }
	res = callHosted(t, guest, "Describe", "x")
	assert.Equal(t, wire.StatusError, res.status, "the plugin returned the host's error")
	assert.Equal(t, `no user "x"`, res.msg)
}

//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestHostArgumentsThatCannotCrossNeverReachTheHost(t *testing.T) {
	plugin := testdataPlugin(t, "hosted")
	plugin["plugin.go"] = strings.Replace(plugin["plugin.go"], "return h.host.Sum(ctx, base, more...)",
		"return h.host.Sum(ctx, base<<20, more...)", 1)

	host := originHost(t, okAnswer(t, 0))
	guest, res := loadHosted(t, plugin, host)
	require.Equal(t, wire.StatusOK, res.status, res.msg)

	res = callHosted(t, guest, "Total", 1<<40, []int(nil))
	assert.Equal(t, wire.StatusError, res.status, "the plugin returned the error")
	assert.Equal(t, "plg: host method Sum: parameter 1: wire: integer beyond ±2^53: 1152921504606846976", res.msg)
	assert.Len(t, host.calls, 1, "only Create's call reached the host")
}

//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestMalformedHostAnswersDiscardTheInstance(t *testing.T) {
	tests := []struct {
		name   string
		msg    string
		answer []byte
	}{
		{"empty", "empty answer", nil},
		{"zero status", "invalid status 0", []byte{0}},
		{"unknown status", "invalid status 99", []byte{99}},
		{"truncated result", "result 0: Since: wire: truncated", []byte{byte(wire.StatusOK), 1}},
		{"trailing bytes", "wire: trailing", append(okAnswer(t, testcontract.User{}), 0)},
		{"truncated message", "error message: wire: truncated", []byte{byte(wire.StatusError), 9}},
		{"message with trailing bytes", "panic message: wire: trailing", append(msgAnswer(wire.StatusPanic, "m"), 0)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host := originHost(t, tc.answer)
			guest, res := loadHosted(t, testdataPlugin(t, "hosted"), host)
			require.Equal(t, wire.StatusOK, res.status, res.msg)

			res = callHosted(t, guest, "Describe", "ada")
			assert.Equal(t, wire.StatusABI, res.status, "the plugin's own result does not matter")
			assert.Contains(t, res.msg, "host method Lookup: malformed host answer: "+tc.msg)

			host.answer = func(uint32) []byte { return okAnswer(t, "fine") }
			res = callHosted(t, guest, "Value")
			assert.Equal(t, wire.StatusOK, res.status, "the fault does not outlive its call")
		})
	}

	// A malformed answer during Create fails the load the same way.
	host := &fakeHost{answer: func(uint32) []byte { return nil }}
	_, res := loadHosted(t, testdataPlugin(t, "hosted"), host)
	assert.Equal(t, wire.StatusABI, res.status)
	assert.Contains(t, res.msg, "host method Value: malformed host answer: empty answer")
}

//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestCreateMustTakeTheHost(t *testing.T) {
	hosted := testdataPlugin(t, "hosted")
	swap := func(from, to string) map[string]string {
		return map[string]string{"plugin.go": strings.Replace(hosted["plugin.go"], from, to, 1)}
	}

	const contractImport = `"github.com/heynemann/go-plg/internal/testcontract"`

	tests := []struct {
		plugin map[string]string
		name   string
		msg    string
		status wire.Status
	}{
		{hosted, "the Host", "", wire.StatusOK},
		{
			map[string]string{"plugin.go": strings.ReplaceAll(strings.Replace(hosted["plugin.go"], contractImport,
				"tc "+contractImport, 1), "testcontract.", "tc.")},
			"the Host through an aliased import", "", wire.StatusOK,
		},
		{
			map[string]string{"plugin.go": dotImported},
			"the Host through a dot import", "Create returned a nil testcontract.Hosted", wire.StatusLoad,
		},
		{
			swap("Create(host testcontract.Host) (testcontract.Hosted, error) {",
				"Create() (testcontract.Hosted, error) {\n\tvar host testcontract.Host\n"), "no parameter",
			"Create has type func() (interp.valueInterface, error); want func Create(testcontract.Host) " +
				"(<interface>, error) (Register passes plg.WithHost[testcontract.Host])",
			wire.StatusLoad,
		},
		{
			swap("Create(host testcontract.Host)", "Create(host testcontract.Hosted)"), "another interface",
			"plugin Create must take the Host interface testcontract.Host (from " + contractImport +
				"); it takes testcontract.Hosted",
			wire.StatusLoad,
		},
		{
			map[string]string{"plugin.go": strings.Replace(hosted["plugin.go"], "Create(host testcontract.Host)",
				"Create(host hostAlias)", 1) + "\ntype hostAlias = testcontract.Host\n"},
			"a local alias", "it takes hostAlias", wire.StatusLoad,
		},
		{
			swap("Create(host testcontract.Host)", "Create(host *testcontract.Host)"), "a pointer to the Host",
			"it takes *testcontract.Host", wire.StatusLoad,
		},
		{
			map[string]string{"plugin.go": "package hosted\n\nimport (\n\t\"context\"\n\n\t" + contractImport +
				"\n)\n\nfunc Create(host context.Context) (testcontract.Hosted, error) { return nil, nil }\n"},
			"another package's interface", "it takes context.Context", wire.StatusLoad,
		},
		{
			map[string]string{
				"plugin.go":      hosted["plugin.go"],
				"README.txt":     "not Go",
				"plugin_test.go": "package hosted\n\nfunc Create(int) {}\n",
				"method.go":      "package hosted\n\ntype maker struct{}\n\nfunc (maker) Create(int) {}\n",
				"otherimport.go": "package hosted\n\nimport _ \"strings\"\n",
			},
			"next to other files", "", wire.StatusOK,
		},
		{
			map[string]string{"plugin.go": "package hosted\n"},
			"no Create", "must declare Create: func Create(testcontract.Host)",
			wire.StatusLoad,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, res := loadHosted(t, tc.plugin, originHost(t, nil))
			assert.Equal(t, tc.status, res.status, res.msg)
			assert.Contains(t, res.msg, tc.msg)
		})
	}
}

// dotImported is the hosted plugin's Create through a dot import of the contract.
const dotImported = `package hosted

import (
	"context"

	. "github.com/heynemann/go-plg/internal/testcontract"
)

func Create(host Host) (Hosted, error) { return nil, nil }

var _ = context.Background
`

//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestCreateTakingAHostWithoutOne(t *testing.T) {
	root := gopathWith(t, testdataPlugin(t, "hosted"), hostedDispatcher(t))
	p := goabi.InitPayload{GoPath: root, Dispatch: goabi.DispatchImportPath, Methods: names(hostedType())}
	payload, err := p.Append(nil)
	require.NoError(t, err)

	guest := goguest.New(nil, nil, nil)
	res := parse(t, guest.Init(send(guest, payload)))
	assert.Equal(t, wire.StatusLoad, res.status)
	assert.Contains(t, res.msg, "plugin Create has the wrong type: Create has type func(interp.valueInterface) "+
		"(interp.valueInterface, error); want func Create() (<interface>, error) (Create takes a Host only when "+
		"Register passes plg.WithHost)")
}

//nolint:paralleltest // interprets testcontract: serial, see goguest_test.go
func TestRejectsHostStubsItDoesNotAgreeWith(t *testing.T) {
	stubs := hostedDispatcher(t)

	tests := []struct {
		name       string
		dispatcher string
		msg        string
		methods    []string
	}{
		{"missing variable", stubs, "resolving host method Missing", []string{"Missing"}},
		{
			"not a method type", stubs + "\nvar PlgHostCall_Odd func(int) error\n",
			"host method Odd has type func(int) error",
			[]string{"Odd"},
		},
		{
			"a function, not a variable", stubs + "\nfunc PlgHostCall_Fixed(p0.Context) error { return nil }\n",
			"host method Fixed has type func(context.Context) error",
			[]string{"Fixed"},
		},
		{
			"parameter the codec rejects", stubs + "\nvar PlgHostCall_Clash func(p0.Context, struct{ A int " +
				"`json:\"a\"`; B int `json:\"a\"` }) error\n",
			"host method Clash parameter 1",
			[]string{"Clash"},
		},
		{
			"result the codec rejects", stubs + "\nvar PlgHostCall_Clash func(p0.Context) (struct{ A int " +
				"`json:\"a\"`; B int `json:\"a\"` }, error)\n",
			"host method Clash result 0",
			[]string{"Clash"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := gopathWith(t, testdataPlugin(t, "hosted"), tc.dispatcher)
			guest := goguest.New(nil, nil, (&fakeHost{}).call)
			res := parse(t, guest.Init(send(guest, hostedPayload(t, root, tc.methods))))
			assert.Equal(t, wire.StatusABI, res.status, res.msg)
			assert.Contains(t, res.msg, tc.msg)
		})
	}
}
