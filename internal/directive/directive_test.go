package directive_test

import (
	"go/ast"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/buildguest"
	"github.com/heynemann/go-plg/internal/directive"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// doc is a doc comment of the given lines.
func doc(lines ...string) *ast.CommentGroup {
	group := &ast.CommentGroup{}
	for _, line := range lines {
		group.List = append(group.List, &ast.Comment{Text: line})
	}

	return group
}

func TestParseReadsEveryDirective(t *testing.T) {
	t.Parallel()

	dirs, err := directive.Parse(doc(
		"// Rule is implemented by reward rules.",
		"//",
		"//plg:limits memory=64MiB timeout=2s pool=1..8",
		"//plg:guest-packages\tencoding/xml  example.com/lib",
		"//plg:validation Input",
		"// plg:limits memory=nonsense is prose: a directive has no space after the slashes",
		"/*plg:limits memory=nonsense*/",
	))
	require.NoError(t, err)
	assert.Equal(t, directive.Directives{
		Limits:        directive.Limits{Memory: 64 << 20, Timeout: 2 * time.Second, PoolMin: 1, PoolMax: 8},
		GuestPackages: []string{"encoding/xml", "example.com/lib"},
		Validation:    "Input",
	}, dirs)
}

func TestParseWithoutDirectives(t *testing.T) {
	t.Parallel()

	for name, group := range map[string]*ast.CommentGroup{
		"no doc comment": nil,
		"prose only":     doc("// Rule is implemented by reward rules."),
	} {
		dirs, err := directive.Parse(group)
		require.NoError(t, err, name)
		assert.Equal(t, directive.Directives{}, dirs, name)
	}
}

func TestParseLimitsOneKeyAtATime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		directive string
		want      directive.Limits
	}{
		{"memory=65536", directive.Limits{Memory: 64 << 10}}, // bytes, the smallest: one page
		{"memory=100000", directive.Limits{Memory: 100000}},  // need not be whole pages
		{"memory=4294967296B", directive.Limits{Memory: 4 << 30}},
		{"memory=64KiB", directive.Limits{Memory: 64 << 10}},
		{"memory=48MiB", directive.Limits{Memory: 48 << 20}},
		{"memory=4GiB", directive.Limits{Memory: 4 << 30}}, // the largest
		{"timeout=500ms", directive.Limits{Timeout: 500 * time.Millisecond}},
		{"timeout=1m30s", directive.Limits{Timeout: 90 * time.Second}},
		{"timeout=1ns", directive.Limits{Timeout: 1}},
		{"pool=1..8", directive.Limits{PoolMin: 1, PoolMax: 8}},
		{"pool=4", directive.Limits{PoolMin: 4, PoolMax: 4}},
		{"pool=1..4096", directive.Limits{PoolMin: 1, PoolMax: 4096}},
		{"pool=007..7", directive.Limits{PoolMin: 7, PoolMax: 7}},
		{"timeout=2s memory=1GiB", directive.Limits{Memory: 1 << 30, Timeout: 2 * time.Second}}, // any order
	}

	for _, tc := range tests {
		dirs, err := directive.Parse(doc("//plg:limits " + tc.directive))
		require.NoError(t, err, tc.directive)
		assert.Equal(t, tc.want, dirs.Limits, tc.directive)
	}
}

