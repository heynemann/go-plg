//go:build integration

package cli_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg/internal/testproxy"
)

// repoDir is the testdata's author repository: plugins gold (Go), rules/silver (Lua) and coupon (TypeScript)
// importing the shared directory lib, and plugins plg.yaml does not list, which tests list in copies of it.
func repoDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.Abs(filepath.Join("..", "..", "testdata", "validate", "repo"))
	require.NoError(t, err)

	return dir
}

// copyRepo copies the testdata's author repository into a temporary directory, with plgYAML as its plg.yaml and
// the extra files (path relative to the repository: content), and returns its directory.
func copyRepo(t *testing.T, plgYAML string, files map[string]string) string {
	t.Helper()

	src, dst := repoDir(t), filepath.Join(t.TempDir(), "repo")

	require.NoError(t, filepath.WalkDir(src, func(name string, entry fs.DirEntry, err error) error {
		require.NoError(t, err)

		rel, err := filepath.Rel(src, name)
		require.NoError(t, err)

		if entry.IsDir() {
			require.NoError(t, os.MkdirAll(filepath.Join(dst, rel), 0o750))

			return nil
		}

		data, err := os.ReadFile(name) //nolint:gosec // G304: the testdata
		require.NoError(t, err)

		target := filepath.Join(dst, rel)
		require.NoError(t, os.WriteFile(target, data, 0o600)) //nolint:gosec // G703: the test's directory

		return nil
	}))

	files["plg.yaml"] = plgYAML
	for name, content := range files {
		path := filepath.Join(dst, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}

	return dst
}

// section returns what plg validate printed for the plugin listed as plugin in a repository: its steps' table.
func section(t *testing.T, stdout, plugin string) string {
	t.Helper()

	_, after, found := strings.Cut(stdout, plugin+"\nSTEP ")
	require.True(t, found, "no section for %s in\n%s", plugin, stdout)

	steps, _, _ := strings.Cut(after, "\n\n")

	return steps
}

// TestValidateRepoValidatesEveryPlugin validates the testdata's author repository: each plugin it lists, in Go,
// Lua and TypeScript, gets its section and passes, the shared directory copied into its files where its language
// imports it (a Go plugin's vendor directory at the shared package's import path, type-checked with the plugin;
// the plugin's root for Lua's require and TypeScript's relative import), and Create, which calls the shared code,
// runs in the real guests; a summary closes the output. A plugin of the repository validated alone gets the
// repository's settings; one the repository does not list is validated on its own, with a warning.
func TestValidateRepoValidatesEveryPlugin(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	repo := repoDir(t)
	settings := "plg.yaml   ok      " + filepath.Join(repo, "plg.yaml") + "; shared lib\n"

	got := validate(environ, repo)
	require.Equal(t, 0, got.code, "%s%s", got.stdout, got.stderr)

	gold := section(t, got.stdout, "gold")
	for _, want := range []string{
		settings,
		"shared     ok      copied lib at vendor/example.com/rewardsrepo/lib\n",
		"go.mod     ok      " + filepath.Join(repo, "go.mod") + " requires example.com/rewards v1.2.0\n",
		"types      ok      the plugin's package type-checks against cohort.Provider\n",
		"load       ok      Create ran in the go guest (version ",
	} {
		assert.Contains(t, gold, want)
	}

	for plugin, language := range map[string]string{"rules/silver": "lua", "coupon": "typescript"} {
		steps := section(t, got.stdout, plugin)
		assert.Contains(t, steps, settings, plugin)
		assert.Contains(t, steps, "shared     ok      copied lib at lib\n", plugin)
		assert.Contains(t, steps, "load       ok      Create ran in the "+language+" guest (version ", plugin)
	}

	assert.True(t, strings.HasSuffix(got.stdout, "\n\nPLUGIN        RESULT  DETAIL\n"+
		"gold          ok      \n"+
		"rules/silver  ok      \n"+
		"coupon        ok      \n"), got.stdout)
	assert.NotContains(t, got.stdout, "warning")

	alone := validate(environ, filepath.Join(repo, "rules", "silver"))
	require.Equal(t, 0, alone.code, "%s%s", alone.stdout, alone.stderr)
	assert.Contains(t, alone.stdout, "STEP       RESULT  DETAIL\n"+settings+
		"manifest   ok      lua plugin silver implementing example.com/rewards/cohort.Provider@v1.2.0\n"+
		"shared     ok      copied lib at lib\n")
	assert.NotContains(t, alone.stdout, "PLUGIN", "a plugin alone has no summary")

	unlisted := validate(environ, filepath.Join(repo, "hungry"))
	require.Equal(t, 0, unlisted.code, "%s%s", unlisted.stdout, unlisted.stderr)
	assert.Contains(t, unlisted.stdout, "plg.yaml   warning  "+filepath.Join(repo, "plg.yaml")+" does not list "+
		"this plugin: it is validated without the repository's settings\n")
	assert.NotContains(t, unlisted.stdout, "shared ")
	assert.Contains(t, unlisted.stdout, "Create ran in the lua guest", "within Lua's default 64 MiB")
}

// TestValidateRepoAppliesItsSettings validates plugins under a plg.yaml setting limits and requiring ownership:
// the memory limit fails the plugin whose Create holds 24 MiB, the timeout stops the one whose Create never
// returns, and the plugin without an owner fails its manifest step, before anything is loaded; the summary names
// the step each failed at and plg validate exits 1. The same plugins pass without those settings (but the one never
// returning), the missing owner a warning.
func TestValidateRepoAppliesItsSettings(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	strict := copyRepo(t, "plugins: [hungry, spins, unowned]\nlimits: {memory: 16MiB, timeout: 500ms, pool: 1..2}\n"+
		"requireOwnership: true\n", map[string]string{})

	got := validate(environ, strict)
	assert.Equal(t, 1, got.code)
	assert.Equal(t, "plg validate: 3 of the 3 plugins "+filepath.Join(strict, "plg.yaml")+" lists are not valid: "+
		"hungry, spins, unowned\n", got.stderr[strings.LastIndex(strings.TrimSuffix(got.stderr, "\n"), "\n")+1:])

	for _, plugin := range []string{"hungry", "spins"} {
		steps := section(t, got.stdout, plugin)
		assert.Contains(t, steps, "plg.yaml   ok      "+filepath.Join(strict, "plg.yaml")+"; ownership required\n",
			plugin)
		assert.Contains(t, steps, "limits     ok      memory=16MiB timeout=500ms pool=1..2 (plg.yaml)\n", plugin)
	}

	assert.Contains(t, section(t, got.stdout, "hungry"), "load       FAIL    plg: validate/hungry: create failed: "+
		"not enough memory")
	assert.Contains(t, section(t, got.stdout, "spins"), "load       FAIL    ")
	assert.Contains(t, section(t, got.stdout, "spins"), "deadline exceeded")
	assert.Equal(t, "     RESULT  DETAIL\n"+ // after "STEP "
		"plg.yaml  ok      "+filepath.Join(strict, "plg.yaml")+"; ownership required\n"+
		"manifest  FAIL    lua plugin unowned implementing example.com/rewards/cohort.Provider@v1.2.0\n"+
		"          error   plg.ownership.missing (owner): plugin.yaml's owner lacks team.name and one of "+
		"contact.oncall, contact.channel, contact.email", section(t, got.stdout, "unowned"))
	assert.True(t, strings.HasSuffix(got.stdout, "PLUGIN   RESULT  DETAIL\n"+
		"hungry   FAIL    failed at load\n"+
		"spins    FAIL    failed at load\n"+
		"unowned  FAIL    failed at manifest\n"), got.stdout)

	lenient := copyRepo(t, "plugins: [hungry, unowned]\n", map[string]string{})

	got = validate(environ, lenient)
	require.Equal(t, 0, got.code, "%s%s", got.stdout, got.stderr)
	assert.Contains(t, section(t, got.stdout, "hungry"), "limits     ok      memory=64MiB pool=1..")
	assert.Contains(t, section(t, got.stdout, "hungry"), ", no timeout (plg's defaults)\n")
	assert.Contains(t, section(t, got.stdout, "unowned"), "           warning  plg.ownership.missing (owner): ")
	assert.True(t, strings.HasSuffix(got.stdout, "PLUGIN   RESULT  DETAIL\n"+
		"hungry   ok      \n"+
		"unowned  ok      1 warning\n"), got.stdout)
}

// TestValidateRepoTypeChecksOnlyWhatThePluginImports validates the TypeScript plugin coupon of a repository whose
// shared directory also holds a file written for another contract's typings: coupon never imports it, so its
// type-check passes.
func TestValidateRepoTypeChecksOnlyWhatThePluginImports(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())

	repo := copyRepo(t, "plugins: [coupon]\nshared: [lib]\n", map[string]string{
		"lib/pricing.ts": "export const none: pricing.Order | null = null;\n",
	})

	got := validate(environ, repo)
	require.Equal(t, 0, got.code, "%s%s", got.stdout, got.stderr)
	assert.Contains(t, section(t, got.stdout, "coupon"), "types      ok      ")
}

