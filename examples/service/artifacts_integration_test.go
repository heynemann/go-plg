//go:build integration

package main

import (
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/examples/internal/authorrepo"
)

// adminProvenance is the part of an artifact's provenance the admin handler shows that this test reads.
type adminProvenance struct {
	Contract adminContract `json:"contract"`
	Digest   string        `json:"digest"`
	Commit   string        `json:"commit"`
	Dirty    bool          `json:"dirty"`
}

// adminContract is the contract module an artifact was validated against.
type adminContract struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// The example author repository's CI script validates and packs its three rules (Go, Lua, TypeScript), and the
// service serves those artifacts: each rule prices orders, after its own Validate passed the service's limits and
// with its owner named, and the admin handler shows the commit and the contract each was packed from.
func TestServiceServesPackedArtifacts(t *testing.T) {
	t.Parallel()

	dist := authorrepo.Pack(t)

	entries, err := os.ReadDir(dist)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	assert.ElementsMatch(t, []string{"bundle.plg", "seasonal.plg", "vip.plg"}, names, "an artifact per plugin")

	url := start(t, "-artifacts", dist, "-admin-token", adminToken)

	got := quoteFor(t, url, `{"customer":"ada","coupon":"SUMMER","items":[`+
		`{"sku":"tea","quantity":10,"unitCents":200},{"sku":"mug","quantity":1,"unitCents":1000},`+
		`{"sku":"spoon","quantity":2,"unitCents":500}]}`)
	assert.Equal(t, quote{
		Applied: "pricing/seasonal", SubtotalCents: 4000, TotalCents: 3680,
		Discounts: []ruleResult{
			{Rule: "pricing/bundle", Cents: 200, Reason: "3 products: 5% off"},
			{Rule: "pricing/seasonal", Cents: 320, Reason: "coupon SUMMER: 8% off"},
			{Rule: "pricing/vip", Cents: 300, Reason: "gold customer: 300 cents off"},
		},
	}, got, "every packed rule prices the order")

	answer := adminDo(t, http.MethodGet, url+"/admin/plg/plugins/pricing/seasonal", adminToken, http.NoBody)
	require.Equal(t, http.StatusOK, answer.status, string(answer.body))

	var state struct {
		Provenance *adminProvenance `json:"provenance"`
		Owner      adminOwner       `json:"owner"`
		Status     string           `json:"status"`
	}

	answer.decode(t, &state)
	assert.Equal(t, "serving", state.Status)
	assert.Equal(t, "promotions", state.Owner.Team.Name)
	require.NotNil(t, state.Provenance, "loaded from an artifact")
	assert.Equal(t, "github.com/heynemann/go-plg/examples", state.Provenance.Contract.Path)
	assert.Equal(t, "v1.0.0", state.Provenance.Contract.Version)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, state.Provenance.Digest)
	assert.Regexp(t, `^[0-9a-f]{40}$`, state.Provenance.Commit, "packed from the repository's commit")
	assert.False(t, state.Provenance.Dirty, "the CI script's reports and artifacts are not the plugin's files")
}
