package plg_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/wire"
)

// hostCtxKey carries the value testHost.Value reports.
type hostCtxKey struct{}

func withHostValue(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, hostCtxKey{}, v)
}

// testHost is the service's implementation of testcontract.Host.
type testHost struct {
	users map[string]testcontract.User
}

func newTestHost() *testHost {
	since := time.Date(2020, 1, 2, 3, 4, 5, 6, time.UTC)
	boss := testcontract.User{ID: "grace", Name: "Grace", Since: since.Add(-time.Hour)}

	return &testHost{users: map[string]testcontract.User{
		"ada": {ID: "ada", Name: "Ada", Tags: []string{"math", "engines"}, Since: since, Manager: &boss},
	}}
}

func (h *testHost) Lookup(_ context.Context, id string) (testcontract.User, error) {
	user, ok := h.users[id]
	if !ok {
		return testcontract.User{}, fmt.Errorf("%w %q", errNoUser, id)
	}

	return user, nil
}

func (*testHost) Value(ctx context.Context) (string, error) {
	v, ok := ctx.Value(hostCtxKey{}).(string)
	if !ok {
		return "", nil
	}

	return v, nil
}

func (*testHost) Fail(_ context.Context, msg string) error {
	switch msg {
	case "":
		return nil
	case "panic":
		panic("host panicked on purpose")
	default:
		return hostFailureError(msg)
	}
}

var errNoUser = errors.New("no user")

// hostFailureError is the error testHost.Fail returns: exactly its message.
type hostFailureError string

func (e hostFailureError) Error() string { return string(e) }

func (*testHost) Sum(_ context.Context, base int, more ...int) (int, error) {
	for _, n := range more {
		base += n
	}

	return base, nil
}

// Host interfaces for the validation tests. They are declared next to pinger, as a contract's Host must be.
type (
	pingHost interface {
		Note(ctx context.Context, msg string) error
	}
	hostNoCtx interface {
		Note(msg string) error
	}
	hostChan interface {
		Note(ctx context.Context) (chan int, error)
	}
	hostClash interface {
		Note(ctx context.Context, c clash) error
	}
	pingHostImpl  struct{}
	clashHostImpl struct{}
)

func (pingHostImpl) Note(context.Context, string) error { return nil }

func (clashHostImpl) Note(context.Context, clash) error { return nil }

func TestRegisterAcceptsAHostFromTheContractPackage(t *testing.T) {
	t.Parallel()

	ctr := newUnitContainer(t)
	err := plg.Register[pinger](t.Context(), ctr, "ping", emptyStorage(), plg.WithHost[pingHost](pingHostImpl{}))
	require.NoError(t, err)
}

func TestRegisterRejectsInvalidHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host  plg.RegisterOption
		name  string
		want  []string
		iface bool // also ErrInvalidInterface
	}{
		{
			plg.WithHost(42), "not an interface",
			[]string{"plg: invalid host: plg: invalid interface int: not an interface type"},
			true,
		},
		{
			plg.WithHost[interface {
				M(ctx context.Context) error
			}](nil), "unnamed interface",
			[]string{"not a named interface type"},
			true,
		},
		{
			plg.WithHost[hostNoCtx](nil), "method without a context",
			[]string{"plg_test.hostNoCtx: method Note: first parameter must be context.Context"},
			true,
		},
		{
			plg.WithHost[hostChan](nil), "result that is not data",
			[]string{"method Note: result 0 has type chan int: chan int is not a data type (kind chan)"},
			true,
		},
		{
			plg.WithHost[pingHost](nil), "nil implementation",
			[]string{"plg: invalid host plg_test.pingHost: nil implementation"},
			false,
		},
		{
			plg.WithHost[hostClash](clashHostImpl{}), "type the codec rejects",
			[]string{"plg: invalid host plg_test.hostClash: method Note: parameter 1: ", `"name"`},
			false,
		},
		{
			plg.WithHost[testcontract.Host](newTestHost()), "declared in another package",
			[]string{
				"plg: invalid host testcontract.Host must be declared in the contract package " +
					"github.com/heynemann/go-plg_test of plg_test.pinger, whose source Go plugins interpret; it is in " +
					"github.com/heynemann/go-plg/internal/testcontract",
			},
			false,
		},
	}

	ctr := newUnitContainer(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := plg.Register[pinger](t.Context(), ctr, "ping", emptyStorage(), tc.host)
			require.ErrorIs(t, err, plg.ErrInvalidHost)
			assert.Equal(t, tc.iface, errors.Is(err, plg.ErrInvalidInterface))

			for _, want := range tc.want {
				require.ErrorContains(t, err, want)
			}
		})
	}

	err := plg.Register[pinger](t.Context(), ctr, "ping", emptyStorage(), plg.WithHost[pingHost](pingHostImpl{}),
		plg.WithHost[pingHost](&pingHostImpl{}))
	require.ErrorIs(t, err, plg.ErrInvalidHost)
	require.ErrorContains(t, err, "WithHost given more than once")
}

// hostMethod returns the index of the named Host method, the index host_call carries.
func hostMethod(tb testing.TB, name string) uint32 {
	tb.Helper()

	m, ok := reflect.TypeFor[testcontract.Host]().MethodByName(name)
	require.True(tb, ok, name)

	return uint32(m.Index) //nolint:gosec // G115: a handful of methods
}

