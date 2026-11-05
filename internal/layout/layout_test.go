package layout_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/layout"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type (
	point struct {
		X int `json:"x"`
		Y int `json:"y"`
	}
	pointRenamed struct { // json names do not cross the wire
		X int `json:"left"`
		Y int `json:"top"`
	}
	pointAdded struct {
		X int
		Y int
		Z int
	}
	pointSwapped struct {
		Y int
		X int
	}
	pointSkipped struct {
		X int
		Y int `json:"-"`
	}
	pointNarrow struct {
		X int8
		Y int
	}
	pointRenamedGo struct {
		X  int
		Y2 int `json:"y"`
	}

	echo[T any] interface {
		Echo(ctx context.Context, v T) (T, error)
	}
	echoTwice interface {
		Echo(ctx context.Context, v point) (point, error)
		Other(ctx context.Context) error
	}
	renamed interface {
		Mirror(ctx context.Context, v point) (point, error)
	}
)

func fingerprint(t *testing.T, iface reflect.Type) string {
	t.Helper()

	plan, err := layout.OfInterface(iface)
	require.NoError(t, err)

	return plan.Fingerprint()
}

// TestFingerprintFollowsTheEncoding: a fingerprint changes exactly when generated code would encode
// differently or would no longer match the Go types.
func TestFingerprintFollowsTheEncoding(t *testing.T) {
	t.Parallel()

	base := fingerprint(t, reflect.TypeFor[echo[point]]())

	assert.Equal(t, base, fingerprint(t, reflect.TypeFor[echo[pointRenamed]]()), "json names do not cross")
	assert.Equal(t, base, fingerprint(t, reflect.TypeFor[echo[point]]()), "deterministic")

	for name, iface := range map[string]reflect.Type{
		"field added":           reflect.TypeFor[echo[pointAdded]](),
		"fields swapped":        reflect.TypeFor[echo[pointSwapped]](),
		"field skipped":         reflect.TypeFor[echo[pointSkipped]](),
		"field narrowed":        reflect.TypeFor[echo[pointNarrow]](),
		"field renamed in Go":   reflect.TypeFor[echo[pointRenamedGo]](),
		"method added":          reflect.TypeFor[echoTwice](),
		"method renamed":        reflect.TypeFor[renamed](),
		"pointer to the type":   reflect.TypeFor[echo[*point]](),
		"slice of the type":     reflect.TypeFor[echo[[]point]](),
		"map of the type":       reflect.TypeFor[echo[map[string]point]](),
		"bytes, not a slice":    reflect.TypeFor[echo[[]byte]](),
		"time, not a struct":    reflect.TypeFor[echo[time.Time]](),
		"one value, not a pair": reflect.TypeFor[echo[int]](),
	} {
		assert.NotEqual(t, base, fingerprint(t, iface), name)
	}
}

type (
	tree struct {
		Name string
		Kids []*tree
	}
	list  []list
	nest  map[string]nest
	sizes struct {
		T    time.Time
		P    *tree
		S    string
		Skip string `json:"-"`
		I    int16
		B    bool
	}
)

// TestBuildPlansEachTypeOnce: recursive types terminate, a type reached twice is one node, and the minimum
// sizes are the codec's.
func TestBuildPlansEachTypeOnce(t *testing.T) {
	t.Parallel()

	plan, err := layout.OfInterface(reflect.TypeFor[interface {
		A(ctx context.Context, t tree, l list, n nest) (tree, error)
		B(ctx context.Context, s sizes, again []sizes) error
	}]())
	require.NoError(t, err)
	require.Len(t, plan.Methods, 2)

	a := plan.Methods[0]
	assert.Equal(t, "A", a.Name)
	assert.Same(t, a.Params[0], a.Results[0], "one node per type")
	assert.Same(t, a.Params[0], a.Params[0].Fields[1].Node.Elem.Elem, "a recursive struct refers to itself")
	assert.Same(t, a.Params[1], a.Params[1].Elem, "a recursive slice refers to itself")
	assert.Same(t, a.Params[2], a.Params[2].Elem, "a recursive map refers to itself")

	s := plan.Methods[1].Params[0]
	assert.Equal(t, layout.Struct, s.Kind)
	assert.Equal(t, 16+1+4+8+1, s.MinSize, "time, pointer, string, int and bool; the skipped field adds nothing")
	assert.Equal(t, []string{"Skip"}, s.Skipped)
	assert.Same(t, s, plan.Methods[1].Params[1].Elem)

	for i, node := range plan.Composites {
		assert.Equal(t, i+1, node.ID, "composites are numbered in order")
		assert.True(t, node.Kind.Composite())
	}
}

type (
	unexported struct {
		A int
		b int //nolint:unused // the unexported field is the case under test
	}
	clash struct {
		A int `json:"x"`
		B int `json:"x"` //nolint:govet,revive // structtag, struct-tag: the duplicate is the case under test
	}
	empty struct{}
)

func TestBuildRejectsWhatTheCodecCannotEncode(t *testing.T) {
	t.Parallel()

	for name, iface := range map[string]reflect.Type{
		"unexported field":   reflect.TypeFor[echo[unexported]](),
		"duplicate names":    reflect.TypeFor[echo[clash]](),
		"int map keys":       reflect.TypeFor[echo[map[int]string]](),
		"channel":            reflect.TypeFor[echo[chan int]](),
		"interface":          reflect.TypeFor[echo[error]](),
		"array":              reflect.TypeFor[echo[[2]int]](),
		"nested unsupported": reflect.TypeFor[echo[map[string][]func()]](),
		"empty elements":     reflect.TypeFor[echo[[]empty]](),
	} {
		_, err := layout.OfInterface(iface)
		require.ErrorIs(t, err, layout.ErrUnsupported, name)
	}

	_, err := layout.OfInterface(reflect.TypeFor[interface {
		M(ctx context.Context) (chan int, error)
	}]())
	require.ErrorContains(t, err, "method M: result 0")
}

func TestKindNames(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "float32", layout.Float32.String())
	assert.Equal(t, "kind(99)", layout.Kind(99).String())
	assert.False(t, layout.Time.Composite())
	assert.True(t, layout.Slice.Composite())
}

type (
	// parent and child are mutually recursive: a child's minimum size is its parent's slice length prefix.
	parent struct{ Children []child }
	child  struct{ Parent parent }
	// outer holds a struct that encodes as nothing, so it encodes as nothing too.
	outer struct{ In empty }
)

// TestBuildSizesMutuallyRecursiveStructs: a struct reached again while it is planned gets its size once the
// layout is complete, whichever of the two the layout reaches first, as the codec sizes it.
func TestBuildSizesMutuallyRecursiveStructs(t *testing.T) {
	t.Parallel()

	for name, iface := range map[string]reflect.Type{
		"parent first": reflect.TypeFor[interface {
			M(ctx context.Context, p parent, c child, cs []child) error
		}](),
		"child first": reflect.TypeFor[interface {
			M(ctx context.Context, c child, p parent, cs []child) error
		}](),
	} {
		plan, err := layout.OfInterface(iface)
		require.NoError(t, err, name)

		for _, node := range plan.Methods[0].Params {
			assert.Equal(t, 4, node.MinSize, "%s: %v", name, node.Type)
		}

		assert.Equal(t, 4, plan.Methods[0].Params[2].Elem.MinSize, name)
	}

	_, err := layout.OfInterface(reflect.TypeFor[echo[[]outer]]())
	require.ErrorIs(t, err, layout.ErrUnsupported, "a struct of structs encoding as nothing encodes as nothing")
}
