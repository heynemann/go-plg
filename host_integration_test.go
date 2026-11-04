//go:build integration

package plg_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/internal/wire"
)

// registerHosted registers lang's hosted test plugin as "hosted/ref", with Register's context carrying
// "register".
func registerHosted(tb testing.TB, lang contractLanguage, ctr *plg.Container) {
	tb.Helper()

	err := plg.Register[testcontract.Hosted](withHostValue(tb.Context(), "register"), lang.binder(ctr), "hosted",
		plg.WithStorage(lang.storage(tb, "hosted", map[string]string{"ref": "hosted"})),
		plg.WithHost[testcontract.Host](newTestHost()))
	require.NoError(tb, err)
}

// hostedPlugin registers lang's hosted test plugin in ctr and returns it.
//
//nolint:ireturn // the plugin is served as the contract interface under test
func hostedPlugin(tb testing.TB, lang contractLanguage, ctr *plg.Container) testcontract.Hosted {
	tb.Helper()

	registerHosted(tb, lang, ctr)

	hosted, err := plg.Get[testcontract.Hosted](tb.Context(), ctr, "hosted/ref")
	require.NoError(tb, err)

	return hosted
}

func TestHostMethodsReturnTypedResults(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		hosted := hostedPlugin(t, lang, newContainer(t))

		user, err := hosted.Describe(t.Context(), "ada")
		require.NoError(t, err)

		want := newTestHost().users["ada"]
		want.Name = "described Ada"
		assert.Equal(t, want, user, "the struct crossed host -> guest -> host, nested pointer, slice and time included")

		tests := []struct {
			more []int
			base int
			want int
		}{
			{base: 1, more: []int{2, 3}, want: 6},
			{base: 7, want: 7},
			{base: -maxSafeInt, more: []int{maxSafeInt, maxSafeInt}, want: maxSafeInt},
		}

		for _, tc := range tests {
			got, err := hosted.Total(t.Context(), tc.base, tc.more...)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got, "variadic host method with %d, %v", tc.base, tc.more)
		}
	})
}

// TestMapsOfValuesThatEncodeAsNothingCross holds every language to decoding a map whose values encode as nothing
// (struct{}), so an entry with an empty key is a bare 4-byte length prefix: as a plugin's argument and result and
// as a host method's argument and result. A map claiming more entries than its input holds key prefixes for is
// still rejected as truncated, however little each value needs.
func TestMapsOfValuesThatEncodeAsNothingCross(t *testing.T) {
	t.Parallel()

	keys, ok := reflect.TypeFor[testcontract.Hosted]().MethodByName("Keys")
	require.True(t, ok)

	method := uint32(keys.Index) //nolint:gosec // G115: a method index is far below 2^32

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		ctr := newContainer(t)
		hosted := hostedPlugin(t, lang, ctr)

		for _, keys := range []map[string]struct{}{{"": {}}, {"": {}, "a": {}, "bc": {}}, {}} {
			got, err := hosted.Keys(t.Context(), keys)
			require.NoError(t, err)
			assert.Equal(t, keys, got)
		}

		for _, args := range [][]byte{
			{1, 0, 0, 0},                  // one entry, no key
			{2, 0, 0, 0, 0, 0, 0, 0},      // two entries, one key
			{1, 0, 0, 0, 2, 0, 0, 0, 'a'}, // a key cut short
		} {
			res, err := plg.CallRaw(t.Context(), ctr, "hosted/ref", method, args)
			require.NoError(t, err)
			assert.Equal(t, wire.StatusABI, res.Status, "%v", args)
			assert.NotEmpty(t, res.Message, "%v", args)
		}
	})
}

func TestHostMethodsSeeTheContextOfTheCallInProgress(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		hosted := hostedPlugin(t, lang, newContainer(t))

		for _, v := range []string{"first call", "second call", ""} {
			got, err := hosted.Value(withHostValue(t.Context(), v))
			require.NoError(t, err)
			assert.Equal(t, v, got)
		}

		origin, err := hosted.Origin(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "register", origin, "while Create runs, host calls see Register's context")
	})
}

func TestHostErrorsReachThePluginAsErrors(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		hosted := hostedPlugin(t, lang, newContainer(t))

		seen, err := hosted.Relay(t.Context(), "quota exceeded")
		require.NoError(t, err, "the plugin handled the host's error itself")
		assert.Equal(t, "quota exceeded", seen, "the plugin got an error with the host's exact message")

		seen, err = hosted.Relay(t.Context(), "")
		require.NoError(t, err)
		assert.Empty(t, seen, "a nil error stays nil")

		_, err = hosted.Describe(t.Context(), "nobody")

		var pluginErr *plg.PluginError

		require.ErrorAs(t, err, &pluginErr, "the plugin returned the host's error")
		assert.Equal(t, `no user "nobody"`, pluginErr.Message)
		require.NotErrorIs(t, err, plg.ErrCall, "an error from the host is the plugin's to handle")

		// A result the host cannot encode is an error for the plugin too, naming the method and result.
		_, err = hosted.Total(t.Context(), maxSafeInt, 1)
		require.ErrorAs(t, err, &pluginErr)
		assert.Contains(t, pluginErr.Message,
			"host method testcontract.Host.Sum: result 0: wire: integer beyond ±2^53")
	})
}