func encodeAll(tb testing.TB, values ...any) []byte {
	tb.Helper()

	var buf []byte

	for _, v := range values {
		codec, err := wire.For(reflect.TypeOf(v))
		require.NoError(tb, err)

		buf, err = codec.Append(buf, reflect.ValueOf(v))
		require.NoError(tb, err)
	}

	return buf
}

func message(status wire.Status, msg string) []byte {
	return wire.AppendMessage([]byte{byte(status)}, msg)
}

func TestHostDispatch(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	handler, err := plg.HostHandler(reflect.TypeFor[testcontract.Hosted](),
		plg.WithHost[testcontract.Host](newTestHost()), "hosted/ref", slog.New(slog.NewJSONHandler(&logs, nil)))
	require.NoError(t, err)

	ok := func(values ...any) []byte { return append([]byte{byte(wire.StatusOK)}, encodeAll(t, values...)...) }

	tests := []struct {
		ctx    context.Context //nolint:containedctx // the context each case calls the handler with
		name   string
		method string
		args   []byte
		want   []byte
	}{
		{t.Context(), "struct result", "Lookup", encodeAll(t, "ada"), ok(newTestHost().users["ada"])},
		{t.Context(), "error", "Lookup", encodeAll(t, "bob"), message(wire.StatusError, `no user "bob"`)},
		{withHostValue(t.Context(), "seen"), "the call's context", "Value", nil, ok("seen")},
		{t.Context(), "nil error", "Fail", encodeAll(t, ""), ok()},
		{t.Context(), "variadic", "Sum", encodeAll(t, 1, []int{2, 3}), ok(6)},
		{t.Context(), "no variadic values", "Sum", encodeAll(t, 1, []int(nil)), ok(1)},
		{
			t.Context(), "panic", "Fail", encodeAll(t, "panic"), message(wire.StatusPanic,
				"plg: host method panicked: testcontract.Host.Fail: host panicked on purpose"),
		},
		{
			t.Context(), "result that cannot cross", "Sum", encodeAll(t, 1<<53, []int{1}),
			message(wire.StatusConvert, "plg: value cannot cross the plugin boundary: host method "+
				"testcontract.Host.Sum: result 0: wire: integer beyond ±2^53: 9007199254740993"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The answer is appended after what dst holds, even when the method fails half-way.
			got, err := handler(tc.ctx, []byte("kept"), hostMethod(t, tc.method), tc.args)
			require.NoError(t, err)
			assert.Equal(t, append([]byte("kept"), tc.want...), got)
		})
	}

	t.Run("the panic is logged", func(t *testing.T) {
		_, err := handler(t.Context(), nil, hostMethod(t, "Fail"), encodeAll(t, "panic"))
		require.NoError(t, err)

		logged := logs.String()
		assert.Contains(t, logged, `"level":"ERROR","msg":"plg: host method panicked","key":"hosted/ref",`+
			`"method":"testcontract.Host.Fail","panic":"host panicked on purpose","stack":"goroutine `)
		assert.Contains(t, logged, "(*testHost).Fail")
	})
}

func TestHostDispatchRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	handler, err := plg.HostHandler(reflect.TypeFor[testcontract.Hosted](),
		plg.WithHost[testcontract.Host](newTestHost()), "hosted/ref", slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	tests := []struct {
		name   string
		msg    string
		args   []byte
		method uint32
	}{
		{"unknown method", "unknown host method 4 of testcontract.Host", nil, 4},
		{
			"truncated arguments", "malformed host call arguments: testcontract.Host.Lookup: parameter 1: wire:",
			[]byte{1, 0},
			hostMethod(t, "Lookup"),
		},
		{
			"trailing bytes", "malformed host call arguments: testcontract.Host.Value: arguments: wire: trailing",
			[]byte{0},
			hostMethod(t, "Value"),
		},
	}

	for _, tc := range tests {
		got, err := handler(t.Context(), []byte("kept"), tc.method, tc.args)
		require.ErrorContains(t, err, tc.msg, tc.name)
		assert.Equal(t, []byte("kept"), got, tc.name)
	}
}

// BenchmarkHostDispatch is the host side of one host call: decoding the arguments, calling the Host method by
// reflection and encoding its struct result.
func BenchmarkHostDispatch(b *testing.B) {
	handler, err := plg.HostHandler(reflect.TypeFor[testcontract.Hosted](),
		plg.WithHost[testcontract.Host](newTestHost()), "hosted/ref", slog.New(slog.DiscardHandler))
	require.NoError(b, err)

	ctx, method, args := b.Context(), hostMethod(b, "Lookup"), encodeAll(b, "ada")

	var dst []byte

	b.ReportAllocs()

	for b.Loop() {
		dst, err = handler(ctx, dst[:0], method, args)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHostDispatchParallel(b *testing.B) {
	handler, err := plg.HostHandler(reflect.TypeFor[testcontract.Hosted](),
		plg.WithHost[testcontract.Host](newTestHost()), "hosted/ref", slog.New(slog.DiscardHandler))
	require.NoError(b, err)

	ctx, method, args := b.Context(), hostMethod(b, "Lookup"), encodeAll(b, "ada")

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var dst []byte

		for pb.Next() {
			var err error

			dst, err = handler(ctx, dst[:0], method, args)
			if err != nil {
				b.Error(err)

				return
			}
		}
	})
}
