package contractref_test

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/contractref"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestParseSplitsPackageInterfaceAndVersion(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]contractref.Ref{
		"example.com/contracts/rewards.Cohort@v1.4.0": {
			Package: "example.com/contracts/rewards", Interface: "Cohort", Version: "v1.4.0",
		},
		"example.com/contracts/v2/rewards.Cohort@v2.0.0-rc.1": {
			Package: "example.com/contracts/v2/rewards", Interface: "Cohort", Version: "v2.0.0-rc.1",
		},
		"example.com/contracts.Rule@v0.0.0-20261108120000-0123456789ab": {
			Package: "example.com/contracts", Interface: "Rule", Version: "v0.0.0-20261108120000-0123456789ab",
		},
		"contracts.rule@v1.0.0":          {Package: "contracts", Interface: "rule", Version: "v1.0.0"},
		"example.com/a.b/c.Iface@v3.2.1": {Package: "example.com/a.b/c", Interface: "Iface", Version: "v3.2.1"},
	} {
		ref, err := contractref.Parse(input)
		require.NoError(t, err, input)
		assert.Equal(t, want, ref, input)
		assert.Equal(t, input, ref.String(), "String is what Parse reads")
		assert.Equal(t, want.Package+"."+want.Interface, ref.Name())
	}
}

func TestParseRejectsMalformedReferences(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"":                              "implements is required",
		"example.com/c.I":               `implements "example.com/c.I": it must end with "@"`,
		"example.com/c.I@":              `the version must be a canonical semantic version, as in v1.4.0, not ""`,
		"example.com/c.I@1.0.0":         `not "1.0.0"`,
		"example.com/c.I@v1.0":          `not "v1.0"`,
		"example.com/c.I@v1":            `not "v1"`,
		"example.com/c.I@v1.0.0+build":  `not "v1.0.0+build"`,
		"example.com/c.I@latest":        `not "latest"`,
		"example.com/c@v1.0.0":          "it must name the interface after its package path",
		"Iface@v1.0.0":                  "it must name the interface after its package path",
		"example.com/c.@v1.0.0":         `the interface name must be a Go identifier, not ""`,
		"/c.I@v1.0.0":                   `malformed import path "/c": empty path element`,
		"example.com/c d.I@v1.0.0":      `malformed import path "example.com/c d"`,
		"example.com//c.I@v1.0.0":       `malformed import path "example.com//c"`,
		"example.com/c.I@v1.0.0@v2.0.0": `not "v1.0.0@v2.0.0"`,
	} {
		_, err := contractref.Parse(input)
		require.ErrorContains(t, err, want, input)
	}
}

// buildInfo is a program's build info: a main module and dependencies, some replaced.
func buildInfo() *debug.BuildInfo {
	return &debug.BuildInfo{
		Main: debug.Module{Path: "example.com/service", Version: "(devel)"},
		Deps: []*debug.Module{
			{Path: "example.com/contracts", Version: "v1.4.0"},
			{Path: "example.com/contracts/rewards", Version: "v2.1.0"}, // a nested module
			{
				Path:    "example.com/local",
				Version: "v1.0.0",
				Replace: &debug.Module{
					Path:    "../local",
					Version: "(devel)",
				}, // replaced by a directory, as go records it
			},
			{
				Path: "example.com/bare", Version: "v1.0.0",
				Replace: &debug.Module{Path: "/src/bare"}, // a directory replacement without a version
			},
			{
				Path: "example.com/forked", Version: "v1.5.0",
				Replace: &debug.Module{Path: "example.com/fork", Version: "v1.3.2"}, // by another module version
			},
			{Path: "example.com/stamped", Version: ""}, // no version recorded
		},
	}
}

func TestModuleOfFindsThePackagesModule(t *testing.T) {
	t.Parallel()

	for pkg, want := range map[string]contractref.Module{
		"example.com/contracts":              {Path: "example.com/contracts", Version: "v1.4.0"},
		"example.com/contracts/billing":      {Path: "example.com/contracts", Version: "v1.4.0"},
		"example.com/contracts/rewards":      {Path: "example.com/contracts/rewards", Version: "v2.1.0"},
		"example.com/contracts/rewards/tier": {Path: "example.com/contracts/rewards", Version: "v2.1.0"},
		"example.com/service/contract":       {Path: "example.com/service", Version: contractref.Devel},
		"example.com/service/contract_test":  {Path: "example.com/service", Version: contractref.Devel},
		"example.com/local/api":              {Path: "example.com/local", Version: contractref.Devel, Dir: "../local"},
		"example.com/bare":                   {Path: "example.com/bare", Version: contractref.Devel, Dir: "/src/bare"},
		"example.com/forked/api":             {Path: "example.com/forked", Version: "v1.3.2"},
		"example.com/stamped/api":            {Path: "example.com/stamped", Version: contractref.Devel},
	} {
		got, found := contractref.ModuleOf(buildInfo(), pkg)
		require.True(t, found, pkg)
		assert.Equal(t, want, got, pkg)
		assert.Equal(t, want.Version == contractref.Devel, got.Devel(), pkg)
	}

	for _, pkg := range []string{"example.com/contractsx", "example.com/other", "example.com/other_test", ""} {
		_, found := contractref.ModuleOf(buildInfo(), pkg)
		assert.False(t, found, "no module provides %q", pkg)
	}

	_, found := contractref.ModuleOf(&debug.BuildInfo{}, "example.com/x")
	assert.False(t, found, "a build info without modules provides nothing")
}

