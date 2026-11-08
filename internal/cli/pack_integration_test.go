//go:build integration

package cli_test

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/cli"
	"github.com/heynemann/go-plg/internal/testproxy"
)

// pack runs plg pack with args in the environment environ, from the testdata's plugins directory.
func pack(environ []string, args ...string) validated {
	return packIn(environ, filepath.Join("..", "..", "testdata", "validate", "plugins"), args...)
}

// packIn runs plg pack with args in the environment environ, from the directory dir.
func packIn(environ []string, dir string, args ...string) validated {
	var stdout, stderr bytes.Buffer

	code := cli.Main(append([]string{"pack"}, args...), &cli.Env{
		Stdout: &stdout, Stderr: &stderr, Getenv: func(string) string { return "" },
		Environ: func() []string { return environ }, Dir: dir,
	})

	return validated{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// assertEndsWith checks that got ends with want: what plg pack says comes after what the plugins' loads logged.
func assertEndsWith(t *testing.T, got, want string) {
	t.Helper()

	assert.True(t, strings.HasSuffix(got, want), "%q does not end with %q", got, want)
}

// repoYAML is the testdata repository's plg.yaml.
func repoYAML(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(repoDir(t), "plg.yaml"))
	require.NoError(t, err)

	return string(data)
}

// openPacked opens the artifact name.
func openPacked(t *testing.T, name string) *plg.Artifact {
	t.Helper()

	data, err := os.ReadFile(name) //nolint:gosec // G304: the test's artifact
	require.NoError(t, err)

	art, err := plg.OpenArtifact(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	return art
}

// packedReport decodes the validate report an artifact's provenance holds.
func packedReport(t *testing.T, art *plg.Artifact) map[string]any {
	t.Helper()

	var report map[string]any

	require.NoError(t, sonic.ConfigStd.Unmarshal(art.Provenance().Report, &report))

	return report
}

// artifactsIn lists the files of dir; none when it does not exist.
func artifactsIn(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}

	require.NoError(t, err)

	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}

	return names
}

