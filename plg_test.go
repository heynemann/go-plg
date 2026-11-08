package plg_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/layout"
	"github.com/heynemann/go-plg/internal/testcontract"
	"github.com/heynemann/go-plg/plgimpl"
)

func TestMain(m *testing.M) {
	if os.Getenv(plg.GlobalTracingChild) != "" { // a process of TestTracingFollowsTheGlobalProvider's own
		m.Run()

		return
	}

	for _, contract := range []plg.Contract{pingerContract(), fastPingerContract()} {
		err := plg.RegisterContract(contract)
		if err != nil {
			panic(err)
		}
	}

	err := prepareGuests()
	if err != nil {
		panic(err)
	}

	goleak.VerifyTestMain(m)
}

// pinger is a contract registered by these tests, so Register runs without any plugin or guest.
type pinger interface {
	Ping(ctx context.Context, msg string) (string, error)
}

type pingerAdapter struct {
	WPing func(ctx context.Context, msg string) (string, error)
}

func (a *pingerAdapter) Ping(ctx context.Context, msg string) (string, error) {
	return a.WPing(ctx, msg)
}

func pingerContract() plg.Contract {
	return plg.Contract{
		Interface: reflect.TypeFor[pinger](),
		Adapter:   reflect.TypeFor[pingerAdapter](),
		Source:    fstest.MapFS{"pinger.go": {Data: []byte("package plg_test\n")}},
	}
}

// clashIn takes and clashOut returns a struct whose fields share a name for other guest languages: the
// interface contract accepts it, the codec does not.
type (
	clash struct {
		A string `json:"name"`
		B string `json:"name,omitempty"` //nolint:revive,govet // struct-tag, structtag: the duplicate is the case under test
	}
	clashIn interface {
		Ping(ctx context.Context, c clash) error
	}
	clashOut interface {
		Ping(ctx context.Context) ([]clash, error)
	}
)

type (
	clashInAdapter struct {
		WPing func(context.Context, clash) error
	}
	clashOutAdapter struct {
		WPing func(context.Context) ([]clash, error)
	}
)

func (a *clashInAdapter) Ping(ctx context.Context, c clash) error    { return a.WPing(ctx, c) }
func (a *clashOutAdapter) Ping(ctx context.Context) ([]clash, error) { return a.WPing(ctx) }

// aliasClash's anonymous structs embed different types under the alias name Embedded, and hostReservedAlias's
// an alias named as the dispatcher's PlgClose: the Go dispatcher, which declares an alias of each such name to
// keep the fields' names, cannot render them.
type (
	aliasTarget struct{ ID string }
	Embedded    = aliasTarget
	PlgClose    = aliasTarget
	counterIn   = struct{ testcontract.Embedded }
	targetIn    = struct{ *Embedded }
	aliasClash  interface {
		Ping(ctx context.Context, a counterIn, b targetIn) error
	}
	aliasClashAdapter struct {
		WPing func(context.Context, counterIn, targetIn) error
	}
)

func (a *aliasClashAdapter) Ping(ctx context.Context, x counterIn, y targetIn) error {
	return a.WPing(ctx, x, y)
}

var errStorage = errors.New("storage unavailable")

// failingStorage is a memory storage whose List or Open fails.
type failingStorage struct {
	*plg.MemoryStorage

	failList, failOpen bool
}

func (s *failingStorage) List(ctx context.Context) ([]plg.Descriptor, error) {
	if s.failList {
		return nil, errStorage
	}

	descs, err := s.MemoryStorage.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing: %w", err)
	}

	return descs, nil
}

func (s *failingStorage) Open(ctx context.Context, desc *plg.Descriptor) (fs.FS, error) {
	if s.failOpen {
		return nil, errStorage
	}

	files, err := s.MemoryStorage.Open(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	return files, nil
}

// listStorage lists fixed descriptors and opens every plugin as files (failing when it is nil).
type listStorage struct {
	files fs.FS
	name  string
	descs []plg.Descriptor
}

func (s listStorage) Name() string                                   { return s.name }
func (s listStorage) List(context.Context) ([]plg.Descriptor, error) { return s.descs, nil }

func (s listStorage) Open(context.Context, *plg.Descriptor) (fs.FS, error) {
	if s.files == nil {
		return nil, errStorage
	}

	return s.files, nil
}

// closingStorage closes its container when listed, as a Close racing a Register would.
type closingStorage struct {
	ctr *plg.Container
}

func (closingStorage) Name() string { return "closing" }

func (s closingStorage) List(ctx context.Context) ([]plg.Descriptor, error) {
	err := s.ctr.Close(ctx)
	if err != nil {
		return nil, fmt.Errorf("closing: %w", err)
	}

	return nil, nil
}

func (closingStorage) Open(context.Context, *plg.Descriptor) (fs.FS, error) {
	return nil, errStorage
}

// brokenFS fails every read.
type brokenFS struct{}

func (brokenFS) Open(string) (fs.File, error) { return nil, errStorage }

// unreadableFS is files, except that opening the file named broken fails.
type unreadableFS struct {
	files  fstest.MapFS
	broken string
}

func (f unreadableFS) Open(name string) (fs.File, error) {
	if name == f.broken {
		return nil, errStorage
	}

	file, err := f.files.Open(name)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}

	return file, nil
}

