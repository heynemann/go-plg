package directive_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/directive"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestLimitsSetReadsEachKeyAndUnit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key, value string
		want       directive.Limits
	}{
		{"memory", "65536", directive.Limits{Memory: 64 << 10}},
		{"memory", "64KiB", directive.Limits{Memory: 64 << 10}},
		{"memory", "96MiB", directive.Limits{Memory: 96 << 20}},
		{"memory", "4GiB", directive.Limits{Memory: 4 << 30}},
		{"memory", "100000", directive.Limits{Memory: 100000}},
		{"timeout", "1500ms", directive.Limits{Timeout: 1500 * time.Millisecond}},
		{"pool", "3", directive.Limits{PoolMin: 3, PoolMax: 3}},
		{"pool", "1..4096", directive.Limits{PoolMin: 1, PoolMax: 4096}},
	} {
		var got directive.Limits

		require.NoError(t, got.Set(tc.key, tc.value), "%s=%s", tc.key, tc.value)
		assert.Equal(t, tc.want, got, "%s=%s", tc.key, tc.value)
	}
}

func TestLimitsSetRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ key, value, want string }{
		{"cpu", "1", `unknown limit "cpu" (memory, timeout, pool)`},
		{"memory", "65535", "memory must be from 64KiB to 4GiB"},
		{"memory", "63KiB", "memory must be from 64KiB to 4GiB"},
		{"memory", "5GiB", "memory must be from 64KiB to 4GiB"},
		{"memory", "4097MiB", "memory must be from 64KiB to 4GiB"},
		{"memory", "64MB", "memory must be from 64KiB to 4GiB"},
		{"memory", "-1MiB", "memory must be from 64KiB to 4GiB"},
		{"memory", "MiB", "memory must be from 64KiB to 4GiB"},
		{"timeout", "2", "timeout must be a duration above zero (like 2s), not \"2\""},
		{"timeout", "0s", "timeout must be a duration above zero"},
		{"timeout", "-1s", "timeout must be a duration above zero"},
		{"pool", "0", "pool must be N or MIN..MAX with 1 <= MIN <= MAX <= 4096, not \"0\""},
		{"pool", "4..2", "pool must be N or MIN..MAX"},
		{"pool", "1..4097", "pool must be N or MIN..MAX"},
		{"pool", "1..", "pool must be N or MIN..MAX"},
		{"pool", "x", "pool must be N or MIN..MAX"},
	} {
		got := directive.Limits{Memory: 1 << 20, Timeout: time.Second, PoolMin: 1, PoolMax: 2}
		before := got

		err := got.Set(tc.key, tc.value)
		require.ErrorIs(t, err, directive.ErrDirective, "%s=%s", tc.key, tc.value)
		require.ErrorContains(t, err, tc.want, "%s=%s", tc.key, tc.value)
		assert.Equal(t, before, got, "a rejected value changes nothing")
	}
}

func TestLimitsOverrideAndString(t *testing.T) {
	t.Parallel()

	contract := directive.Limits{Memory: 64 << 20, Timeout: 2 * time.Second, PoolMin: 1, PoolMax: 8}

	assert.Equal(t, contract, contract.Override(directive.Limits{}), "an empty override keeps every limit")
	assert.Equal(t,
		directive.Limits{Memory: 16 << 20, Timeout: 2 * time.Second, PoolMin: 2, PoolMax: 2},
		contract.Override(directive.Limits{Memory: 16 << 20, PoolMin: 2, PoolMax: 2}),
		"set limits win; the pool is one limit")

	assert.Equal(t, "memory=64MiB timeout=2s pool=1..8", contract.String())

	for want, limits := range map[string]directive.Limits{
		"memory=100000 pool=3..3": {Memory: 100000, PoolMin: 3, PoolMax: 3},
		"memory=1GiB":             {Memory: 1 << 30},
		"none":                    {},
	} {
		assert.Equal(t, want, limits.String())
	}
}