// TestPackWritesReproducibleArtifacts packs the testdata's author repository (a Go, a Lua and a TypeScript plugin
// using its shared directory) twice, into two directories: plg pack validates every plugin as plg validate does
// (its table, then the summary), writes <key>.plg for each and names them with their digests, and the two runs'
// artifacts are byte for byte the same. Each artifact opens with plg.OpenArtifact: the plugin's files with the
// shared directory where validate copies it, and its provenance: the files' digest, no commit (the copy is in no
// git repository), the contract module and version the plugin pins, this plg's version and validate's report of
// the plugin, without the resource report.
func TestPackWritesReproducibleArtifacts(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	repo := copyRepo(t, repoYAML(t), map[string]string{"rules/silver/tests/tiers.yaml": silverCases})
	first, second := t.TempDir(), t.TempDir()

	packed := pack(environ, "-o", first, repo)
	require.Equal(t, 0, packed.code, "%s%s", packed.stdout, packed.stderr)
	assert.Contains(t, packed.stdout, "PLUGIN        RESULT  DETAIL\ngold          ok      \n")
	assert.NotContains(t, packed.stdout, "resources", "the resource report changes from run to run")

	again := pack(environ, "-o", second, repo)
	require.Equal(t, 0, again.code, "%s%s", again.stdout, again.stderr)

	keys := map[string]string{"gold": "gold", "silver": "rules/silver", "coupon": "coupon"}

	assert.ElementsMatch(t, []string{"gold.plg", "silver.plg", "coupon.plg"}, artifactsIn(t, first))

	for key, listed := range keys {
		name := filepath.Join(first, key+".plg")
		data, err := os.ReadFile(name) //nolint:gosec // G304: the test's artifact
		require.NoError(t, err)

		other, err := os.ReadFile(filepath.Join(second, key+".plg")) //nolint:gosec // G304: as above
		require.NoError(t, err)
		assert.Equal(t, data, other, "%s: the same input packs to the same bytes", key)

		art := openPacked(t, name)
		provenance := art.Provenance()
		assert.Contains(t, packed.stderr, "plg pack: wrote "+name+" ("+provenance.Digest+")\n")
		assert.Equal(t, plg.ContractModule{Path: "example.com/rewards", Version: "v1.2.0"}, provenance.Contract, key)
		assert.Equal(t, "(devel)", provenance.PlgVersion, "%s: the test binary's plg, a checkout", key)
		assert.Empty(t, provenance.Commit, key)
		assert.False(t, provenance.Dirty, key)

		report := packedReport(t, art)
		assert.Equal(t, "plg.yaml", report["repository"], key)
		assert.NotContains(t, string(provenance.Report), repo, "%s: the report's paths are relative to the repository",
			key)
		assert.Equal(t, true, report["valid"], key)

		plugins, ok := report["plugins"].([]any)
		require.True(t, ok, key)
		require.Len(t, plugins, 1, key)

		plugin, ok := plugins[0].(map[string]any)
		require.True(t, ok, key)
		assert.Equal(t, listed, plugin["plugin"], key)
		assert.NotContains(t, plugin, "resources", key)

		manifest, err := fs.ReadFile(art, "plugin.yaml")
		require.NoError(t, err)

		desc, err := plg.ParseManifest(manifest)
		require.NoError(t, err)
		assert.Equal(t, "example.com/rewards/cohort.Provider@v1.2.0", desc.Implements, key)
	}

	for name, file := range map[string]string{
		"gold":   "vendor/example.com/rewardsrepo/lib/tiers.go",
		"silver": "lib/tiers.lua",
		"coupon": "lib/tiers.ts",
	} {
		_, err := fs.Stat(openPacked(t, filepath.Join(first, name+".plg")), file)
		require.NoError(t, err, "%s holds the shared %s", name, file)
	}

	var silver packedSteps

	art := openPacked(t, filepath.Join(first, "silver.plg"))
	require.NoError(t, sonic.ConfigStd.Unmarshal(art.Provenance().Report, &silver))
	require.Len(t, silver.Plugins, 1)
	assert.Contains(t, silver.Plugins[0].Steps, packedStep{
		Step: "test", Result: "ok", Subject: "tests/tiers.yaml: a silver user is a member",
	}, "the test cases ran and are reported")
}

// packedSteps is the part of a packed report the tests read: each plugin's steps.
type packedSteps struct {
	Plugins []packedPlugin `json:"plugins"`
}

// packedPlugin is a plugin of [packedSteps].
type packedPlugin struct {
	Steps []packedStep `json:"steps"`
}

// packedStep is a step of a [packedPlugin].
type packedStep struct {
	Step    string `json:"step"`
	Result  string `json:"result"`
	Subject string `json:"subject"`
}

// silverCases are test cases of the repository's silver plugin, so packing it runs (and reports) a case.
const silverCases = `cases:
  - name: a silver user is a member
    call: IsMember
    input: {user: ana}
    host:
      - {method: Tier, args: {user: ana}, result: silver}
    expect:
      result: true
`