func newUnitContainer(t *testing.T) *plg.Container {
	t.Helper()

	ctr := plg.New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(context.Background())) })

	return ctr
}

func emptyStorage() plg.RegisterOption {
	return plg.WithStorage(plg.NewMemoryStorage("empty"))
}

// implementsOf is what the test plugins of the interface T declare they implement ([plg.Descriptor.Implements]):
// T's package and name, at v1.0.0. The test binary's contract modules are its main module, version (devel).
func implementsOf[T any]() string {
	iface := reflect.TypeFor[T]()

	return iface.PkgPath() + "." + iface.Name() + "@v1.0.0"
}

// manifestOf is a plugin.yaml of a plugin implementing T, with the given keys after its implements line.
func manifestOf[T any](keys string) string {
	return "implements: " + implementsOf[T]() + "\n" + keys
}

// goPluginStorage holds one Go plugin, "k".
func goPluginStorage(t *testing.T) *plg.MemoryStorage {
	t.Helper()

	store := plg.NewMemoryStorage("go")
	files := fstest.MapFS{"plugin.go": {Data: []byte("package p\n")}}
	require.NoError(t, store.Put(&plg.Descriptor{
		Key: "k", Implements: implementsOf[pinger](), Language: plg.LanguageGo, Entrypoint: "plugin.go",
	}, files))

	return store
}

// brokenMeters is a meter provider whose counters and histograms panic on every measurement once broken is set:
// a provider that starts failing while the container runs.
type brokenMeters struct {
	noop.MeterProvider

	broken *atomic.Bool
}

//nolint:ireturn // the provider returns the API's Meter interface
func (p brokenMeters) Meter(string, ...metric.MeterOption) metric.Meter {
	return brokenMeter{broken: p.broken}
}

type brokenMeter struct {
	noop.Meter

	broken *atomic.Bool
}

//nolint:ireturn // the meter returns the API's instrument interface
func (m brokenMeter) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return brokenCounter{broken: m.broken}, nil
}

//nolint:ireturn // the meter returns the API's instrument interface
func (m brokenMeter) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return brokenHistogram{broken: m.broken}, nil
}

type brokenCounter struct {
	noop.Int64Counter

	broken *atomic.Bool
}

func (c brokenCounter) Add(context.Context, int64, ...metric.AddOption) {
	if c.broken.Load() {
		panic("meter bug")
	}
}

type brokenHistogram struct {
	noop.Float64Histogram

	broken *atomic.Bool
}

func (h brokenHistogram) Record(context.Context, float64, ...metric.RecordOption) {
	if h.broken.Load() {
		panic("meter bug")
	}
}

// meterPanicKeys checks that every PanicError of panics is a broken meter's panic, contained in PhaseCallback,
// and returns their keys in order.
func meterPanicKeys(t *testing.T, panics []*plg.PanicError) []string {
	t.Helper()

	keys := make([]string, 0, len(panics))

	for _, pe := range panics {
		assert.Equal(t, plg.PhaseCallback, pe.Phase, pe)
		assert.Equal(t, "metric.MeterProvider", pe.Method, pe)
		assert.Equal(t, "meter bug", pe.Value, pe)

		keys = append(keys, pe.Key)
	}

	return keys
}

