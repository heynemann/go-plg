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
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

func TestMain(m *testing.M) {
	err := plg.RegisterContract(pingerContract())
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

func (s *failingStorage) Open(ctx context.Context, desc plg.Descriptor) (fs.FS, error) {
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

func (s listStorage) Open(context.Context, plg.Descriptor) (fs.FS, error) {
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

func (closingStorage) Open(context.Context, plg.Descriptor) (fs.FS, error) {
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
				{Language: plg.LanguageGo, Entrypoint: "p.go"},
			}})},
		},
		{
			name: "noentry", want: plg.ErrInvalidDescriptor, msg: "storage s",
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{
				{Key: "k", Language: plg.LanguageGo},
			}})},
		},
		{
			name: "dupkey", want: plg.ErrDuplicateKey, msg: `"k": listed by storages one and two`,
			opts: []plg.RegisterOption{
				plg.WithStorage(listStorage{name: "one", descs: []plg.Descriptor{{Key: "k", Entrypoint: "a"}}}),
				plg.WithStorage(listStorage{name: "two", descs: []plg.Descriptor{{Key: "k", Entrypoint: "b"}}}),
			},
		},
		{
			name: "language", want: plg.ErrUnsupportedLanguage, msg: `plugin k (storage s)`,
			opts: []plg.RegisterOption{plg.WithStorage(listStorage{name: "s", descs: []plg.Descriptor{
				{Key: "k", Language: "cobol", Entrypoint: "main.cbl"},
			}})},
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

	require.NoError(t, store.Put(second, files))
	require.NoError(t, store.Put(first, files))

	files["p/plugin.go"] = &fstest.MapFile{Data: []byte("v2")}

	descs, err := store.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []plg.Descriptor{first, second}, descs, "sorted by key")

	opened, err := store.Open(ctx, first)
	require.NoError(t, err)

	data, err := fs.ReadFile(opened, "p/plugin.go")
	require.NoError(t, err)
	assert.Equal(t, "v1", string(data), "Put copied the files")

	second.Version = "2"
	require.NoError(t, store.Put(second, files))
	store.Delete("a")

	descs, err = store.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []plg.Descriptor{second}, descs, "Put replaced b, Delete removed a")

	_, err = store.Open(ctx, first)
	require.ErrorIs(t, err, fs.ErrNotExist)

	err = store.Put(plg.Descriptor{Language: plg.LanguageGo}, files)
	require.ErrorIs(t, err, plg.ErrInvalidDescriptor)

	err = store.Put(plg.Descriptor{Key: "broken"}, brokenFS{})
	require.ErrorIs(t, err, errStorage)
}