// TestPackRefusesInvalidPluginsAndReplacedContracts packs nothing, exiting 1, when a plugin is not valid (the
// others are, and none is packed), while plg.yaml replaces the contract module by a local checkout (which validate
// accepts, with a warning), and while a Go plugin's go.mod replaces it by a directory (likewise); and refuses an
// output directory inside the files it packs before validating anything.
func TestPackRefusesInvalidPluginsAndReplacedContracts(t *testing.T) {
	t.Parallel()

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	out := filepath.Join(t.TempDir(), "dist")

	invalid := copyRepo(t, "plugins: [rules/silver, unowned]\nshared: [lib]\nrequireOwnership: true\n",
		map[string]string{})
	got := pack(environ, "-o", out, invalid)
	assert.Equal(t, 1, got.code, "%s%s", got.stdout, got.stderr)
	assertEndsWith(t, got.stderr, "\nplg pack: 1 of the 2 plugins "+filepath.Join(invalid, "plg.yaml")+
		" lists is not valid: unowned: nothing packed\n")
	assert.Contains(t, got.stdout, "rules/silver  ok      \nunowned       FAIL    failed at manifest\n")
	assert.Empty(t, artifactsIn(t, out))

	checkout, err := filepath.Abs(filepath.Join("..", "..", "testdata", "validate", "checkout", "rewards"))
	require.NoError(t, err)

	replaced := copyRepo(t, "plugins: [describes]\nreplace: {example.com/rewards: "+checkout+"}\n",
		map[string]string{})
	require.Equal(t, 0, validate(testproxy.Environ("off", t.TempDir()), replaced).code, "validate accepts it")

	got = pack(testproxy.Environ("off", t.TempDir()), "-o", out, replaced)
	assert.Equal(t, 1, got.code, "%s%s", got.stdout, got.stderr)
	assert.Contains(t, got.stdout, "plg.yaml  FAIL    plg pack refuses while a contract module is replaced by a "+
		"local directory (remove the replace to pack against the module plugin.yaml pins): "+
		filepath.Join(replaced, "plg.yaml")+" replaces example.com/rewards => "+checkout+"\n")
	assert.Equal(t, "plg pack: 1 of the 1 plugins "+filepath.Join(replaced, "plg.yaml")+" lists is not valid: "+
		"describes: nothing packed\n", got.stderr, "nothing loaded, so nothing logged")

	require.Equal(t, 0, validate(environ, "goreplace").code, "validate accepts it")

	got = pack(environ, "-o", out, "goreplace")
	assert.Equal(t, 1, got.code, "%s%s", got.stdout, got.stderr)
	assert.Contains(t, got.stdout, "go.mod     FAIL")
	assert.Contains(t, got.stdout, "plg pack refuses while a contract module is replaced by a local directory "+
		"(remove the replace to pack against the module plugin.yaml pins): "+
		filepath.Join(abs(t, "goreplace"), "go.mod")+" replaces example.com/rewards by the directory ")
	assert.Equal(t, "plg pack: "+abs(t, "goreplace")+" is not a valid plugin: nothing packed\n", got.stderr,
		"nothing loaded, so nothing logged")
	assert.Empty(t, artifactsIn(t, out))

	repo := copyRepo(t, repoYAML(t), map[string]string{})
	for _, inside := range []string{filepath.Join(repo, "gold", "dist"), filepath.Join(repo, "lib", "dist")} {
		got = pack(environ, "-o", inside, repo)
		assert.Equal(t, 1, got.code, inside)
		assert.Equal(t, "plg pack: the output directory is inside files plg pack packs (the next pack would pack "+
			"the artifacts too): choose another with -o: "+inside+" is inside "+filepath.Dir(inside)+"\n",
			got.stderr)
		assert.Empty(t, got.stdout, "nothing validated")
	}

	parent := t.TempDir()
	plugin := filepath.Join(parent, "luagood")
	require.NoError(t, os.CopyFS(plugin, os.DirFS(abs(t, "luagood"))))

	got = packIn(environ, parent, "luagood") // -o defaults to the working directory, here the plugin's parent
	assert.Equal(t, 0, got.code, "%s%s", got.stdout, got.stderr)

	written := filepath.Join(parent, "luagood.plg")
	assertEndsWith(t, got.stderr, "\nplg pack: wrote luagood.plg ("+openPacked(t, written).Provenance().Digest+")\n")
	assert.Equal(t, []string{"luagood", "luagood.plg"}, artifactsIn(t, parent), "no temporary file is left")

	got = packIn(environ, plugin, ".") // from inside the plugin, -o defaulting to "."
	assert.Equal(t, 1, got.code)
	assert.Contains(t, got.stderr, "choose another with -o: . is inside "+plugin+"\n", "-o is named as given")

	got = packIn(environ, plugin, "-o", "dist", ".")
	assert.Equal(t, 1, got.code)
	assert.Contains(t, got.stderr, "choose another with -o: dist is inside "+plugin+"\n", "-o is named as given")
}