// TestValidateRepoReplacesTheContractByALocalCheckout validates plugins of a repository whose plg.yaml replaces
// the contract module by a local checkout declaring a method v1.2.0 lacks: no module is fetched (GOPROXY=off, an
// empty module cache), the interface is the checkout's, a plugin implementing it passes with the warnings that its
// version is not known, and a Go plugin written against v1.2.0 fails its type-check. A checkout without the
// contract's package fails the contract step.
func TestValidateRepoReplacesTheContractByALocalCheckout(t *testing.T) {
	t.Parallel()

	checkout, err := filepath.Abs(filepath.Join("..", "..", "testdata", "validate", "checkout", "rewards"))
	require.NoError(t, err)

	repo := copyRepo(t, "plugins: [describes, gold]\nshared: [lib]\nreplace: {example.com/rewards: "+checkout+"}\n",
		map[string]string{})

	got := validate(testproxy.Environ("off", t.TempDir()), repo)
	assert.Equal(t, 1, got.code, "%s%s", got.stdout, got.stderr)

	replaced := "contract   warning  module example.com/rewards replaced by the directory " + checkout +
		" (plg.yaml): the pin v1.2.0 is not checked against it\n"
	describes := section(t, got.stdout, "describes")

	for _, want := range []string{
		"plg.yaml   ok       " + filepath.Join(repo, "plg.yaml") + "; shared lib; replace example.com/rewards => " +
			checkout + "\n",
		replaced,
		"interface  ok       cohort.Provider: Describe, IsMember, List; Host cohort.Users: Tier (a stub: every call " +
			"fails)\n",
		"load       ok       Create ran in the lua guest (version ",
		"           warning  plg.contract.devel",
	} {
		assert.Contains(t, describes, want)
	}

	gold := section(t, got.stdout, "gold")
	assert.Contains(t, gold, replaced)
	assert.Contains(t, gold, "types      FAIL     the plugin does not type-check against the contract: plugin.go:")
	assert.Contains(t, gold, "(missing method Describe)")
	assert.True(t, strings.HasSuffix(got.stdout, "PLUGIN     RESULT  DETAIL\n"+
		"describes  ok      2 warnings\n"+
		"gold       FAIL    failed at types\n"), got.stdout)

	empty := copyRepo(t, "plugins: [describes]\nreplace: {example.com/rewards: checkout}\n", map[string]string{
		"checkout/go.mod": "module example.com/rewards\n",
	})

	got = validate(testproxy.Environ("off", t.TempDir()), empty)
	assert.Equal(t, 1, got.code)
	assert.Contains(t, got.stdout, "contract  FAIL    no module provides the contract package "+
		"example.com/rewards/cohort: plg.yaml replaces example.com/rewards by "+filepath.Join(empty, "checkout")+
		", which has no directory "+filepath.Join(empty, "checkout", "cohort")+"\n")
}

