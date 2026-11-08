package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/authorrepo"
	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/typings"
)

// fileMode is the mode of the files plg init writes; dirMode its directories'.
const (
	fileMode = 0o644
	dirMode  = 0o755
)

var (
	errNotEmpty   = errors.New("exists and is not empty: plg init writes a new plugin directory")
	errGoModOther = errors.New("requires another version of the contract module")
)

// languages are the languages -lang names, as plugin.yaml names them.
//
//nolint:gochecknoglobals // immutable lookup table
var languages = map[string]plg.Language{
	"go": plg.LanguageGo, "lua": plg.LanguageLua, "ts": plg.LanguageTypeScript, "typescript": plg.LanguageTypeScript,
}

// runInit runs `plg init`.
func runInit(env *Env, args []string) outcome {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	implements := flags.String("implements", "", "the interface the plugin implements: <package>.<Interface>@<version>")
	lang := flags.String("lang", "", "the plugin's language: go, lua or ts")
	host := flags.String("host", "", "the contract's Host interface the plugin's Create takes")

	operands, res, ok := parseMixed("init", flags, args)
	if !ok {
		return res
	}

	language, known := languages[*lang]

	switch {
	case *implements == "":
		return usageError("init", "-implements names the interface the plugin implements")
	case !known:
		return usageError("init", "-lang is go, lua or ts")
	case len(operands) > 1:
		return usageError("init", "name at most one plugin directory")
	default:
	}

	ref, err := contractref.Parse(*implements)
	if err != nil {
		return usageError("init", err.Error())
	}

	arg := strings.ToLower(ref.Interface)
	if len(operands) == 1 {
		arg = operands[0]
	}

	run := &validation{
		env: env, dir: resolve(env.Dir, arg), arg: arg, host: *host, table: newStepTable(env.Stdout), newPlugin: true,
	}

	run.table.row("STEP", "RESULT", "DETAIL")

	created := run.scaffold(context.Background(), ref, language)

	err = run.table.flush()
	if err != nil {
		return failure("init", err)
	}

	if !created {
		return outcome{code: exitFailure, msg: fmt.Sprintf("plg init: no plugin created in %s\n", run.dir)}
	}

	return outcome{code: exitOK}
}

// scaffold creates the plugin directory: it resolves the contract as validate does, writes the plugin's files
// and adds it to its Go module and its author repository. When a step fails it leaves nothing behind.
func (v *validation) scaffold(ctx context.Context, ref contractref.Ref, language plg.Language) bool {
	if !v.emptyDir() || !v.initRepository() {
		return false
	}

	desc := plg.Descriptor{Language: language, Implements: ref.String()}

	mod, ok := v.resolve(ctx, ref)
	if !ok {
		return false
	}

	loaded, ok := v.contract(ref, mod, &desc)
	if !ok {
		return false
	}

	if len(v.hosts) > 0 {
		v.table.row("", stepHint, fmt.Sprintf("for a Create taking a Host, pass -host <%s>",
			strings.Join(v.hosts, "|")))
	}

	res := &resolved{loaded: loaded, mod: mod, ref: ref, desc: desc}

	files, ok := v.pluginFiles(res)
	if !ok {
		return false
	}

	goMod := v.goModFile(res)
	if goMod == nil {
		return false
	}

	return v.create(files, goMod)
}

// emptyDir checks that the plugin directory does not exist or is empty.
func (v *validation) emptyDir() bool {
	entries, err := os.ReadDir(v.dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err != nil:
		return v.failed("directory", err)
	case len(entries) > 0:
		return v.failed("directory", fmt.Errorf("%s %w", v.dir, errNotEmpty))
	default:
		return true
	}
}

// initRepository finds the author repository the plugin goes in, if any: its settings apply to the contract's
// resolution, and init adds the plugin to its plg.yaml.
func (v *validation) initRepository() bool {
	repo, err := authorrepo.Find(v.dir)
	if err != nil {
		return v.failed(authorrepo.FileName, err)
	}

	if repo == nil {
		return true
	}

	v.repo = repo

	return v.repository()
}

