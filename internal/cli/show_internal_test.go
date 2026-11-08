package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/adminjson"
)

// TestDescribeShowsTheProvenance shows the provenance of a plugin loaded from an artifact on a row of plg status's
// table: its commit and whether the files differed from it (or that there was no commit), its contract module,
// the plg that packed it and its digest. A plugin without provenance has no such row.
func TestDescribeShowsTheProvenance(t *testing.T) {
	t.Parallel()

	provenance := &adminjson.Provenance{
		Contract: adminjson.Module{Path: "example.com/rewards", Version: "v1.2.0"},
		Digest:   "sha256:00ff", Commit: "0123abcd", Plg: "v0.9.0", Dirty: true,
	}
	plugin := adminjson.Plugin{Key: "rules/silver", Registration: "rules", Status: "serving", Provenance: provenance}

	for _, tc := range []struct {
		commit string
		want   string
		dirty  bool
	}{
		{"0123abcd", "commit 0123abcd (dirty)", true},
		{"0123abcd", "commit 0123abcd", false},
		{"", "no git commit", false},
	} {
		provenance.Commit, provenance.Dirty = tc.commit, tc.dirty

		var out bytes.Buffer

		require.NoError(t, describe(&out, &plugin))
		assert.Contains(t, out.String(), "\nprovenance    "+tc.want+", contract example.com/rewards v1.2.0, packed "+
			"by plg v0.9.0, digest sha256:00ff\n")
	}

	plugin.Provenance = nil

	var out bytes.Buffer

	require.NoError(t, describe(&out, &plugin))
	assert.NotContains(t, out.String(), "provenance")
}