// TestValidateNamesThePinOrRequirementToChange validates Go plugins whose plugin.yaml pin and repository go.mod
// requirement differ: the older of the two is the one to change, and the go.mod step says which and how.
func TestValidateNamesThePinOrRequirementToChange(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	repo := repoDir(t)

	got := validate(environ, filepath.Join(repo, "goneedsnewer"))
	assert.Equal(t, 1, got.code)
	assert.Contains(t, got.stdout, "go.mod     FAIL     the plugin's go.mod does not require the pinned contract "+
		"version: "+filepath.Join(repo, "go.mod")+" requires example.com/rewards v1.2.0, plugin.yaml pins v1.3.0: "+
		"raise the requirement in "+filepath.Join(repo, "go.mod")+" to example.com/rewards v1.3.0 (go get "+
		"example.com/rewards@v1.3.0)\n")

	newer := copyRepo(t, "plugins: [gold]\nshared: [lib]\n", map[string]string{
		"go.mod": "module example.com/rewardsrepo\n\ngo 1.27\n\nrequire example.com/rewards v1.3.0\n",
	})

	got = validate(environ, newer)
	assert.Equal(t, 1, got.code)
	assert.Contains(t, section(t, got.stdout, "gold"), "go.mod     FAIL    the plugin's go.mod does not require the "+
		"pinned contract version: "+filepath.Join(newer, "go.mod")+" requires example.com/rewards v1.3.0, plugin.yaml "+
		"pins v1.2.0: raise the pin in "+filepath.Join(newer, "gold", "plugin.yaml")+" to "+
		"example.com/rewards/cohort.Provider@v1.3.0")
}

