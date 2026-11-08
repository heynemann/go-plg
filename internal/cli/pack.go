package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bytedance/sonic"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/artifact"
	"github.com/heynemann/go-plg/internal/authorrepo"
	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/modfetch"
)

// errReplaced reports a contract module replaced by a local directory, which plg pack refuses: the artifact would
// hold a plugin validated against a checkout no service is built with.
var errReplaced = errors.New("plg pack refuses while a contract module is replaced by a local directory " +
	"(remove the replace to pack against the module plugin.yaml pins)")

// errOutputInside reports an output directory inside files plg pack packs.
var errOutputInside = errors.New("the output directory is inside files plg pack packs (the next pack would " +
	"pack the artifacts too): choose another with -o")

// errNotCopied reports a storage's Open that did not return its copy of the plugin's files.
var errNotCopied = errors.New("plg's storage did not return its copy of the plugin's files")

// errManifestChanged reports a plugin.yaml changed between its reading and the snapshot plg pack packs.
var errManifestChanged = errors.New("plugin.yaml changed while plg pack read the plugin's files: pack again")

// Modes of what plg pack writes.
const (
	artifactMode = 0o644
	outDirMode   = 0o755
)

// packed is a plugin plg pack validated: its report, its files as a storage loads them (the author repository's
// shared directories copied in), the contract module it was validated against, the git state of what its files
// were snapshotted from, and the directory its report's paths are relative to.
type packed struct {
	report   *pluginReport
	files    *memfs.FS
	contract modfetch.Module
	key      string
	root     string
	git      gitCommit
}

// runPack runs `plg pack`.
func runPack(env *Env, args []string) outcome {
	flags := flag.NewFlagSet("pack", flag.ContinueOnError)
	outDir := flags.String("o", ".", "the directory the artifacts are written to")
	loading, report := newValidateFlags(flags), newReportFlags(flags)

	operands, res, ok := parseMixed("pack", flags, args)
	if !ok {
		return res
	}

	if msg := validateUsage(operands, *report.format, *report.colour, defaultNearLimit); msg != "" {
		return usageError("pack", msg)
	}

	run, out := loading.validation(env, operands[0], defaultNearLimit), resolve(env.Dir, *outDir)

	err := checkOutput(run.dir, out)
	if err != nil {
		return failure("pack", err)
	}

	var plugins []*packed

	run.packed = func(p *packed) { plugins = append(plugins, p) }

	rep, err := validateTarget(&run, report.output(env))
	if err != nil {
		return failure("pack", err)
	}

	if !rep.Valid {
		return outcome{code: exitFailure, msg: "plg pack: " + rep.invalid() + ": nothing packed\n"}
	}

	written, err := writeArtifacts(rep, plugins, out, *outDir)
	if err != nil {
		return failure("pack", err)
	}

	return outcome{code: exitOK, msg: written}
}

// checkOutput fails when out, the output directory, is inside a directory whose files plg pack packs from dir (see
// packedDirs). Both are clean absolute paths (see resolve), as are the directories packedDirs returns.
func checkOutput(dir, out string) error {
	inputs, err := packedDirs(dir)
	if err != nil {
		return err
	}

	for _, input := range inputs {
		if out == input || strings.HasPrefix(out, input+string(filepath.Separator)) {
			return fmt.Errorf("%w: %s is inside %s", errOutputInside, out, input)
		}
	}

	return nil
}

// packedDirs returns the directories plg pack packs files from, given dir: the plugin directory, or an author
// repository's plugin directories, and the repository's shared directories.
func packedDirs(dir string) ([]string, error) {
	repo, err := authorrepo.Find(dir)
	inputs := []string{dir}

	if authorrepo.Exists(dir) {
		repo, err = authorrepo.Load(dir)
		inputs = nil
	}

	if err != nil || repo == nil {
		return inputs, err //nolint:wrapcheck // its errors name plg.yaml; the command prefixes its name
	}

	listed := slices.Clone(repo.Shared)
	if inputs == nil {
		listed = append(listed, repo.Plugins...)
	}

	for _, path := range listed {
		inputs = append(inputs, filepath.Join(repo.Dir, filepath.FromSlash(path)))
	}

	return inputs, nil
}

// snapshot, for plg pack, copies the plugin's files once as a storage loads them (plg.NewFSStorage's copy:
// regular files, links inside the plugin left out, within the limits of a plugin's files) and reads the git state
// of what they were copied from at that moment; validation then reads that copy, which is what is packed, so a
// file changed on disk meanwhile is neither validated nor packed. A failure fails the plugin's pack step.
func (v *validation) snapshot(ctx context.Context) bool {
	if v.packed == nil {
		return true
	}

	opened, err := plg.NewFSStorage(filepath.Dir(v.filesDir)).Open(ctx,
		&plg.Descriptor{Key: filepath.Base(v.filesDir)})
	if err != nil {
		return v.failed("pack", err)
	}

	files, ok := opened.(*memfs.FS)
	if !ok { // unreachable: FSStorage's Open returns its copy
		return v.failed("pack", fmt.Errorf("%w: %T", errNotCopied, opened))
	}

	inputs := []string{v.dir}
	if v.repo != nil {
		for _, shared := range v.repo.Shared {
			inputs = append(inputs, filepath.Join(v.repo.Dir, filepath.FromSlash(shared)))
		}

		inputs = append(inputs, v.repo.File())
	}

	state, err := gitState(ctx, v.env, inputs)
	if err != nil {
		return v.failed("pack", err)
	}

	v.files, v.snap, v.git = files, files, state

	return true
}