// Interfaces breaking the interface contract.
type (
	noMethods   any
	noParams    interface{ M() error }
	ctxNotFirst interface {
		M(n int, ctx context.Context) error
	}
	noResults    interface{ M(ctx context.Context) }
	errorNotLast interface {
		M(ctx context.Context) (error, int)
	}
	funcParam interface {
		M(ctx context.Context, f func()) error
	}
	chanResult interface {
		M(ctx context.Context) (chan int, error)
	}
	anyParam interface {
		M(ctx context.Context, v any) error
	}
	errorParam interface {
		M(ctx context.Context, e error) error
	}
	secondCtx interface {
		M(ctx, other context.Context) error
	}
	nestedFunc interface {
		M(ctx context.Context, v []map[string]func()) error
	}
	intKeys interface {
		M(ctx context.Context, v map[int]string) error
	}
	intPointer interface {
		M(ctx context.Context, v *int) error
	}
	privateField interface {
		M(ctx context.Context, v struct{ x int }) error
	}
	deepPrivate interface {
		M(ctx context.Context, v []*deepData) error
	}
	complexParam interface {
		M(ctx context.Context, c complex128) error
	}
	arrayParam interface {
		M(ctx context.Context, a [4]int) error
	}
	uintptrParam interface {
		M(ctx context.Context, p uintptr) error
	}
	unexportedMeth interface {
		m(ctx context.Context) error
	}
	generic[T any] interface {
		M(ctx context.Context, v T) error
	}

	deepData struct{ Inner strings.Builder } // strings.Builder has unexported fields
)

func TestRegisterRejectsInterfacesBreakingTheContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		register func(context.Context, *plg.Container) error
		name     string
		want     []string // the method and the offending type
	}{
		{reg[int], "not an interface", []string{"int", "not an interface"}},
		{reg[*pinger], "pointer to interface", []string{"*plg_test.pinger", "not an interface"}},
		{reg[interface {
			M(ctx context.Context) error
		}], "unnamed", []string{"not a named interface"}},
		{reg[error], "predeclared", []string{"error", "not a named interface"}},
		{reg[generic[int]], "generic", []string{"generic"}},
		{reg[noMethods], "no methods", []string{"plg_test.noMethods", "no methods"}},
		{reg[unexportedMeth], "unexported method", []string{"method m", "unexported"}},
		{reg[noParams], "no params", []string{"method M", "context.Context"}},
		{reg[ctxNotFirst], "ctx not first", []string{"method M", "first parameter must be context.Context"}},
		{reg[noResults], "no results", []string{"method M", "last result must be error"}},
		{reg[errorNotLast], "error not last", []string{"method M", "last result must be error"}},
		{reg[funcParam], "func", []string{"method M", "parameter 1", "func() is not a data type (kind func)"}},
		{reg[chanResult], "chan", []string{"method M", "result 0", "chan int is not a data type (kind chan)"}},
		{reg[anyParam], "interface", []string{"method M", "parameter 1", "interface {} is not a data type"}},
		{reg[errorParam], "error param", []string{"method M", "parameter 1", "error is not a data type"}},
		{reg[secondCtx], "second ctx", []string{"method M", "parameter 1", "context.Context is not a data type"}},
		{reg[nestedFunc], "nested func", []string{"method M", "[]map[string]func()", "func() is not a data type"}},
		{reg[intKeys], "int keys", []string{"method M", "map[int]string", "map keys must be strings, not int"}},
		{reg[intPointer], "pointer to int", []string{"method M", "*int is not a data type", "point to structs"}},
		{reg[privateField], "unexported field", []string{"method M", "struct { x int }", "unexported field x"}},
		{reg[deepPrivate], "deep unexported field", []string{
			"method M", "[]*plg_test.deepData", "field plg_test.deepData.Inner", "strings.Builder is not a data type",
		}},
		{reg[complexParam], "complex", []string{"method M", "complex128 is not a data type"}},
		{reg[arrayParam], "array", []string{"method M", "[4]int is not a data type (kind array)"}},
		{reg[uintptrParam], "uintptr", []string{"method M", "uintptr is not a data type"}},
	}

	ctr := newUnitContainer(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.register(t.Context(), ctr)
			require.ErrorIs(t, err, plg.ErrInvalidInterface)

			for _, want := range tc.want {
				require.ErrorContains(t, err, want)
			}
		})
	}
}

func reg[T any](ctx context.Context, ctr *plg.Container) error {
	return plg.Register[T](ctx, ctr, "x", emptyStorage())
}

// Data types the interface contract accepts, including recursive structs, slices and maps.
type (
	node struct {
		Next     *node
		Name     string
		Children []node
	}
	nested   []nested
	tree     map[string]tree
	label    string
	celsius  float64
	allTypes interface {
		Scalars(ctx context.Context, b bool, i int8, u uint64, f float32, s string, c celsius) error
		Composite(ctx context.Context, b []byte, m map[string][]int, n *node, t time.Time, tp *time.Time) (
			map[label]node, []*node, error)
		Recursive(ctx context.Context, n nested) (tree, error)
		Nothing(ctx context.Context) error
	}
)

