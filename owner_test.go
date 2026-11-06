package plg_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
)

// fullOwner is a plugin.yaml owner block setting every field, and the Owner it parses into.
const fullOwner = `owner:
  maintainers:
    - {name: Ana Lima, email: ana@example.com, handle: "@ana"}
    - name: Bo
  team: {name: payments, department: finance}
  org: commerce
  company: Acme Inc.
  contact: {oncall: payments-oncall, channel: "#payments", email: payments@example.com}
  links:
    repository: https://git.example.com/payments/rules
    docs: https://docs.example.com/rules
    runbook: https://runbooks.example.com/rules
    issues: https://git.example.com/payments/rules/issues
  labels: {tier: 1, cost-center: "42"}
`

func wantFullOwner() plg.Owner {
	return plg.Owner{
		Maintainers: []plg.Maintainer{{Name: "Ana Lima", Email: "ana@example.com", Handle: "@ana"}, {Name: "Bo"}},
		Team:        plg.Team{Name: "payments", Department: "finance"},
		Org:         "commerce",
		Company:     "Acme Inc.",
		Contact:     plg.Contact{Oncall: "payments-oncall", Channel: "#payments", Email: "payments@example.com"},
		Links: plg.Links{
			Repository: "https://git.example.com/payments/rules", Docs: "https://docs.example.com/rules",
			Runbook: "https://runbooks.example.com/rules", Issues: "https://git.example.com/payments/rules/issues",
		},
		Labels: map[string]string{"tier": "1", "cost-center": "42"},
	}
}

func TestParseManifestReadsTheOwner(t *testing.T) {
	t.Parallel()

	const base = "language: go\nentrypoint: .\n"

	tests := map[string]struct {
		manifest string
		want     plg.Owner
	}{
		"every field":    {manifest: base + fullOwner, want: wantFullOwner()},
		"no owner":       {manifest: base},
		"a null owner":   {manifest: base + "owner:\n"},
		"an empty owner": {manifest: base + "owner: {}\n"},
		"only the team": {
			manifest: base + "owner: {team: {name: core}}\n",
			want:     plg.Owner{Team: plg.Team{Name: "core"}},
		},
		"empty lists": {manifest: base + "owner: {maintainers: [], labels: {}}\n"},
		"maintainer only": {
			manifest: base + "owner: {maintainers: [{handle: x}]}\n",
			want:     plg.Owner{Maintainers: []plg.Maintainer{{Handle: "x"}}},
		},
		"at the limits": {
			manifest: base + "owner:\n  org: " + strings.Repeat("o", plg.MaxOwnerStringBytes) + "\n  maintainers:\n" +
				strings.Repeat(
					"    - {name: m}\n",
					plg.MaxOwnerMaintainers,
				) + "  labels:\n" + labels(plg.MaxOwnerLabels),
			want: plg.Owner{
				Org:         strings.Repeat("o", plg.MaxOwnerStringBytes),
				Maintainers: repeatMaintainer(plg.MaxOwnerMaintainers),
				Labels:      labelMap(plg.MaxOwnerLabels),
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			desc, err := plg.ParseManifest([]byte(tc.manifest))
			require.NoError(t, err)
			assertOwner(t, &tc.want, desc.Owner)

			descs, err := manifestStorage(tc.manifest).List(t.Context())
			require.NoError(t, err)
			require.Len(t, descs, 1)
			assertOwner(t, &tc.want, descs[0].Owner)
		})
	}
}

// assertOwner checks got is want, no owner being the same as an empty one and an empty slice or map the same as
// none.
func assertOwner(t *testing.T, want, got *plg.Owner) {
	t.Helper()

	var norm plg.Owner

	if got != nil {
		norm = *got
	}

	if len(norm.Maintainers) == 0 {
		norm.Maintainers = nil
	}

	if len(norm.Labels) == 0 {
		norm.Labels = nil
	}

	assert.Equal(t, *want, norm)
}

func labels(n int) string {
	var b strings.Builder

	for i := range n {
		fmt.Fprintf(&b, "    l%d: v\n", i)
	}

	return b.String()
}

func labelMap(n int) map[string]string {
	m := make(map[string]string, n)
	for i := range n {
		m[fmt.Sprintf("l%d", i)] = "v"
	}

	return m
}

func repeatMaintainer(n int) []plg.Maintainer {
	m := make([]plg.Maintainer, n)
	for i := range m {
		m[i] = plg.Maintainer{Name: "m"}
	}

	return m
}

func TestParseManifestEnforcesOwnerLimits(t *testing.T) {
	t.Parallel()

	const base = "language: go\nentrypoint: .\n"

	long := strings.Repeat("x", plg.MaxOwnerStringBytes+1)

	tests := map[string]string{
		"long team name":         "owner: {team: {name: " + long + "}}\n",
		"long department":        "owner: {team: {department: " + long + "}}\n",
		"long org":               "owner: {org: " + long + "}\n",
		"long company":           "owner: {company: " + long + "}\n",
		"long maintainer name":   "owner: {maintainers: [{name: " + long + "}]}\n",
		"long maintainer email":  "owner: {maintainers: [{email: " + long + "}]}\n",
		"long maintainer handle": "owner: {maintainers: [{handle: " + long + "}]}\n",
		"long oncall":            "owner: {contact: {oncall: " + long + "}}\n",
		"long channel":           "owner: {contact: {channel: " + long + "}}\n",
		"long contact email":     "owner: {contact: {email: " + long + "}}\n",
		"long repository":        "owner: {links: {repository: " + long + "}}\n",
		"long docs":              "owner: {links: {docs: " + long + "}}\n",
		"long runbook":           "owner: {links: {runbook: " + long + "}}\n",
		"long issues":            "owner: {links: {issues: " + long + "}}\n",
		"long label key":         "owner: {labels: {" + long + ": v}}\n",
		"long label value":       "owner: {labels: {k: " + long + "}}\n",
		"too many maintainers": "owner:\n  maintainers:\n" +
			strings.Repeat("    - {name: m}\n", plg.MaxOwnerMaintainers+1),
		"too many labels":     "owner:\n  labels:\n" + labels(plg.MaxOwnerLabels+1),
		"an empty maintainer": "owner: {maintainers: [{}]}\n",
		"an empty label key":  "owner: {labels: {\"\": v}}\n",
		"unknown owner key":   "owner: {teams: {name: x}}\n",
		"unknown team key":    "owner: {team: {lead: x}}\n",
		"unknown contact key": "owner: {contact: {pager: x}}\n",
		"maintainers a map":   "owner: {maintainers: {name: x}}\n",
		"team a string":       "owner: {team: payments}\n",
		"a label list":        "owner: {labels: {k: [a]}}\n",
		"a duplicate label":   "owner:\n  labels:\n    k: a\n    k: b\n",
		"owner a list":        "owner: [x]\n",
	}

	for name, owner := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := plg.ParseManifest([]byte(base + owner))
			require.ErrorIs(t, err, plg.ErrInvalidManifest)

			_, err = manifestStorage(base + owner).List(t.Context())
			require.ErrorIs(t, err, plg.ErrInvalidManifest)
		})
	}
}