func TestParseRejectsMalformedDirectives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		line, msg string
	}{
		// Memory: units, syntax and range.
		{"//plg:limits memory=65535", "memory=65535: want bytes or a number of B, KiB, MiB or GiB from 64KiB"},
		{"//plg:limits memory=63KiB", "memory=63KiB: want"},
		{"//plg:limits memory=4194305KiB", "memory=4194305KiB: want"}, // 4 GiB + 1 KiB
		{"//plg:limits memory=4294967297", "memory=4294967297: want"},
		{"//plg:limits memory=17179869184GiB", "memory=17179869184GiB: want"}, // 2^64 bytes: overflows
		{"//plg:limits memory=64MB", "memory=64MB: want"},
		{"//plg:limits memory=64mib", "memory=64mib: want"},
		{"//plg:limits memory=MiB", "memory=MiB: want"},
		{"//plg:limits memory=", "memory=: want"},
		{"//plg:limits memory=+64MiB", "memory=+64MiB: want"},
		{"//plg:limits memory=1_000_000", "memory=1_000_000: want"},
		{"//plg:limits memory=1.5GiB", "memory=1.5GiB: want"},
		// Timeout.
		{"//plg:limits timeout=0s", "timeout=0s: want a positive duration"},
		{"//plg:limits timeout=-1s", "timeout=-1s: want a positive duration"},
		{"//plg:limits timeout=2", "timeout=2: want a positive duration"},
		{"//plg:limits timeout=soon", "timeout=soon: want a positive duration"},
		// Pool.
		{"//plg:limits pool=0..4", "pool=0..4: want MIN..MAX or N with 1 <= MIN <= MAX <= 4096"},
		{"//plg:limits pool=0", "pool=0: want MIN..MAX"},
		{"//plg:limits pool=3..2", "pool=3..2: want MIN..MAX"},
		{"//plg:limits pool=1..4097", "pool=1..4097: want MIN..MAX"},
		{"//plg:limits pool=1-8", "pool=1-8: want MIN..MAX"},
		{"//plg:limits pool=1..", "pool=1..: want MIN..MAX"},
		{"//plg:limits pool=..8", "pool=..8: want MIN..MAX"},
		{"//plg:limits pool=+1..2", "pool=+1..2: want MIN..MAX"},
		{"//plg:limits pool=1..2..3", "pool=1..2..3: want MIN..MAX"},
		{"//plg:limits pool=99999999999999999999", "want MIN..MAX"},
		// The directive's shape.
		{"//plg:limits", "no limits; write memory=, timeout= or pool="},
		{"//plg:limits memory", `"memory" is not key=value`},
		{"//plg:limits cpu=2", `unknown limit "cpu" (want memory, timeout or pool)`},
		{"//plg:limits memory=64MiB memory=32MiB", "memory is set twice"},
		{"//plg:limit memory=64MiB", "unknown directive //plg:limit (want //plg:limits, //plg:guest-packages"},
		{"//plg:", "unknown directive //plg: "},
		{"//plg: limits memory=64MiB", "unknown directive //plg: "},
		// Guest packages, checked as plg build-guest checks them.
		{"//plg:guest-packages", "name at least one package"},
		{"//plg:guest-packages strings", "strings is in the default allowlist already"},
		{"//plg:guest-packages encoding/xml net/rpc encoding/xml", "encoding/xml is listed twice"},
		{"//plg:guest-packages ./local", `"./local" is not an import path`},
		// Validation.
		{"//plg:validation", "want the name of one type of the contract package"},
		{"//plg:validation Input Other", "want the name of one type"},
		{"//plg:validation pkg.Input", "want the name of one type"},
	}

	for _, tc := range tests {
		_, err := directive.Parse(doc("// Rule is a rule.", tc.line))
		require.ErrorIs(t, err, directive.ErrDirective, tc.line)
		require.ErrorContains(t, err, tc.line+": ", "the error quotes the directive")
		require.ErrorContains(t, err, tc.msg, tc.line)
	}

	_, err := directive.Parse(doc("//plg:guest-packages strings"))
	require.ErrorIs(t, err, buildguest.ErrPackages, "build-guest's own error, wrapped")
}

func TestParseRejectsRepeatedDirectives(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"limits memory=64MiB", "guest-packages encoding/xml", "validation Input"} {
		_, err := directive.Parse(doc("//plg:"+name, "// prose between", "//plg:"+name))
		require.ErrorIs(t, err, directive.ErrDirective, name)
		assert.ErrorContains(t, err, "//plg:"+name+": invalid plg directive: repeated", name)
	}
}