func TestRegisterAcceptsDataTypes(t *testing.T) {
	t.Parallel()

	err := plg.Register[allTypes](t.Context(), newUnitContainer(t), "all", emptyStorage())
	require.ErrorIs(t, err, plg.ErrNoContract, "validation passed; only the contract is missing")
	require.NotErrorIs(t, err, plg.ErrInvalidInterface)
	require.ErrorContains(t, err, "plg_test.allTypes")
	require.ErrorContains(t, err, "plg gen allTypes", "it tells how to fix it")
	require.ErrorContains(t, err, "plg.RegisterContract")
}

func TestRegisterArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want error
		name string
		msg  string
		opts []plg.RegisterOption
	}{
		{name: "", opts: []plg.RegisterOption{emptyStorage()}, want: plg.ErrInvalidName},
		{name: "a/b", opts: []plg.RegisterOption{emptyStorage()}, want: plg.ErrInvalidName},
		{name: "none", want: plg.ErrNoStorage},
		{name: "nil", opts: []plg.RegisterOption{emptyStorage(), plg.WithStorage(nil)}, want: plg.ErrNoStorage},
		{
			name: "list", want: plg.ErrLoad, msg: "listing storage broken",
			opts: []plg.RegisterOption{plg.WithStorage(&failingStorage{
				MemoryStorage: plg.NewMemoryStorage("broken"), failList: true,
			})},
		},
		{
			name: "nokey", want: plg.ErrInvalidDescriptor, msg: "storage s",
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{
				{Implements: implementsOf[pinger](), Language: plg.LanguageGo, Entrypoint: "p.go"},
			}})},
		},
		{
			name: "noentry", want: plg.ErrInvalidDescriptor, msg: "storage s",
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{
				{Key: "k", Implements: implementsOf[pinger](), Language: plg.LanguageGo},
			}})},
		},
		{
			name: "noimplements", want: plg.ErrInvalidDescriptor, msg: `storage s lists "k": implements is required`,
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{
				{Key: "k", Language: plg.LanguageGo, Entrypoint: "p.go"},
			}})},
		},
		{
			name: "badimplements", want: plg.ErrInvalidDescriptor,
			msg: `storage s lists "k": implements "example.com/c.Pinger@1.0.0": the version must be a canonical`,
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{
				{Key: "k", Implements: "example.com/c.Pinger@1.0.0", Language: plg.LanguageGo, Entrypoint: "p.go"},
			}})},
		},
		{
			name: "dupkey", want: plg.ErrDuplicateKey, msg: `"k": listed by storages one and two`,
			opts: []plg.RegisterOption{
				plg.WithStorage(listStorage{name: "one", descs: []plg.Descriptor{
					{Key: "k", Implements: implementsOf[pinger](), Entrypoint: "a"},
				}}),
				plg.WithStorage(listStorage{name: "two", descs: []plg.Descriptor{
					{Key: "k", Implements: "example.com/other.Iface@v2.0.0", Entrypoint: "b"},
				}}),
			},
		},
		{
			name: "nohosts", want: plg.ErrInvalidHTTPPolicy, msg: "AllowedHosts is required",
			opts: []plg.RegisterOption{emptyStorage(), plg.WithHTTP(plg.HTTPPolicy{})},
		},
		{
			name: "badhost", want: plg.ErrInvalidHTTPPolicy, msg: `"https://api.example.com"`,
			opts: []plg.RegisterOption{emptyStorage(), plg.WithHTTP(plg.HTTPPolicy{
				AllowedHosts: []string{"https://api.example.com"},
			})},
		},
		{
			name: "badlimit", want: plg.ErrInvalidHTTPPolicy, msg: "MaxResponseBytes",
			opts: []plg.RegisterOption{emptyStorage(), plg.WithHTTP(plg.HTTPPolicy{
				AllowedHosts: []string{"api.example.com"}, MaxResponseBytes: 2 << 30,
			})},
		},
		{
			name: "language", want: plg.ErrUnsupportedLanguage, msg: `plugin k (storage s)`,
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{
				{Key: "k", Implements: implementsOf[pinger](), Language: "cobol", Entrypoint: "main.cbl"},
			}})},
		},
		{name: "pool-empty", opts: withEmpty(plg.WithPool(0, 1)), want: plg.ErrInvalidOption, msg: "WithPool(0, 1)"},
		{name: "pool-inverted", opts: withEmpty(plg.WithPool(3, 2)), want: plg.ErrInvalidOption, msg: "(3, 2)"},
		{name: "pool-huge", opts: withEmpty(plg.WithPool(1, 4097)), want: plg.ErrInvalidOption, msg: "<= 4096"},
		{name: "limit-zero", opts: withEmpty(plg.WithMemoryLimit(0)), want: plg.ErrInvalidOption, msg: "(0)"},
		{name: "limit-small", opts: withEmpty(plg.WithMemoryLimit(64<<10 - 1)), want: plg.ErrInvalidOption},
		{name: "limit-huge", opts: withEmpty(plg.WithMemoryLimit(4<<30 + 1)), want: plg.ErrInvalidOption},
		{
			name: "calls-zero", opts: withEmpty(plg.WithConcurrentCalls(0)), want: plg.ErrInvalidOption,
			msg: "WithConcurrentCalls(0): want 1 to 4096",
		},
		{name: "calls-negative", opts: withEmpty(plg.WithConcurrentCalls(-1)), want: plg.ErrInvalidOption},
		{name: "calls-huge", opts: withEmpty(plg.WithConcurrentCalls(4097)), want: plg.ErrInvalidOption},
		{
			name: "calls-too-many-in-all", want: plg.ErrInvalidOption, msg: "up to 257 instances: want at most 1048576",
			opts: []plg.RegisterOption{emptyStorage(), plg.WithPool(1, 257), plg.WithConcurrentCalls(4096)},
		},
		{
			name: "desc-limit-small", want: plg.ErrInvalidDescriptor, msg: `"k" with a memory limit of 65535 bytes`,
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{
				{
					Key: "k", Implements: implementsOf[pinger](), Language: plg.LanguageGo, Entrypoint: "p.go",
					MemoryLimit: 64<<10 - 1,
				},
			}})},
		},
		{
			name: "poll-negative", opts: withEmpty(plg.WithPollInterval(-time.Second)), want: plg.ErrInvalidOption,
			msg: "WithPollInterval(-1s)",
		},
		{
			name: "desc-limit-huge", want: plg.ErrInvalidDescriptor, msg: "4294967297 bytes",
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{
				{
					Key: "k", Implements: implementsOf[pinger](), Language: plg.LanguageGo, Entrypoint: "p.go",
					MemoryLimit: 4<<30 + 1,
				},
			}})},
		},
		{
			name: "desc-owner-too-many-labels",
			want: plg.ErrInvalidDescriptor,
			msg:  `"k": owner has too many labels: 33, limit 32`,
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{
				{
					Key: "k", Implements: implementsOf[pinger](), Language: plg.LanguageGo, Entrypoint: "p.go",
					Owner: &plg.Owner{Labels: labelMap(33)},
				},
			}})},
		},
		{
			name: "desc-owner-long-string", want: plg.ErrInvalidDescriptor, msg: `"k": team.name: owner strings`,
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{{
				Key: "k", Implements: implementsOf[pinger](), Language: plg.LanguageGo, Entrypoint: "p.go",
				Owner: &plg.Owner{Team: plg.Team{Name: strings.Repeat("t", plg.MaxOwnerStringBytes+1)}},
			}}})},
		},
		{
			name: "go-guest-empty", opts: withEmpty(plg.WithGoGuest(nil)), want: plg.ErrInvalidOption,
			msg: "WithGoGuest: the module is empty",
		},
		{
			name: "go-guest-twice", want: plg.ErrInvalidOption, msg: "WithGoGuest given 2 times",
			opts: []plg.RegisterOption{emptyStorage(), plg.WithGoGuest([]byte("a")), plg.WithGoGuest([]byte("a"))},
		},
		{
			// The custom guest replaces the linked one (the unit test binary links none) and fails to compile.
			name: "go-guest-not-wasm", want: plg.ErrLoad,
			msg:  "loading the go sha256:982d9e3eb996f559e633f4d194def3761d909f5a3b647d1a851fead67c32c9d1 guest",
			opts: []plg.RegisterOption{plg.WithGoGuest([]byte("text")), plg.WithStorage(goPluginStorage(t))},
		},
		{
			name: "go-guest-without-the-abi", want: plg.ErrLoad, msg: "missing memory or one of the plg_alloc",
			opts: []plg.RegisterOption{
				plg.WithGoGuest([]byte("\x00asm\x01\x00\x00\x00")), plg.WithStorage(goPluginStorage(t)),
			},
		},
	}

	ctr := newUnitContainer(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := plg.Register[pinger](t.Context(), ctr, tc.name, tc.opts...)
			require.ErrorIs(t, err, tc.want)
			require.ErrorContains(t, err, tc.msg)

			if errors.Is(tc.want, plg.ErrInvalidName) {
				return
			}

			require.NoError(t, plg.Register[pinger](t.Context(), ctr, tc.name, emptyStorage()),
				"a failed Register leaves its name free")
		})
	}
}