// checkSnapshot fails when the snapshot's plugin.yaml is not manifest, the plugin.yaml validation read before it.
func (v *validation) checkSnapshot(manifest []byte) bool {
	if v.snap == nil {
		return true
	}

	data, err := v.snap.ReadFile("plugin.yaml")
	if err != nil || !bytes.Equal(data, manifest) {
		return v.failed("pack", errManifestChanged)
	}

	return true
}

// keep passes the plugin plg pack validated on, with the files validation read.
func (v *validation) keep(res *resolved) {
	v.packed(&packed{report: v.report, files: v.snap, contract: res.mod, key: v.key(), root: v.root(), git: v.git})
}

// writeArtifacts writes the artifact of each plugin, <key>.plg in the directory out, and returns what it wrote,
// naming the directory shown, as -o gave it.
func writeArtifacts(rep *report, plugins []*packed, out, shown string) (string, error) {
	err := os.MkdirAll(out, outDirMode)
	if err != nil {
		return "", fmt.Errorf("creating the output directory: %w", err)
	}

	var written strings.Builder

	for _, plugin := range plugins {
		record, err := provenance(rep, plugin)
		if err != nil {
			return "", err
		}

		name := filepath.Join(out, plugin.key+".plg")

		err = writeArtifact(name, plugin.files, record)
		if err != nil {
			return "", err
		}

		fmt.Fprintf(&written, "plg pack: wrote %s (%s)\n", filepath.Join(shown, plugin.key+".plg"), record.Digest)
	}

	return written.String(), nil
}

// provenance returns the provenance record of the plugin, validated as rep reports; its report's paths are
// relative to the plugin's root.
func provenance(rep *report, plugin *packed) (*adminjson.Provenance, error) {
	own := report{Version: reportVersion, Valid: plugin.report.Valid}
	if rep.Repository != "" {
		own.Repository = relativeTo(plugin.root, rep.Repository)
	}

	shown := *plugin.report
	shown.Dir = relativeTo(plugin.root, shown.Dir)
	own.Plugins = []*pluginReport{&shown}

	data, err := sonic.ConfigStd.Marshal(&own)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: the report: %w", plugin.key, err)
	}

	return &adminjson.Provenance{
		Report:   data,
		Contract: adminjson.Module{Path: plugin.contract.Path, Version: plugin.contract.Version},
		Digest:   artifact.Digest(plugin.files),
		Commit:   plugin.git.commit,
		Plg:      contractref.Built(plgModule).Version,
		Dirty:    plugin.git.dirty,
	}, nil
}

// gitCommit is the git commit packed files are at, and whether they differ from it.
type gitCommit struct {
	commit string
	dirty  bool
}

// gitState returns the git commit the first of inputs (a directory) is at, and whether any of inputs differs from
// it: changed, added, untracked or ignored files. Without git, or outside a repository with a commit, there is no
// commit.
func gitState(ctx context.Context, env *Env, inputs []string) (gitCommit, error) {
	head, err := git(ctx, env, inputs[0], "rev-parse", "--verify", "HEAD")
	if err != nil {
		return gitCommit{}, nil //nolint:nilerr // no repository, no commit or no git: nothing to record
	}

	status, err := git(ctx, env, inputs[0], append([]string{
		"status", "--porcelain", "-z", "--untracked-files=all",
		"--ignored=matching", "--",
	}, inputs...)...)
	if err != nil {
		return gitCommit{}, fmt.Errorf("git status: %w", err)
	}

	return gitCommit{commit: strings.TrimSpace(string(head)), dirty: len(status) > 0}, nil
}

// git runs git with args in dir, in env's environment, and returns its output.
func git(ctx context.Context, env *Env, dir string, args ...string) ([]byte, error) {
	//nolint:gosec // G204: git, with plg's own arguments and the packed paths
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env.environ()

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return out, nil
}

// writeArtifact writes the artifact of files with record to the file name, through a temporary file renamed into
// place, so a failure leaves no partial artifact.
func writeArtifact(name string, files *memfs.FS, record *adminjson.Provenance) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+"-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}

	err = artifact.Write(tmp, files, record)
	if err == nil {
		err = tmp.Chmod(artifactMode)
	}

	err = errors.Join(err, tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), name)
	}

	if err != nil {
		os.Remove(tmp.Name()) //nolint:errcheck,gosec // the temporary file, if left

		return fmt.Errorf("writing %s: %w", name, err)
	}

	return nil
}