// pluginFiles returns the plugin's files: plugin.yaml, the typings of a Lua or TypeScript plugin, the skeleton and
// the example tests.
func (v *validation) pluginFiles(res *resolved) ([]typings.File, bool) {
	contract := typingsContract(res)

	scaffold, err := typings.NewScaffold(string(res.desc.Language), contract, v.key())
	if err != nil {
		return nil, v.failed("skeleton", describable(res, err))
	}

	files := []typings.File{{Name: "plugin.yaml", Data: manifestFor(&res.desc, scaffold.Entrypoint)}}

	if res.desc.Language != plg.LanguageGo {
		typ, err := typings.Generate(string(res.desc.Language), contract)
		if err != nil {
			return nil, v.failed("typings", describable(res, err))
		}

		files = append(files, typ.Files...)
	}

	return append(files, scaffold.Files...), true
}

// manifestFor writes the new plugin's plugin.yaml, with an owner block to fill in.
func manifestFor(desc *plg.Descriptor, entrypoint string) []byte {
	return fmt.Appendf(nil, `implements: %s
language: %s
entrypoint: %s
# Who owns the plugin: plg shows it with the plugin's errors, logs and metrics. Fill it in (every field is
# optional; org, company, maintainers, links and labels are the others).
owner:
  team: {name: ""}
  contact: {channel: "", email: ""}
`, desc.Implements, desc.Language, entrypoint)
}

// goModule is the go.mod a Go plugin's module requires the contract in: path, with its new content (nil when
// it already requires the contract), and whether init creates it.
type goModule struct {
	path    string
	data    []byte
	created bool
}

