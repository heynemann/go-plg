package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/authorrepo"
	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/modfetch"
)

// validateRepo validates every plugin the author repository in run.dir lists, in its plg.yaml's order: a section
// per plugin (its directory, then the steps' table), then a summary table with a row per plugin.
func validateRepo(base *validation) outcome {
	repo, err := authorrepo.Load(base.dir)
	if err != nil {
		return failure("validate", err)
	}

	summary := newTable(base.env.Stdout)
	summary.row("PLUGIN", "RESULT", "DETAIL")

	var invalid []string

	for _, plugin := range repo.Plugins {
		run := *base
		run.dir, run.repo = filepath.Join(repo.Dir, filepath.FromSlash(plugin)), repo
		run.table = newStepTable(base.env.Stdout)

		_, err = fmt.Fprintf(base.env.Stdout, "%s\n", plugin)
		if err != nil {
			return failure("validate", err)
		}

		run.table.row("STEP", "RESULT", "DETAIL")

		valid := run.run(context.Background())

		err = run.table.flush()
		if err == nil {
			_, err = fmt.Fprintln(base.env.Stdout)
		}

		if err != nil {
			return failure("validate", err)
		}

		result := stepOK
		if !valid {
			result = stepFailed

			invalid = append(invalid, plugin)
		}

		summary.row(plugin, result, run.table.summary())
	}

	err = summary.flush()
	if err != nil {
		return failure("validate", err)
	}

	if len(invalid) > 0 {
		return outcome{code: exitFailure, msg: fmt.Sprintf("plg validate: %d of the %d plugins %s lists %s not "+
			"valid: %s\n", len(invalid), len(repo.Plugins), repo.File(), plural(len(invalid), "is", "are"),
			strings.Join(invalid, ", "))}
	}

	return outcome{code: exitOK}
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

	v.table.row(authorrepo.FileName, stepOK, settings(v.repo))

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
		v.table.row(authorrepo.FileName, stepWarning, repo.File()+" does not list this plugin: it is validated "+
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
	v.files = os.DirFS(v.dir)

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

	v.table.row("shared", stepOK, detail)

	v.files = os.DirFS(dir)

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

	v.table.row("contract", stepWarning, fmt.Sprintf("module %s replaced by the directory %s (%s): the pin %s is "+
		"not checked against it", replace.Path, replace.Dir, authorrepo.FileName, ref.Version))

	return &modfetch.Module{Path: replace.Path, Version: contractref.Devel, Dir: replace.Dir}, nil
}

// stepTable is a validation's table of steps, which also counts its warnings and remembers the first step that
// failed.
type stepTable struct {
	*table

	failedStep string
	warnings   int
}

func newStepTable(out io.Writer) *stepTable {
	return &stepTable{table: newTable(out)}
}

// row writes one row of cells: step, result, detail.
func (t *stepTable) row(cells ...string) {
	if len(cells) > 1 {
		switch {
		case cells[1] == stepFailed && t.failedStep == "":
			t.failedStep = cells[0]
		case cells[1] == stepWarning:
			t.warnings++
		default:
		}
	}

	t.table.row(cells...)
}

// summary describes the validation in a word: the step that failed, or the warnings.
func (t *stepTable) summary() string {
	switch {
	case t.failedStep != "":
		return "failed at " + t.failedStep
	case t.warnings > 0:
		return strconv.Itoa(t.warnings) + " " + plural(t.warnings, "warning", "warnings")
	default:
		return ""
	}
}