// A main module built in a repository is stamped from version control: a tag of a clean checkout is a release, an
// untagged commit (a pseudo-version) or uncommitted changes (+dirty) a local checkout. A dependency's
// pseudo-version is a real pin.
func TestModuleOfTellsAReleasedMainModuleFromACheckout(t *testing.T) {
	t.Parallel()

	for stamped, want := range map[string]string{
		"v1.4.0":                             "v1.4.0",
		"v1.4.0-rc.1":                        "v1.4.0-rc.1",
		"v1.4.0+dirty":                       contractref.Devel,
		"v0.0.0-20261108044727-011982b7d8e8": contractref.Devel,
		"v1.4.1-0.20261108044727-011982b7d8e8+dirty": contractref.Devel,
	} {
		info := &debug.BuildInfo{
			Main: debug.Module{Path: "example.com/service", Version: stamped},
			Deps: []*debug.Module{{Path: "example.com/contracts", Version: "v1.4.1-0.20261108044727-011982b7d8e8"}},
		}

		got, found := contractref.ModuleOf(info, "example.com/service/contract")
		require.True(t, found, stamped)
		assert.Equal(t, contractref.Module{Path: "example.com/service", Version: want}, got, stamped)

		dep, found := contractref.ModuleOf(info, "example.com/contracts")
		require.True(t, found, stamped)
		assert.Equal(t, "v1.4.1-0.20261108044727-011982b7d8e8", dep.Version, "a dependency's pseudo-version is kept")
	}
}

// TestBuiltReadsTheRunningProgramsBuildInfo reads the test binary's own build info: its main module is this
// repository, built from a checkout, and a package no module provides has no version either.
func TestBuiltReadsTheRunningProgramsBuildInfo(t *testing.T) {
	t.Parallel()

	assert.Equal(t, contractref.Module{Path: "github.com/heynemann/go-plg", Version: contractref.Devel},
		contractref.Built("github.com/heynemann/go-plg/internal/testcontract"))
	assert.Equal(t, contractref.Module{Path: "github.com/stretchr/testify", Version: testifyVersion(t)},
		contractref.Built("github.com/stretchr/testify/assert"), "a dependency, at its version")
	assert.Equal(t, contractref.Module{Path: "example.com/nowhere", Version: contractref.Devel},
		contractref.Built("example.com/nowhere"))
}

// testifyVersion returns the version of testify the test binary is built with.
func testifyVersion(t *testing.T) string {
	t.Helper()

	bi, ok := debug.ReadBuildInfo()
	require.True(t, ok)

	for _, dep := range bi.Deps {
		if dep.Path == "github.com/stretchr/testify" {
			return dep.Version
		}
	}

	require.Fail(t, "testify is not in the build info")

	return ""
}

func TestCompatibleNeedsTheSameMajorAndNoNewerMinor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ built, pinned string }{
		{"v1.4.0", "v1.4.0"},
		{"v1.4.0", "v1.0.0"},
		{"v1.4.0", "v1.4.9"},      // a newer patch is a fix, not an addition
		{"v1.4.2", "v1.4.0-rc.1"}, // a prerelease of the same minor
		{"v1.5.0-rc.1", "v1.5.0"}, // the service's prerelease of that minor
		{"v0.3.0", "v0.2.7"},      // v0 follows the same rule
		{"v2.0.0", "v2.0.0"},
		{"v1.4.0", "v1.4.0-0.20261108120000-0123456789ab"}, // a pseudo-version of the same minor
	} {
		require.NoError(t, contractref.Compatible(tc.built, tc.pinned), "built %s, pinned %s", tc.built, tc.pinned)
	}

	for _, tc := range []struct{ built, pinned, want string }{
		{"v1.4.0", "v2.0.0", "the plugin is written against v2.0.0, a major version other than the service's v1.4.0"},
		{"v2.1.0", "v1.9.0", "the plugin is written against v1.9.0, a major version other than the service's v2.1.0"},
		{"v1.4.0", "v1.5.0", "the plugin is written against v1.5.0, a minor version newer than the service's v1.4.0"},
		{"v0.3.0", "v0.4.0", "the plugin is written against v0.4.0, a minor version newer than the service's v0.3.0"},
		{"v0.3.0", "v1.0.0", "the plugin is written against v1.0.0, a major version other than the service's v0.3.0"},
	} {
		err := contractref.Compatible(tc.built, tc.pinned)
		require.ErrorIs(t, err, contractref.ErrIncompatible)
		require.EqualError(t, err, "incompatible contract version: "+tc.want)
	}
}