// goModFile returns the go.mod a Go plugin builds with: the author repository's (the nearest go.mod above the
// plugin inside it), which gains the contract module's requirement, else a new one in the plugin directory. It
// returns an empty goModule for a Lua or TypeScript plugin, and nil when the repository's go.mod requires another
// version of the contract module.
func (v *validation) goModFile(res *resolved) *goModule {
	if res.desc.Language != plg.LanguageGo {
		return &goModule{}
	}

	path := v.repoGoMod()
	if path == "" {
		return &goModule{path: filepath.Join(v.dir, "go.mod"), data: newGoMod(v.key(), res), created: true}
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: the author repository's go.mod

	var file *modfile.File

	if err == nil {
		file, err = modfile.Parse(path, data, nil)
	}

	if err == nil {
		var required bool

		required, err = requires(file, res)
		if required {
			return &goModule{path: path}
		}
	}

	if err == nil {
		err = file.AddRequire(res.mod.Path, res.ref.Version)
	}

	if err == nil {
		data, err = file.Format()
	}

	if err != nil {
		v.failed("go.mod", err)

		return nil
	}

	return &goModule{path: path, data: data}
}

// requires reports whether the go.mod file requires the contract module at the plugin's pin; requiring another
// version is an error naming both fixes.
func requires(file *modfile.File, res *resolved) (bool, error) {
	for _, req := range file.Require {
		switch {
		case req.Mod.Path != res.mod.Path:
		case req.Mod.Version != res.ref.Version:
			return false, fmt.Errorf("%s %w: %s %s, not the pin %s (init with @%s, or go get %s@%s)",
				file.Syntax.Name, errGoModOther, req.Mod.Path, req.Mod.Version, res.ref.Version, req.Mod.Version,
				req.Mod.Path, res.ref.Version)
		default:
			return true, nil
		}
	}

	return false, nil
}

// repoGoMod returns the nearest go.mod above the plugin directory inside its author repository; empty without one.
func (v *validation) repoGoMod() string {
	if v.repo == nil {
		return ""
	}

	for dir := filepath.Dir(v.dir); ; dir = filepath.Dir(dir) {
		path := filepath.Join(dir, "go.mod")
		if exists(path) {
			return path
		}

		if dir == v.repo.Dir || dir == filepath.Dir(dir) {
			return ""
		}
	}
}

// goVersion matches the language version of a Go release, as runtime.Version writes it.
var goVersion = regexp.MustCompile(`go(1\.\d+)`)

// newGoMod writes the go.mod of a Go plugin's own module, named after the plugin, requiring the contract module
// at the pinned version, for the Go release plg was built with.
func newGoMod(key string, res *resolved) []byte {
	path := key
	if module.CheckImportPath(path) != nil {
		path = "plugin"
	}

	file := fmt.Sprintf("module %s\n\n", path)
	if match := goVersion.FindStringSubmatch(runtime.Version()); match != nil {
		file += fmt.Sprintf("go %s\n\n", match[1])
	}

	return fmt.Appendf([]byte(file), "require %s %s\n", res.mod.Path, res.ref.Version)
}

// create writes the plugin directory (with its go.mod when it is a module of its own), adds the plugin to its
// author repository, and the contract module's requirement to the repository's go.mod. On failure it undoes what
// it did.
func (v *validation) create(files []typings.File, goMod *goModule) bool {
	undo, ok := v.writePlugin(files, goMod)
	if !ok {
		return false
	}

	undo, ok = v.list(undo)
	if !ok {
		return false
	}

	if goMod.data != nil && !goMod.created {
		err := os.WriteFile(goMod.path, goMod.data, fileMode)
		if err != nil {
			undo()

			return v.failed("go.mod", err)
		}

		v.table.row("go.mod", stepOK, goMod.path+" requires the contract module")
	}

	v.table.row("owner", stepWarning, "fill in plugin.yaml's owner block")

	next := "plg validate " + v.arg + ", or plg dev " + v.arg + " to validate on each save"
	if goMod.data != nil {
		next = "go mod tidy (in " + filepath.Dir(goMod.path) + "), then " + next
	}

	v.table.row("next", stepHint, next)

	return true
}

// writePlugin writes the plugin directory's files, and its go.mod when it is a module of its own. It returns what
// removes them.
func (v *validation) writePlugin(files []typings.File, goMod *goModule) (func(), bool) {
	if goMod.created {
		files = append(files, typings.File{Name: "go.mod", Data: goMod.data})
	}

	existed := exists(v.dir) // and was empty
	undo := func() {
		os.RemoveAll(v.dir) //nolint:errcheck,gosec // undoing init: best effort

		if existed {
			os.Mkdir(v.dir, dirMode) //nolint:errcheck,gosec // as above
		}
	}

	err := writeFiles(v.dir, files)
	if err != nil {
		undo()

		return nil, v.failed("files", err)
	}

	names := make([]string, len(files))
	for i, file := range files {
		names[i] = file.Name
	}

	v.table.row("files", stepOK, "wrote "+strings.Join(names, ", ")+" in "+v.dir)

	return undo, true
}

// list adds the plugin to its author repository's plg.yaml, if any; on failure it runs undo. It returns what undoes
// both.
func (v *validation) list(undo func()) (func(), bool) {
	if v.repo == nil {
		return undo, true
	}

	listed, err := os.ReadFile(v.repo.File())
	if err == nil {
		err = v.repo.AddPlugin(v.dir)
	}

	if err != nil {
		undo()

		return nil, v.failed(authorrepo.FileName, err)
	}

	rel, _ := v.repo.Lists(v.dir)
	v.table.row(authorrepo.FileName, stepOK, "lists "+rel)

	return func() {
		undo()
		os.WriteFile(v.repo.File(), listed, fileMode) //nolint:errcheck,gosec // undoing init: best effort
	}, true
}

// exists reports whether path exists.
func exists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

// writeFiles writes files into dir, creating the directories they need.
func writeFiles(dir string, files []typings.File) error {
	for _, file := range files {
		name := filepath.Join(dir, filepath.FromSlash(file.Name))

		err := os.MkdirAll(filepath.Dir(name), dirMode)
		if err == nil {
			err = os.WriteFile(name, file.Data, fileMode)
		}

		if err != nil {
			return err //nolint:wrapcheck // names the file
		}
	}

	return nil
}