// gitIn returns a function running git with its arguments in the repository repo, in the environment environ, as
// an author committing with no configuration of their own, and returning its trimmed output.
func gitIn(t *testing.T, environ []string, repo string) func(args ...string) string {
	t.Helper()

	return func(args ...string) string {
		t.Helper()

		//nolint:gosec // G204: git with the test's own arguments
		cmd := exec.CommandContext(t.Context(), "git", append([]string{
			"-C", repo, "-c", "user.name=plg",
			"-c", "user.email=plg@example.com", "-c", "commit.gpgsign=false",
		}, args...)...)
		cmd.Env = environ

		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))

		return strings.TrimSpace(string(out))
	}
}

// TestPackRecordsTheGitCommit packs a plugin of an author repository committed to git: the provenance records the
// commit, clean; changing the repository's shared directory, adding a file to the plugin (even an ignored one) or
// editing plg.yaml makes it dirty, changing another plugin of the repository does not.
func TestPackRecordsTheGitCommit(t *testing.T) {
	t.Parallel()

	environ := append(testproxy.Environ(testproxy.New(t, modules()), t.TempDir()),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	repo := copyRepo(t, repoYAML(t), map[string]string{".gitignore": "*.log\n"})
	git := gitIn(t, environ, repo)

	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "plugins")

	head := git("rev-parse", "HEAD")
	silver := filepath.Join(repo, "rules", "silver")

	packOnce := func(msg string) plg.Provenance {
		t.Helper()

		out := t.TempDir()
		got := pack(environ, "-o", out, silver)
		require.Equal(t, 0, got.code, "%s: %s%s", msg, got.stdout, got.stderr)

		return openPacked(t, filepath.Join(out, "silver.plg")).Provenance()
	}

	clean := packOnce("clean")
	assert.Equal(t, head, clean.Commit)
	assert.False(t, clean.Dirty)

	write := func(name, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(repo, filepath.FromSlash(name)), []byte(content), 0o600))
	}

	write("hungry/plugin.lua", "-- another plugin\n")
	assert.False(t, packOnce("another plugin changed").Dirty)
	git("checkout", "-q", "--", "hungry/plugin.lua")

	lib, err := os.ReadFile(filepath.Join(repo, "lib", "tiers.lua")) //nolint:gosec // G304: the test's copy
	require.NoError(t, err)

	for _, change := range []struct {
		name, content string
		tracked       bool
	}{
		{"lib/tiers.lua", string(lib) + "-- edited\n", true},
		{"rules/silver/notes.txt", "untracked\n", false},
		{"rules/silver/debug.log", "ignored\n", false},
		{"plg.yaml", repoYAML(t) + "# edited\n", true},
	} {
		write(change.name, change.content)

		dirty := packOnce(change.name)
		assert.True(t, dirty.Dirty, change.name)
		assert.Equal(t, head, dirty.Commit, change.name)

		if change.tracked {
			git("checkout", "-q", "--", change.name)
		} else {
			require.NoError(t, os.Remove(filepath.Join(repo, filepath.FromSlash(change.name))))
		}

		require.False(t, packOnce("restored").Dirty, change.name)
	}
}

