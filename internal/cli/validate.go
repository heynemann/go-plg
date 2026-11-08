package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/contractload"
	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/modfetch"
	"github.com/heynemann/go-plg/internal/offline"
)

// Step results.
const (
	stepOK      = "ok"
	stepFailed  = "FAIL"
	stepWarning = "warning"
)

// runValidate runs `plg validate`.
func runValidate(env *Env, args []string) outcome {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	host := flags.String("host", "", "the contract's Host interface the plugin's Create takes")
	cacheDir := flags.String("cache-dir", "", "compilation cache directory")

	operands, res, ok := parseMixed("validate", flags, args)
	if !ok {
		return res
	}

	if len(operands) != 1 {
		return usageError("validate", "name one plugin directory")
	}

	dir := resolve(env.Dir, operands[0])
	run := &validation{env: env, dir: dir, host: *host, table: newTable(env.Stdout)}

	if *cacheDir != "" {
		run.cacheDir = resolve(env.Dir, *cacheDir)
	}

	run.table.row("STEP", "RESULT", "DETAIL")

	valid := run.run(context.Background())

	err := run.table.flush()
	if err != nil {
		return failure("validate", err)
	}

	if !valid {
		return outcome{code: exitFailure, msg: fmt.Sprintf("plg validate: %s is not a valid plugin\n", dir)}
	}

	return outcome{code: exitOK}
}

// validation is one run of plg validate on the plugin directory dir; each step adds its rows to table.
type validation struct {
	env      *Env
	table    *table
	dir      string
	host     string
	cacheDir string
}

// run runs the steps in order, stopping at the first that fails; it reports whether every step passed.
func (v *validation) run(ctx context.Context) bool {
	manifest, desc, ok := v.manifest()
	if !ok {
		return false
	}

	ref, err := contractref.Parse(desc.Implements) // ParseManifest checked it
	if err != nil {
		return v.failed("manifest", err)
	}

	mod, ok := v.resolve(ctx, ref)
	if !ok {
		return false
	}

	loaded, ok := v.contract(ref, mod, &desc)
	if !ok {
		return false
	}

	if desc.Language == plg.LanguageGo && (!v.goMod(ref, mod) || !v.goTypes(loaded, &desc)) {
		return false
	}

	return v.load(ctx, loaded.Contract, manifest, &desc)
}

// failed adds the failed step's row.
func (v *validation) failed(step string, err error) bool {
	v.table.row(step, stepFailed, err.Error())

	return false
}

// manifest reads and parses the plugin's plugin.yaml.
func (v *validation) manifest() ([]byte, plg.Descriptor, bool) {
	data, err := os.ReadFile(filepath.Join(v.dir, "plugin.yaml"))
	if err != nil {
		return nil, plg.Descriptor{}, v.failed("manifest", err)
	}

	desc, err := plg.ParseManifest(data)
	if err != nil {
		return nil, plg.Descriptor{}, v.failed("manifest", err)
	}

	v.table.row("manifest", stepOK, fmt.Sprintf("%s plugin %s implementing %s", desc.Language, v.key(),
		desc.Implements))

	return data, desc, true
}

// key is the plugin's key: its directory's name.
func (v *validation) key() string {
	return filepath.Base(v.dir)
}

// resolve fetches the contract module the plugin pins through the go command, or finds it in the module cache.
func (v *validation) resolve(ctx context.Context, ref contractref.Ref) (modfetch.Module, bool) {
	fetcher := modfetch.Fetcher{Environ: v.env.environ()}

	mod, err := fetcher.Fetch(ctx, ref.Package, ref.Version)
	if err != nil {
		return modfetch.Module{}, v.failed("contract", err)
	}

	v.table.row("contract", stepOK, fmt.Sprintf("module %s %s (%s)", mod.Path, mod.Version, mod.Dir))

	return mod, true
}

// contract type-checks the contract package and describes the interface and the Host the plugin's Create takes.
func (v *validation) contract(ref contractref.Ref, mod modfetch.Module, desc *plg.Descriptor) (
	*contractload.Loaded, bool,
) {
	host := v.host
	if host == "" && desc.Language == plg.LanguageGo {
		host = createHost(os.DirFS(v.dir), desc.Entrypoint, ref.Package)
	}

	rel := strings.TrimPrefix(strings.TrimPrefix(ref.Package, mod.Path), "/")

	loaded, err := contractload.Load(filepath.Join(mod.Dir, filepath.FromSlash(rel)), ref.Package, ref.Interface,
		host)
	if err != nil {
		return nil, v.failed("interface", err)
	}

	contract := loaded.Contract
	contract.Module = contractref.Module{Path: mod.Path, Version: mod.Version}

	detail := fmt.Sprintf("%s: %s", contract.Interface.Type, methodNames(contract.Interface.Methods))
	if contract.Host != nil {
		detail += fmt.Sprintf("; Host %s: %s (a stub: every call fails)", contract.Host.Interface.Type,
			methodNames(contract.Host.Interface.Methods))
	}

	v.table.row("interface", stepOK, detail)

	return loaded, true
}