// TestValidateRepoRefusesAnInvalidPlgYAML validates a repository whose plg.yaml breaks its rules, and a plugin of
// it: both fail naming the problem, the repository at its plg.yaml step, before any plugin. A plugin holding a file
// where a shared directory goes fails the shared step.
func TestValidateRepoRefusesAnInvalidPlgYAML(t *testing.T) {
	t.Parallel()

	repo := copyRepo(t, "plugins: [gold]\nshared: [nosuch]\n", map[string]string{})

	got := validate(nil, repo)
	assert.Equal(t, 1, got.code)
	assert.Equal(t, repo+"\n"+
		"STEP      RESULT  DETAIL\n"+
		"plg.yaml  FAIL    invalid plg.yaml: "+filepath.Join(repo, "plg.yaml")+": shared: stat "+
		filepath.Join(repo, "nosuch")+": no such file or directory\n"+
		"\n"+
		"PLUGIN"+strings.Repeat(" ", len(repo)-len("PLUGIN"))+"  RESULT  DETAIL\n"+
		repo+"  FAIL    failed at plg.yaml\n", got.stdout)
	assert.Contains(t, got.stderr, "plg validate: invalid plg.yaml: "+filepath.Join(repo, "plg.yaml")+": shared: "+
		"stat "+filepath.Join(repo, "nosuch")+": no such file or directory\n")

	got = validate(nil, filepath.Join(repo, "gold"))
	assert.Equal(t, 1, got.code)
	assert.Contains(t, got.stdout, "plg.yaml  FAIL    invalid plg.yaml: "+filepath.Join(repo, "plg.yaml")+": ")
	assert.NotContains(t, got.stdout, "manifest", "no step runs after the failed one")

	clash := copyRepo(t, "plugins: [coupon]\nshared: [lib]\n", map[string]string{"coupon/lib": "not the shared lib"})

	got = validate(nil, clash)
	assert.Equal(t, 1, got.code)
	assert.Contains(t, got.stdout, "shared    FAIL    cannot copy a shared directory into the plugin: coupon: the "+
		"plugin already has lib, where lib is copied\n")
	assert.Contains(t, got.stdout, "coupon  FAIL    failed at shared\n")
}