func withEmpty(opt plg.RegisterOption) []plg.RegisterOption {
	return []plg.RegisterOption{emptyStorage(), opt}
}

// TestRegisterAcceptsTheLimitsOfPoolsAndMemory registers at each bound: the options are accepted, and so are
// descriptor limits, whose plugin then fails only for its unsupported language, checked after the descriptor.
func TestRegisterAcceptsTheLimitsOfPoolsAndMemory(t *testing.T) {
	t.Parallel()

	ctr := newUnitContainer(t)

	for i, opt := range []plg.RegisterOption{
		plg.WithPool(1, 1), plg.WithPool(4096, 4096), plg.WithMemoryLimit(64 << 10), plg.WithMemoryLimit(4 << 30),
		plg.WithConcurrentCalls(1), plg.WithConcurrentCalls(4096),
	} {
		require.NoError(t, plg.Register[pinger](t.Context(), ctr, "opt"+strconv.Itoa(i), withEmpty(opt)...))
	}

	for _, limit := range []uint64{64 << 10, 4 << 30} {
		store := listStorage{name: "s", descs: []plg.Descriptor{
			{
				Key: "k", Implements: implementsOf[pinger](), Language: "cobol", Entrypoint: "main.cbl",
				MemoryLimit: limit,
			},
		}}

		err := plg.Register[pinger](t.Context(), ctr, "desc"+strconv.FormatUint(limit, 10), plg.WithStorage(store))
		require.ErrorIs(t, err, plg.ErrUnsupportedLanguage)
	}
}

