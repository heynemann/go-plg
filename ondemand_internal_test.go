package plg

import (
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validatedGeneration returns a generation, served by no catalogue, of a registration validating under policy.
func validatedGeneration(policy ValidationPolicy) *plugin {
	gen := testGeneration()
	gen.reg = &registration{validation: &validation{policy: policy}, keys: xsync.NewMap[string, *listedPlugin]()}
	gen.launch = &launcher{id: pluginID{owner: noOwner, key: gen.key}}

	return gen
}

// TestOnDemandValidationKeepsTheLatestStarted hands a generation the reports of on-demand validations finishing
// out of order: the one started last is kept, and an earlier one finishing later neither replaces it nor applies
// the policy.
func TestOnDemandValidationKeepsTheLatestStarted(t *testing.T) {
	t.Parallel()

	ctr := New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(t.Context())) })

	gen := validatedGeneration(Unpublish)
	later := &Report{At: time.Unix(2, 0)}

	assert.False(t, ctr.validated(t.Context(), gen, later))
	assert.False(t, ctr.validated(t.Context(), gen, &Report{At: time.Unix(1, 0), Err: ErrBlockingIssue}),
		"an earlier validation finishing later is not applied")
	assert.Same(t, later, gen.validated.Load())
	assert.False(t, gen.retiring.Load())
}

// TestOnDemandUnpublishLeavesAReplacedVersion fails an on-demand validation of a generation the catalogue no
// longer serves (a reload replaced or removed it meanwhile): the report is kept with it, and nothing is
// unpublished, retired or rejected.
func TestOnDemandUnpublishLeavesAReplacedVersion(t *testing.T) {
	t.Parallel()

	ctr := New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(t.Context())) })

	gen := validatedGeneration(Unpublish)
	entry := gen.reg.entry(gen.key) // its storages still list it
	failed := &Report{At: time.Now(), Err: ErrBlockingIssue}

	assert.False(t, ctr.validated(t.Context(), gen, failed))
	assert.Same(t, failed, gen.validated.Load())
	assert.False(t, gen.retiring.Load())
	assert.Nil(t, entry.rejection.Load())
	assert.Zero(t, ctr.retired.Size())
}

// TestRetiringAGenerationTwiceClosesItOnce retires a generation twice, as an on-demand Unpublish racing a reload
// may: its instances are closed once and the second retirement does nothing.
func TestRetiringAGenerationTwiceClosesItOnce(t *testing.T) {
	t.Parallel()

	ctr := New()

	t.Cleanup(func() { assert.NoError(t, ctr.Close(t.Context())) })

	spawner := &fakeSpawner{}
	gen := testGeneration()
	gen.pool = warmPool(t, spawner, &poolConfig{minSize: 1, maxSize: 1})

	ctr.retire(t.Context(), gen)
	ctr.retire(t.Context(), gen)

	require.NoError(t, ctr.awaitRetired(t.Context()))
	<-gen.closed
	require.NoError(t, gen.closeErr)
	assert.Equal(t, int32(1), spawner.closed.Load(), "its instance was closed once")
	assert.Zero(t, ctr.retired.Size())
}