// TestPackResolvesRelativePaths packs a plugin of an author repository committed to git, named relative to a
// working directory given as ".", as `plg pack` run in a shell is: from the repository, a file added to the
// plugin's directory makes it dirty (git's pathspecs name the plugin's directory, not one inside it); from inside
// the plugin, `plg pack .` validates the plugin under its directory's name and writes <name>.plg, clean, in the
// relative -o, which it names as given.
//
//nolint:paralleltest // t.Chdir: the working directory is the process's
func TestPackResolvesRelativePaths(t *testing.T) {
	environ := append(testproxy.Environ(testproxy.New(t, modules()), t.TempDir()),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	repo := copyRepo(t, repoYAML(t), map[string]string{})
	git := gitIn(t, environ, repo)

	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "plugins")

	head := git("rev-parse", "HEAD")
	notes := filepath.Join(repo, "rules", "silver", "notes.txt")
	require.NoError(t, os.WriteFile(notes, []byte("untracked\n"), 0o600))

	t.Chdir(repo)

	out := t.TempDir()
	got := packIn(environ, ".", "-o", out, filepath.Join("rules", "silver"))
	require.Equal(t, 0, got.code, got.stdout+got.stderr)

	dirty := openPacked(t, filepath.Join(out, "silver.plg")).Provenance()
	assert.True(t, dirty.Dirty, "notes.txt is not committed")
	assert.Equal(t, head, dirty.Commit)

	require.NoError(t, os.Remove(notes))
	t.Chdir(filepath.Join("rules", "silver"))

	dist := filepath.Join("..", "..", "..", "dist") // next to the repository
	got = packIn(environ, ".", "-o", dist, ".")
	require.Equal(t, 0, got.code, got.stdout+got.stderr)
	assert.Contains(t, rows(got.stdout),
		"manifest ok lua plugin silver implementing example.com/rewards/cohort.Provider@v1.2.0")
	assert.Contains(t, rows(got.stdout), "typings ok plg.d.lua, .luarc.json up to date", "lib is shared")

	clean := openPacked(t, filepath.Join(filepath.Dir(repo), "dist", "silver.plg")).Provenance()
	assert.False(t, clean.Dirty)
	assert.Equal(t, head, clean.Commit)
	assertEndsWith(t, got.stderr, "plg pack: wrote "+filepath.Join(dist, "silver.plg")+" ("+clean.Digest+")\n")
}

// TestPackUsageAndOutputFailures checks pack's operands and flags (validate's, but -near-limit, as the resource
// report is left out), fails on an author repository whose plg.yaml cannot be read before validating anything, and
// fails without leaving a partial artifact when the output directory cannot be created or written.
func TestPackUsageAndOutputFailures(t *testing.T) {
	t.Parallel()

	const noOperand = "plg pack: name one plugin directory or author repository\n"

	for _, tc := range []struct {
		want string
		args []string
	}{
		{args: nil, want: noOperand},
		{args: []string{"a", "b"}, want: noOperand},
		{args: []string{"-near-limit", "0.5", "a"}, want: "plg pack: flag provided but not defined: -near-limit\n"},
		{args: []string{"-format", "xml", "a"}, want: "plg pack: -format must be table or json, not \"xml\"\n"},
	} {
		got := pack(nil, tc.args...)
		assert.Equal(t, 2, got.code, tc.args)
		assertStartsWith(t, got.stderr, tc.want)
		assert.Empty(t, got.stdout, tc.args)
	}

	broken := copyRepo(t, "plugins: []\n", map[string]string{})
	got := pack(nil, "-o", t.TempDir(), broken)
	assert.Equal(t, 1, got.code)
	assertStartsWith(t, got.stderr, "plg pack: invalid plg.yaml")
	assert.Empty(t, got.stdout)

	environ := testproxy.Environ(testproxy.New(t, modules()), t.TempDir())
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	got = pack(environ, "-o", filepath.Join(file, "dist"), "luagood")
	assert.Equal(t, 1, got.code, got.stderr)
	assertEndsWith(t, got.stderr, "\nplg pack: creating the output directory: mkdir "+file+": not a directory\n")

	if os.Geteuid() == 0 {
		return // root writes anywhere
	}

	readOnly := t.TempDir()
	require.NoError(t, os.Chmod(readOnly, 0o500)) //nolint:gosec // G302: a directory nobody may write to

	got = pack(environ, "-o", readOnly, "luagood")
	assert.Equal(t, 1, got.code, got.stderr)
	assert.Contains(t, got.stderr, "plg pack: writing "+filepath.Join(readOnly, "luagood.plg")+": open ")
	assert.Empty(t, artifactsIn(t, readOnly))
}

// assertStartsWith checks that got starts with want.
func assertStartsWith(t *testing.T, got, want string) {
	t.Helper()

	assert.True(t, strings.HasPrefix(got, want), "%q does not start with %q", got, want)
}