func TestRegisterNeedsAContract(t *testing.T) {
	t.Parallel()

	type unregistered interface {
		Ping(ctx context.Context) error
	}

	err := plg.Register[unregistered](t.Context(), newUnitContainer(t), "u", emptyStorage())
	require.ErrorIs(t, err, plg.ErrNoContract)
	require.ErrorContains(t, err, "go generate")
}

func TestRegisteringANameTwiceFails(t *testing.T) {
	t.Parallel()

	ctr := newUnitContainer(t)
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", emptyStorage()))

	err := plg.Register[pinger](t.Context(), ctr, "ping", emptyStorage())
	require.ErrorIs(t, err, plg.ErrDuplicateName)
	require.ErrorContains(t, err, "ping")

	err = plg.Register[testcontract.Fooer](t.Context(), ctr, "ping", emptyStorage())
	require.ErrorIs(t, err, plg.ErrDuplicateName, "names are unique across interfaces")

	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "pong", emptyStorage()))
	require.NoError(t, plg.Register[pinger](t.Context(), newUnitContainer(t), "ping", emptyStorage()),
		"names are per container")
}

func TestEmptyCatalogue(t *testing.T) {
	t.Parallel()

	ctr := newUnitContainer(t)
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", emptyStorage()))

	_, err := plg.Get[pinger](t.Context(), ctr, "ping/x")
	require.ErrorIs(t, err, plg.ErrNotFound)
	require.ErrorContains(t, err, "ping/x")

	for key := range plg.All[pinger](t.Context(), ctr) {
		assert.Fail(t, "an empty registration yields nothing", key)
	}
}

func TestClosedContainer(t *testing.T) {
	t.Parallel()

	ctr := plg.New()
	require.NoError(t, plg.Register[pinger](t.Context(), ctr, "ping", emptyStorage()))
	require.NoError(t, ctr.Close(t.Context()))
	require.NoError(t, ctr.Close(t.Context()), "closing twice does nothing")

	_, err := plg.Get[pinger](t.Context(), ctr, "ping/x")
	require.ErrorIs(t, err, plg.ErrClosed)

	err = plg.Register[pinger](t.Context(), ctr, "other", emptyStorage())
	require.ErrorIs(t, err, plg.ErrClosed)

	for key := range plg.All[pinger](t.Context(), ctr) {
		assert.Fail(t, "a closed container yields nothing", key)
	}
}

func TestCloseDuringRegisterPublishesNothing(t *testing.T) {
	t.Parallel()

	ctr := plg.New()

	err := plg.Register[pinger](t.Context(), ctr, "ping", plg.WithStorage(closingStorage{ctr: ctr}))
	require.ErrorIs(t, err, plg.ErrClosed, "the container closed while Register was loading")

	_, err = plg.Get[pinger](t.Context(), ctr, "ping/x")
	require.ErrorIs(t, err, plg.ErrClosed)
}