func TestSetLeavesTheLimitsAloneOnError(t *testing.T) {
	t.Parallel()

	limits := directive.Limits{Memory: 1 << 20, Timeout: time.Second, PoolMin: 2, PoolMax: 3}
	want := limits

	for key, value := range map[string]string{"memory": "1KiB", "timeout": "0s", "pool": "0", "cpu": "1"} {
		require.ErrorIs(t, limits.Set(key, value), directive.ErrDirective, key)
	}

	assert.Equal(t, want, limits)
}

func TestOverrideTakesTheLimitsSet(t *testing.T) {
	t.Parallel()

	base := directive.Limits{Memory: 128 << 20, PoolMin: 1, PoolMax: 32}

	assert.Equal(t, base, base.Override(directive.Limits{}), "nothing set, nothing changes")
	assert.Equal(t, directive.Limits{Memory: 16 << 20, Timeout: time.Second, PoolMin: 1, PoolMax: 32},
		base.Override(directive.Limits{Memory: 16 << 20, Timeout: time.Second}))
	assert.Equal(t, directive.Limits{Memory: 128 << 20, PoolMin: 4, PoolMax: 4},
		base.Override(directive.Limits{PoolMin: 4, PoolMax: 4}), "the pool is overridden as a pair")
	assert.Equal(t, directive.Limits{Memory: 1 << 30, Timeout: time.Minute, PoolMin: 2, PoolMax: 3},
		base.Override(directive.Limits{Memory: 1 << 30, Timeout: time.Minute, PoolMin: 2, PoolMax: 3}))
}

func TestStringWritesWhatParseReads(t *testing.T) {
	t.Parallel()

	tests := map[string]directive.Limits{
		"none":                                {},
		"memory=64MiB":                        {Memory: 64 << 20},
		"memory=100000B":                      {Memory: 100000},
		"memory=65KiB":                        {Memory: 65 << 10},
		"memory=4GiB timeout=1m30s pool=2..8": {Memory: 4 << 30, Timeout: 90 * time.Second, PoolMin: 2, PoolMax: 8},
		"timeout=250ms pool=4..4":             {Timeout: 250 * time.Millisecond, PoolMin: 4, PoolMax: 4},
	}

	for want, limits := range tests {
		assert.Equal(t, want, limits.String())

		if want == "none" {
			continue
		}

		dirs, err := directive.Parse(doc("//plg:limits " + want))
		require.NoError(t, err, want)
		assert.Equal(t, limits, dirs.Limits, want)
	}

	assert.Equal(t, "0B", directive.FormatMemory(0))
	assert.Equal(t, "1B", directive.FormatMemory(1))
	assert.Equal(t, "1025KiB", directive.FormatMemory(1025<<10))
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"//plg:limits memory=64MiB timeout=2s pool=1..8", "//plg:limits pool=4", "//plg:limits memory=4GiB",
		"//plg:guest-packages encoding/xml", "//plg:validation Input", "//plg:limits memory=17179869184GiB",
		"//plg:limits pool=1..2..3", "//plg:", "// prose",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, line string) {
		if strings.ContainsAny(line, "\n\r") {
			t.Skip("a // comment is one line")
		}

		dirs, err := directive.Parse(doc(line))
		if err != nil {
			require.ErrorIs(t, err, directive.ErrDirective)

			return
		}

		limits := dirs.Limits
		if limits == (directive.Limits{}) {
			return
		}

		assert.GreaterOrEqual(t, limits.PoolMin, 0)
		assert.LessOrEqual(t, limits.PoolMax, directive.MaxPool)
		assert.LessOrEqual(t, limits.PoolMin, limits.PoolMax)
		assert.LessOrEqual(t, limits.Memory, uint64(directive.MaxMemory))
		assert.GreaterOrEqual(t, limits.Timeout, time.Duration(0))

		again, err := directive.Parse(doc("//plg:limits " + limits.String()))
		require.NoError(t, err, "String writes what Parse reads")
		assert.Equal(t, limits, again.Limits)
	})
}
