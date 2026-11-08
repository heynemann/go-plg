package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/authorrepo"
	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/modfetch"
	"github.com/heynemann/go-plg/internal/typings"
)

// validateTarget validates what base.dir holds, a plugin directory or an author repository, and shows the report
// as plg validate does; it fails when the repository's plg.yaml cannot be loaded or the report shown.
func validateTarget(base *validation, out *output) (*report, error) {
	if !authorrepo.Exists(base.dir) {
		base.report = newPluginReport(base.arg, base.dir)
		rep := &report{Version: reportVersion, Plugins: []*pluginReport{base.report}}
		rep.Valid = base.run(context.Background())

		err := out.plugin(base.env.Stdout, rep, base.report)
		if err == nil {
			err = out.show(base.env.Stdout, rep)
		}

		return rep, err
	}

	return validateRepo(base, out)
}

// validateRepo validates every plugin the author repository in run.dir lists, in its plg.yaml's order, and shows
// the report: as a table, a section per plugin (its name, then its steps) as each is validated, then a summary
// table with a row per plugin. An invalid plg.yaml is reported as the repository's one failed step.
func validateRepo(base *validation, out *output) (*report, error) {
	repo, err := authorrepo.Load(base.dir)
	if err != nil {
		return invalidRepo(base, out, err)
	}

	rep := &report{Version: reportVersion, Repository: repo.File(), Valid: true}

	for _, plugin := range repo.Plugins {
		run := *base
		run.dir, run.repo = filepath.Join(repo.Dir, filepath.FromSlash(plugin)), repo
		run.arg = filepath.Join(base.arg, filepath.FromSlash(plugin))
		run.report = newPluginReport(plugin, run.dir)
		rep.Plugins = append(rep.Plugins, run.report)

		if !run.run(context.Background()) {
			rep.Valid = false
		}

		err = out.plugin(base.env.Stdout, rep, run.report)
		if err != nil {
			return nil, err
		}
	}

	return rep, out.show(base.env.Stdout, rep)
}

// invalid says which plugins of the report are not valid: the plugin validated on its own, or those of the
// repository.
func (r *report) invalid() string {
	if r.Repository == "" {
		return r.Plugins[0].Dir + " is not a valid plugin"
	}

	var invalid []string

	for _, plugin := range r.Plugins {
		if !plugin.Valid {
			invalid = append(invalid, plugin.Plugin)
		}
	}

	return fmt.Sprintf("%d of the %d plugins %s lists %s not valid: %s", len(invalid), len(r.Plugins),
		r.Repository, plural(len(invalid), "is", "are"), strings.Join(invalid, ", "))
}

// invalidRepo shows the report of a repository whose plg.yaml is invalid: no plugin was validated, and the
// repository, named as on the command line, failed its plg.yaml step. The table, JSON report and JUnit file show it;
// the error returned is the plg.yaml's (or the output's, when showing it fails).
func invalidRepo(base *validation, out *output, invalid error) (*report, error) {
	failed := newPluginReport(base.arg, base.dir)
	failed.row(authorrepo.FileName, resultFailed, invalid.Error())
	failed.finish()

	rep := &report{Version: reportVersion, Repository: filepath.Join(base.dir, authorrepo.FileName)}
	rep.Plugins = []*pluginReport{failed}

	err := out.plugin(base.env.Stdout, rep, failed)
	if err == nil {
		err = out.show(base.env.Stdout, rep)
	}

	if err != nil {
		return nil, err
	}

	return nil, invalid
}

// plural returns one when n is 1, else many.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}

	return many
}

// repository shows the settings of the author repository listing the plugin, finding it unless plg validate was
// given the repository.
func (v *validation) repository() bool {
	if v.repo == nil {
		return v.findRepository()
	}

	if v.packed != nil && len(v.repo.Replace) > 0 {
		replaces := make([]string, len(v.repo.Replace))
		for i, replace := range v.repo.Replace {
			replaces[i] = replace.Path + " => " + replace.Dir
		}

		return v.failed(authorrepo.FileName, fmt.Errorf("%w: %s replaces %s", errReplaced, v.repo.File(),
			strings.Join(replaces, ", ")))
	}

	v.report.row(authorrepo.FileName, resultOK, settings(v.repo))

	return true
}