func TestWithCompilationCacheDirAndLogger(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	var logs bytes.Buffer

	dir := filepath.Join(file, "cache") // below a regular file: never creatable
	ctr := plg.New(plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))), plg.WithCompilationCacheDir(dir))
	require.NoError(t, ctr.Close(t.Context()))

	assert.Contains(t, logs.String(), `"level":"WARN"`)
	assert.Contains(
		t,
		logs.String(),
		`"dir":"`+dir+`"`,
		"the option reached the engine; the warning reached the logger",
	)
}

func TestRegisterContractRejectsInvalidContracts(t *testing.T) {
	t.Parallel()

	type (
		missingField struct {
			WPong func(ctx context.Context, msg string) (string, error)
		}
		wrongField struct {
			WPing func(ctx context.Context, msg string) string
		}
		noMethods struct {
			WPing func(ctx context.Context, msg string) (string, error)
		}
	)

	source := fstest.MapFS{"x.go": {Data: []byte("package x\n")}}
	iface, adapter := reflect.TypeFor[pinger](), reflect.TypeFor[pingerAdapter]()

	tests := []struct {
		contract plg.Contract
		name     string
		want     string
	}{
		{name: "nil interface", contract: plg.Contract{Adapter: adapter, Source: source}, want: "nil Interface"},
		{
			name: "invalid interface", want: "parameter 1",
			contract: plg.Contract{Interface: reflect.TypeFor[funcParam](), Adapter: adapter, Source: source},
		},
		{
			name: "nil adapter", want: "adapter is not a struct type",
			contract: plg.Contract{Interface: iface, Source: source},
		},
		{
			name: "pointer adapter", want: "adapter is not a struct type",
			contract: plg.Contract{Interface: iface, Adapter: reflect.TypeFor[*pingerAdapter](), Source: source},
		},
		{
			name: "missing field", want: "lacks the field WPing",
			contract: plg.Contract{Interface: iface, Adapter: reflect.TypeFor[missingField](), Source: source},
		},
		{
			name: "wrong field type", want: "lacks the field WPing func(context.Context, string) (string, error)",
			contract: plg.Contract{Interface: iface, Adapter: reflect.TypeFor[wrongField](), Source: source},
		},
		{
			name: "no methods", want: "does not implement",
			contract: plg.Contract{Interface: iface, Adapter: reflect.TypeFor[noMethods](), Source: source},
		},
		{name: "nil source", want: "nil Source", contract: plg.Contract{Interface: iface, Adapter: adapter}},
		{
			name: "no go files", want: "no .go file",
			contract: plg.Contract{
				Interface: iface, Adapter: adapter,
				Source: fstest.MapFS{"sub/x.go": {Data: []byte("package x\n")}, "README": {}},
			},
		},
		{
			name: "unreadable source", want: "reading the source",
			contract: plg.Contract{Interface: iface, Adapter: adapter, Source: brokenFS{}},
		},
		{
			name: "parameter with clashing field names", want: `parameter 1: wire: unsupported type: plg_test.clash: ` +
				`fields A and B are both named "name"`,
			contract: plg.Contract{
				Interface: reflect.TypeFor[clashIn](), Adapter: reflect.TypeFor[clashInAdapter](), Source: source,
			},
		},
		{
			name: "result with clashing field names", want: "result 0: wire: unsupported type: plg_test.clash",
			contract: plg.Contract{
				Interface: reflect.TypeFor[clashOut](), Adapter: reflect.TypeFor[clashOutAdapter](), Source: source,
			},
		},
		{
			name: "one alias name for two embedded types",
			want: "rendering the Go dispatcher: method Ping: goabi: cannot render dispatcher: embedded fields named " +
				"Embedded have the types testcontract.Counter and plg_test.aliasTarget",
			contract: plg.Contract{
				Interface: reflect.TypeFor[aliasClash](), Adapter: reflect.TypeFor[aliasClashAdapter](), Source: source,
			},
		},
		{
			name: "fast adapter not implementing the interface", want: "the Fast adapter does not implement",
			contract: plg.Contract{
				Interface: iface, Adapter: adapter, Source: source,
				Fast: func(plgimpl.Invoker) any { return struct{}{} },
			},
		},
		{
			name: "nil fast adapter", want: "the Fast adapter does not implement",
			contract: plg.Contract{
				Interface: iface, Adapter: adapter, Source: source, Fast: func(plgimpl.Invoker) any { return nil },
			},
		},
		{
			// Generated code predating a change of the interface's types would encode the old layout.
			name: "fast adapter for another layout", want: "generated for another layout",
			contract: plg.Contract{
				Interface: iface,
				Adapter:   adapter,
				Source:    source,
				Layout:    fingerprint(t, reflect.TypeFor[pingerBefore]()),
				Fast:      func(plgimpl.Invoker) any { return &pingerAdapter{} },
			},
		},
		{name: "already registered", want: "already has a contract", contract: pingerContract()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := plg.RegisterContract(tc.contract)
			require.ErrorIs(t, err, plg.ErrInvalidContract)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// pingerBefore is pinger as it was before its message became a string: the fast paths generated then encode
// another layout.
type pingerBefore interface {
	Ping(ctx context.Context, msg []byte) (string, error)
}

// fastPinger has a hand-written fast adapter, as plg gen would generate.
type (
	fastPinger interface {
		Ping(ctx context.Context, msg string, times int) (string, error)
	}
	fastPingerAdapter struct {
		WPing func(ctx context.Context, msg string, times int) (string, error)
	}
	fastPingerFast struct {
		inv plgimpl.Invoker
	}
)

func (a *fastPingerAdapter) Ping(ctx context.Context, msg string, times int) (string, error) {
	return a.WPing(ctx, msg, times)
}

func (*fastPingerFast) Ping(context.Context, string, int) (string, error) {
	return "", nil
}

func fingerprint(t *testing.T, iface reflect.Type) string {
	t.Helper()

	plan, err := layout.OfInterface(iface)
	require.NoError(t, err)

	return plan.Fingerprint()
}

// fastPingerContract is registered by TestMain (contracts are process-global, and tests repeat with -count).
func fastPingerContract() plg.Contract {
	iface := reflect.TypeFor[fastPinger]()

	plan, err := layout.OfInterface(iface)
	if err != nil {
		panic(err)
	}

	return plg.Contract{
		Interface: iface, Adapter: reflect.TypeFor[fastPingerAdapter](),
		Source: fstest.MapFS{"pinger.go": {Data: []byte("package plg_test\n")}},
		Fast:   func(inv plgimpl.Invoker) any { return &fastPingerFast{inv: inv} },
		Layout: plan.Fingerprint(),
	}
}

func TestRegisterContractKeepsACurrentFastAdapter(t *testing.T) {
	t.Parallel()

	var given plgimpl.Invoker = new(answeringInvoker)

	adapter, ok := plg.FastAdapter(reflect.TypeFor[fastPinger](), given).(*fastPingerFast)
	require.True(t, ok, "registered with its fast adapter")
	assert.Same(t, given, adapter.inv, "bound to the invoker")
	assert.Nil(t, plg.FastAdapter(reflect.TypeFor[pinger](), given), "a contract without Fast has none")
}

func TestMustRegisterContractPanicsOnInvalidContracts(t *testing.T) {
	t.Parallel()

	var recovered any

	func() {
		defer func() { recovered = recover() }()

		plg.MustRegisterContract(pingerContract()) // registered by TestMain
	}()

	err, ok := recovered.(error)
	require.True(t, ok, "it panics with the error")
	require.ErrorIs(t, err, plg.ErrInvalidContract)
}

func TestMemoryStorage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := plg.NewMemoryStorage("mem")
	assert.Equal(t, "mem", store.Name())

	files := fstest.MapFS{"p/plugin.go": {Data: []byte("v1")}}
	second := plg.Descriptor{Key: "b", Language: plg.LanguageGo, Entrypoint: "p/plugin.go", Version: "1"}
	first := plg.Descriptor{Key: "a", Language: plg.LanguageGo, Entrypoint: "p", Version: "1"}

	require.NoError(t, store.Put(&second, files))
	require.NoError(t, store.Put(&first, files))

	files["p/plugin.go"] = &fstest.MapFile{Data: []byte("v2")}

	descs, err := store.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []plg.Descriptor{first, second}, descs, "sorted by key")

	opened, err := store.Open(ctx, &first)
	require.NoError(t, err)

	data, err := fs.ReadFile(opened, "p/plugin.go")
	require.NoError(t, err)
	assert.Equal(t, "v1", string(data), "Put copied the files")

	second.Version = "2"
	require.NoError(t, store.Put(&second, files))
	store.Delete("a")

	descs, err = store.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []plg.Descriptor{second}, descs, "Put replaced b, Delete removed a")

	_, err = store.Open(ctx, &first)
	require.ErrorIs(t, err, fs.ErrNotExist)

	err = store.Put(&plg.Descriptor{Language: plg.LanguageGo}, files)
	require.ErrorIs(t, err, plg.ErrInvalidDescriptor)

	err = store.Put(&plg.Descriptor{Key: "broken"}, brokenFS{})
	require.ErrorIs(t, err, errStorage)
}