// goTypes type-checks a Go plugin's package against the contract.
func (v *validation) goTypes(loaded *contractload.Loaded, desc *plg.Descriptor) bool {
	err := loaded.CheckGo(os.DirFS(v.dir), goPackageDir(desc.Entrypoint))

	switch {
	case errors.Is(err, contractload.ErrGoUnchecked):
		v.table.row("types", stepWarning, err.Error())
	case err != nil:
		return v.failed("types", err)
	default:
		v.table.row("types", stepOK, "the plugin's package type-checks against "+
			loaded.Contract.Interface.Type.String())
	}

	return true
}

// goPackageDir is a Go plugin's package directory: its entrypoint, or the directory of an entrypoint .go file.
func goPackageDir(entrypoint string) string {
	if path.Ext(entrypoint) == ".go" {
		return path.Dir(entrypoint)
	}

	return entrypoint
}

// goMod checks that a Go plugin's module requires the contract module at the version plugin.yaml pins.
func (v *validation) goMod(ref contractref.Ref, mod modfetch.Module) bool {
	req, err := modfetch.Required(v.dir, mod.Path)
	if err != nil {
		return v.failed("go.mod", err)
	}

	switch {
	case req.Dir != "":
		v.table.row("go.mod", stepWarning, fmt.Sprintf("%s replaces %s by the directory %s: the pin %s is not "+
			"checked against it", req.GoMod, mod.Path, req.Dir, ref.Version))
	case req.Version != ref.Version:
		return v.failed("go.mod", fmt.Errorf("%w: %s requires %s %s, plugin.yaml pins %s", modfetch.ErrGoMod,
			req.GoMod, mod.Path, req.Version, ref.Version))
	default:
		v.table.row("go.mod", stepOK, fmt.Sprintf("%s requires %s %s", req.GoMod, mod.Path, req.Version))
	}

	return true
}

// load loads the plugin with plg's own loading code and runs its Create in the real guest.
func (v *validation) load(ctx context.Context, contract *offline.Contract, manifest []byte, desc *plg.Descriptor,
) bool {
	logger := slog.New(slog.NewTextHandler(v.env.Stderr, nil))
	plugin := &offline.Plugin{Files: os.DirFS(v.dir), Key: v.key(), Manifest: manifest}

	report, err := offline.Check(ctx, contract, plugin, &offline.Options{Logger: logger, CacheDir: v.cacheDir})
	if err != nil {
		return v.failed("load", err)
	}

	ok := report.Error == nil
	if ok {
		v.table.row("load", stepOK, fmt.Sprintf("Create ran in the %s guest (version %s)", desc.Language,
			report.Version))
	} else {
		v.table.row("load", stepFailed, *report.Error)
	}

	issues(v.table, report)

	return ok
}

// issues adds a row per issue of report.
func issues(table *table, report *adminjson.Report) {
	for _, issue := range report.Issues {
		at := ""
		if issue.Field != "" {
			at = " (" + issue.Field + ")"
		}

		table.row("", issue.Severity, fmt.Sprintf("%s%s: %s", issue.Code, at, issue.Message))
	}
}

// methodNames lists methods by name.
func methodNames(methods []goabi.Method) string {
	names := make([]string, len(methods))
	for i := range methods {
		names[i] = methods[i].Name
	}

	return strings.Join(names, ", ")
}

// createHost returns the name of the contract's interface a Go plugin's Create takes as its parameter, as the
// plugin's source writes it (contract.Host through an import of pkgPath, or Host through a dot import); empty
// when Create takes none, another one or cannot be found: the guest then tells what Create should be.
func createHost(files fs.FS, entrypoint, pkgPath string) string {
	dir := goPackageDir(entrypoint)

	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return ""
	}

	fset := token.NewFileSet()

	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || path.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}

		src, err := fs.ReadFile(files, path.Join(dir, name))
		if err != nil {
			continue
		}

		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			continue
		}

		if host, found := fileCreateHost(file, pkgPath); found {
			return host
		}
	}

	return ""
}

// fileCreateHost returns the Host the Create file declares takes (see createHost) and whether file declares
// Create.
func fileCreateHost(file *ast.File, pkgPath string) (string, bool) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == goabi.CreateFunc {
			return paramHost(fn.Type.Params.List, importName(file, pkgPath)), true
		}
	}

	return "", false
}

// paramHost returns the contract interface a parameter list of one parameter names, the contract package being
// imported under the name imported: Host for imported.Host, or for Host with a dot import; empty otherwise.
func paramHost(params []*ast.Field, imported string) string {
	if len(params) != 1 || len(params[0].Names) > 1 {
		return ""
	}

	switch typ := params[0].Type.(type) {
	case *ast.SelectorExpr:
		if x, ok := typ.X.(*ast.Ident); ok && imported != "" && x.Name == imported {
			return typ.Sel.Name
		}
	case *ast.Ident:
		if imported == "." {
			return typ.Name
		}
	default:
	}

	return ""
}

// importName is the name file imports pkgPath under: its explicit name, else the last element of its path (a
// package named otherwise is not recognized); empty when file does not import it.
func importName(file *ast.File, pkgPath string) string {
	for _, imp := range file.Imports {
		imported, err := strconv.Unquote(imp.Path.Value)
		if err != nil || imported != pkgPath {
			continue
		}

		if imp.Name != nil {
			return imp.Name.Name
		}

		return path.Base(pkgPath)
	}

	return ""
}