func TestHostPanicReachesThePluginAsAnError(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		var logs bytes.Buffer

		hosted := hostedPlugin(t, lang, loggedContainer(t, &logs))

		seen, err := hosted.Relay(t.Context(), "panic")
		require.NoError(t, err, "the plugin handled the error the panic became")
		assert.Equal(t, "plg: hosted/ref: host testcontract.Host.Fail failed: host panicked on purpose", seen,
			"the plugin received the PanicError's message")

		origin, err := hosted.Origin(withHostValue(t.Context(), "later"))
		require.NoError(t, err)
		assert.Equal(t, "register", origin, "the same instance keeps serving: a host panic does not discard it")

		user, err := hosted.Describe(t.Context(), "ada")
		require.NoError(t, err, "the host keeps serving")
		assert.Equal(t, "described Ada", user.Name)

		logged := logs.String()
		assert.Contains(t, logged, `"level":"ERROR","msg":"plg: plugin failure","key":"hosted/ref","phase":"host",`+
			`"value":"host panicked on purpose","language":"`+string(lang.lang)+`","method":"testcontract.Host.Fail"`)
		assert.Contains(t, logged, "(*testHost).Fail", "the host stack is logged")
	})
}

func TestCreateMustMatchWithHost(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	impl := plg.WithHost[testcontract.Host](newTestHost())

	err := plg.Register[testcontract.Hosted](t.Context(), ctr, "nohost",
		plg.WithStorage(goStorage(t, "nohost", map[string]string{"ref": "hosted"})))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, "nohost/ref")
	require.ErrorContains(t, err, "Create has the wrong type: Create has type func(")
	require.ErrorContains(t, err, "want func Create() (<interface>, error) (Create takes a Host only when "+
		"Register passes plg.WithHost)")

	err = plg.Register[testcontract.Fooer](t.Context(), ctr, "noparam",
		plg.WithStorage(goStorage(t, "noparam", map[string]string{"ref": "fooer"})), impl)
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, "want func Create(testcontract.Host) (<interface>, error) (Register passes "+
		"plg.WithHost[testcontract.Host])")

	wrongParam := strings.Replace(hostedSource(t), "Create(host testcontract.Host)",
		"Create(host testcontract.Hosted)", 1)
	store := plg.NewMemoryStorage("wrongparam")
	require.NoError(t, store.Put(plg.Descriptor{Key: "ref", Language: plg.LanguageGo, Entrypoint: "."},
		fstest.MapFS{"plugin.go": {Data: []byte(wrongParam)}}))

	err = plg.Register[testcontract.Hosted](t.Context(), ctr, "wrongparam", plg.WithStorage(store), impl)
	require.ErrorIs(t, err, plg.ErrLoad, "a Create taking another interface is rejected, though yaegi would run it")
	require.ErrorContains(t, err, "wrongparam/ref")
	require.ErrorContains(t, err, "plugin Create must take the Host interface testcontract.Host (from "+
		`"github.com/heynemann/go-plg/internal/testcontract"); it takes testcontract.Hosted`)
}

func hostedSource(t *testing.T) string {
	t.Helper()

	src, err := fs.ReadFile(pluginFiles("hosted"), "plugin.go")
	require.NoError(t, err)

	return string(src)
}

func TestHostServesConcurrentPlugins(t *testing.T) {
	t.Parallel()

	eachLanguage(t, func(t *testing.T, lang contractLanguage) {
		t.Helper()

		ctr := lang.binder(newContainer(t))
		err := plg.Register[testcontract.Hosted](t.Context(), ctr, "hosted",
			plg.WithStorage(lang.storage(t, "hosted", map[string]string{"a": "hosted", "b": "hosted", "c": "hosted"})),
			plg.WithHost[testcontract.Host](newTestHost()))
		require.NoError(t, err)

		errs := make(chan error, 3*4)

		for key, hosted := range plg.All[testcontract.Hosted](t.Context(), ctr) {
			for i := range 4 {
				go func() {
					want := fmt.Sprintf("%s#%d", key, i)

					got, err := hosted.Value(withHostValue(t.Context(), want))
					if err == nil && got != want {
						err = fmt.Errorf("%w: %s: got %q", errWrongValue, want, got)
					}

					errs <- err
				}()
			}
		}

		for range cap(errs) {
			require.NoError(t, <-errs, "each call's host methods see that call's context")
		}
	})
}

var errWrongValue = errors.New("wrong value")

// BenchmarkCallGoHost is one call into a Go plugin making one host call (Value), every protection on; compare
// with BenchmarkCallGo for the host call's round trip.
func BenchmarkCallGoHost(b *testing.B) {
	benchHostValue(b, golang())
}

// BenchmarkCallGoHostStruct is one call making one host call returning a struct (Describe).
func BenchmarkCallGoHostStruct(b *testing.B) {
	benchHostStruct(b, golang())
}

// BenchmarkCallGoHostParallel contends for the plugin's single instance.
func BenchmarkCallGoHostParallel(b *testing.B) {
	benchHostValueParallel(b, golang())
}

func benchHostValue(b *testing.B, lang contractLanguage) {
	b.Helper()

	hosted := hostedPlugin(b, lang, newContainer(b))
	ctx := withHostValue(b.Context(), "bench")

	b.ReportAllocs()

	for b.Loop() {
		_, err := hosted.Value(ctx)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchHostStruct(b *testing.B, lang contractLanguage) {
	b.Helper()

	hosted := hostedPlugin(b, lang, newContainer(b))
	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		_, err := hosted.Describe(ctx, "ada")
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchHostValueParallel(b *testing.B, lang contractLanguage) {
	b.Helper()

	hosted := hostedPlugin(b, lang, newContainer(b))
	ctx := withHostValue(b.Context(), "bench")

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := hosted.Value(ctx)
			if err != nil {
				b.Error(err)

				return
			}
		}
	})
}