// findRepository finds the author repository the plugin is in, if any. A plugin in a repository that does not list
// it is validated on its own, with a warning.
func (v *validation) findRepository() bool {
	repo, err := authorrepo.Find(v.dir)

	switch {
	case err != nil:
		return v.failed(authorrepo.FileName, err)
	case repo == nil:
		return true
	default:
	}

	if _, listed := repo.Lists(v.dir); !listed {
		v.report.row(authorrepo.FileName, resultWarning, repo.File()+" does not list this plugin: it is validated "+
			"without the repository's settings")

		return true
	}

	v.repo = repo

	return v.repository()
}

// settings describes what a repository's plg.yaml sets for its plugins.
func settings(repo *authorrepo.Repo) string {
	described := []string{repo.File()}

	if len(repo.Shared) > 0 {
		described = append(described, "shared "+strings.Join(repo.Shared, ", "))
	}

	for _, replace := range repo.Replace {
		described = append(described, fmt.Sprintf("replace %s => %s", replace.Path, replace.Dir))
	}

	if repo.RequireOwnership {
		described = append(described, "ownership required")
	}

	return strings.Join(described, "; ")
}

// shared sets the plugin's files: its directory, or, when its author repository has shared directories, a
// temporary copy of it with them copied in, which cleanup removes.
func (v *validation) shared(desc *plg.Descriptor) (func(), bool) {
	v.files, v.filesDir = os.DirFS(v.dir), v.dir

	if v.repo == nil || len(v.repo.Shared) == 0 {
		return func() {}, true
	}

	plugin, _ := v.repo.Lists(v.dir)

	goPackage := ""
	if desc.Language == plg.LanguageGo {
		goPackage = goPackageDir(desc.Entrypoint)
	}

	dir, err := os.MkdirTemp("", "plg-validate-*")
	if err != nil {
		return nil, v.failed("shared", err)
	}

	cleanup := func() { os.RemoveAll(dir) } //nolint:errcheck,gosec // a temporary copy

	placements, err := v.repo.Compose(dir, plugin, goPackage)
	if err != nil {
		cleanup()

		return nil, v.failed("shared", err)
	}

	copied := make([]string, len(placements))
	for i, placement := range placements {
		copied[i] = placement.Shared + " at " + placement.At
	}

	detail := "none holds Go files"
	if len(copied) > 0 {
		detail = "copied " + strings.Join(copied, ", ")
	}

	v.report.row("shared", resultOK, detail)

	v.files, v.filesDir = os.DirFS(dir), dir

	return cleanup, true
}

// replaced returns the local checkout of the contract module when the author repository replaces it, nil when it
// does not; it fails when the checkout does not provide the package.
func (v *validation) replaced(ref contractref.Ref) (*modfetch.Module, error) {
	if v.repo == nil {
		return nil, nil //nolint:nilnil // not replaced
	}

	replace, ok := v.repo.Replacement(ref.Package)
	if !ok {
		return nil, nil //nolint:nilnil // not replaced
	}

	pkgDir := filepath.Join(replace.Dir, filepath.FromSlash(strings.TrimPrefix(
		strings.TrimPrefix(ref.Package, replace.Path), "/")))

	info, err := os.Stat(pkgDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w %s: %s replaces %s by %s, which has no directory %s", modfetch.ErrNotFound,
			ref.Package, authorrepo.FileName, replace.Path, replace.Dir, pkgDir)
	}

	v.report.row("contract", resultWarning, fmt.Sprintf("module %s replaced by the directory %s (%s): the pin %s is "+
		"not checked against it", replace.Path, replace.Dir, authorrepo.FileName, ref.Version))

	return &modfetch.Module{Path: replace.Path, Version: contractref.Devel, Dir: replace.Dir}, nil
}

// sharedDirs is where the plugin finds its repository's shared directories, for the editor configuration its
// typings include; nil outside a repository or in one without shared directories.
func (v *validation) sharedDirs() *typings.Shared {
	if v.repo == nil || len(v.repo.Shared) == 0 {
		return nil
	}

	root, err := filepath.Rel(v.dir, v.repo.Dir)
	if err != nil {
		return nil
	}

	return &typings.Shared{Root: filepath.ToSlash(root), Dirs: v.repo.Shared}
}
