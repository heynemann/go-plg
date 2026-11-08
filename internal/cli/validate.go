package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/authorrepo"
	"github.com/heynemann/go-plg/internal/buildguest"
	"github.com/heynemann/go-plg/internal/contractload"
	"github.com/heynemann/go-plg/internal/contractref"
	"github.com/heynemann/go-plg/internal/directive"
	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/goabi"
	"github.com/heynemann/go-plg/internal/modfetch"
	"github.com/heynemann/go-plg/internal/offline"
	"github.com/heynemann/go-plg/internal/testcase"
	"github.com/heynemann/go-plg/internal/tscheck"
	"github.com/heynemann/go-plg/internal/typings"
)

// plgModule is plg's module, whose version as this command was built decides the custom Go guests' plg.
const plgModule = "github.com/heynemann/go-plg"

// Step results.
const (
	stepOK      = "ok"
	stepFailed  = "FAIL"
	stepWarning = "warning"
	stepHint    = "hint"
)

// runValidate runs `plg validate`.
func runValidate(env *Env, args []string) outcome {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	host := flags.String("host", "", "the contract's Host interface the plugin's Create takes")
	cacheDir := flags.String("cache-dir", "", "compilation cache directory")
	guestCache := flags.String("guest-cache", "", "directory of the custom Go guests built for contracts' "+
		"//plg:guest-packages (default <user cache>/plg/guests)")

	operands, res, ok := parseMixed("validate", flags, args)
	if !ok {
		return res
	}

	if len(operands) != 1 {
		return usageError("validate", "name one plugin directory or author repository")
	}

	dir := resolve(env.Dir, operands[0])
	run := validation{env: env, dir: dir, arg: operands[0], host: *host, cacheDir: resolve(env.Dir, *cacheDir)}

	if *guestCache != "" {
		run.guestCache = resolve(env.Dir, *guestCache)
	}

	if authorrepo.Exists(dir) {
		return validateRepo(&run)
	}

	run.table = newStepTable(env.Stdout)
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

// validation is one run of plg validate (or plg types) on the plugin directory dir, named arg on the command line;
// each step adds its rows to table. repo is the author repository listing the plugin, if any, and files the
// plugin's files, the repository's shared directories copied in.
type validation struct {
	env        *Env
	table      *stepTable
	repo       *authorrepo.Repo
	files      fs.FS
	dir        string
	arg        string
	host       string
	cacheDir   string
	guestCache string   // empty means the default, <user cache>/plg/guests
	hosts      []string // the contract's Host candidates when there are several and none was chosen
}

// resolved is what the steps up to the interface found.
type resolved struct {
	loaded   *contractload.Loaded
	mod      modfetch.Module
	ref      contractref.Ref
	manifest []byte
	desc     plg.Descriptor
}

// run runs the steps in order, stopping at the first that fails; it reports whether every step passed.
func (v *validation) run(ctx context.Context) bool {
	res, cleanup, ok := v.resolveContract(ctx)
	if !ok {
		return false
	}

	defer cleanup()

	opts := v.options(&res.desc, res.loaded.Directives.Limits)

	return v.languageChecks(ctx, res, opts) && v.load(ctx, res.loaded.Contract, res.manifest, &res.desc, opts) &&
		v.tests(ctx, res.loaded.Contract, res.manifest, opts)
}

// options are the options the plugin loads and its test cases run with: what it writes and what plg logs go to
// stderr, under its limits (see limits) and the author repository's ownership rule.
func (v *validation) options(desc *plg.Descriptor, declared directive.Limits) *offline.Options {
	opts := &offline.Options{
		Logger: v.logger(), CacheDir: v.cacheDir, Limits: v.limits(desc, declared),
	}

	if v.repo != nil {
		opts.RequireOwnership = v.repo.RequireOwnership
	}

	return opts
}

// languageChecks runs the steps of the plugin's language: a Go plugin's go.mod pin, type-check and custom guest,
// which it sets in opts; a Lua or TypeScript plugin's typings, and a TypeScript plugin's type-check against them.
func (v *validation) languageChecks(ctx context.Context, res *resolved, opts *offline.Options) bool {
	if res.desc.Language == plg.LanguageGo {
		return v.goSteps(ctx, res.ref, res.mod, res.loaded, &res.desc, opts)
	}

	typ, fresh := v.typings(res)

	return fresh && (res.desc.Language != plg.LanguageTypeScript || v.tsTypes(ctx, typ, res))
}

// resolveContract runs the steps up to the interface: the author repository, the manifest, the shared
// directories, the contract module and its interface. When it succeeds the caller runs cleanup once done with
// the plugin's files.
func (v *validation) resolveContract(ctx context.Context) (*resolved, func(), bool) {
	if !v.repository() {
		return nil, nil, false
	}

	manifest, desc, ok := v.manifest()
	if !ok {
		return nil, nil, false
	}

	cleanup, ok := v.shared(&desc)
	if !ok {
		return nil, nil, false
	}

	res, ok := v.interfaceSteps(ctx, manifest, &desc)
	if !ok {
		cleanup()

		return nil, nil, false
	}

	return res, cleanup, true
}

// interfaceSteps resolves the contract module the plugin pins and describes its interface.
func (v *validation) interfaceSteps(ctx context.Context, manifest []byte, desc *plg.Descriptor) (*resolved, bool) {
	ref, err := contractref.Parse(desc.Implements) // ParseManifest checked it
	if err != nil {
		return nil, v.failed("manifest", err)
	}

	mod, ok := v.resolve(ctx, ref)
	if !ok {
		return nil, false
	}

	loaded, ok := v.contract(ref, mod, desc)
	if !ok {
		return nil, false
	}

	return &resolved{loaded: loaded, mod: mod, ref: ref, manifest: manifest, desc: *desc}, true
}

// goSteps runs a Go plugin's own steps: its go.mod's pin, its type-check and its custom guest, which it sets in
// opts.
func (v *validation) goSteps(ctx context.Context, ref contractref.Ref, mod modfetch.Module, loaded *contractload.Loaded,
	desc *plg.Descriptor, opts *offline.Options,
) bool {
	if !v.goMod(ref, mod) || !v.goTypes(loaded, desc) {
		return false
	}

	var ok bool

	opts.GoGuest, ok = v.goGuest(ctx, loaded.Directives.GuestPackages)

	return ok
}

// limits returns the limits the plugin runs under: the author repository's plg.yaml limits over the contract's
// //plg:limits over plg's defaults for its language. Its row says which came from where.
func (v *validation) limits(desc *plg.Descriptor, declared directive.Limits) directive.Limits {
	var repo directive.Limits

	if v.repo != nil {
		repo = v.repo.Limits
	}

	defaults := offline.Defaults(string(desc.Language))
	limits := defaults.Override(declared)
	limits = limits.Override(repo)

	detail := limits.String()
	if limits.Timeout == 0 {
		detail += ", no timeout"
	}

	v.table.row("limits", stepOK, detail+" ("+limitsOrigin(declared, repo)+")")

	return limits
}

// limitsOrigin says where limits come from: plg.yaml's (repo) over the contract's (declared), with their values
// when both set some, then plg's defaults for any neither sets.
func limitsOrigin(declared, repo directive.Limits) string {
	var (
		none    directive.Limits
		sources []string
	)

	for _, source := range []struct {
		name   string
		limits directive.Limits
	}{{authorrepo.FileName, repo}, {"//plg:limits", declared}} {
		switch {
		case source.limits == none:
		case declared != none && repo != none:
			sources = append(sources, source.name+" "+source.limits.String())
		default:
			sources = append(sources, source.name)
		}
	}

	if len(sources) == 0 {
		return "plg's defaults"
	}

	origin := strings.Join(sources, " over ")
	if set := declared.Override(repo); set.Memory == 0 || set.Timeout == 0 || set.PoolMax == 0 {
		origin += ", plg's defaults otherwise"
	}

	return origin
}

// goGuest returns the custom Go guest of the contract's //plg:guest-packages, built from the plugin's module as
// plg build-guest builds one from a service's, with the plg this command was built from (unless a local
// checkout, whose version says nothing: the module's plg then), and kept in the guest cache by what it is built
// from, so later runs find it there. Without guest packages it returns nil: the linked guest.
func (v *validation) goGuest(ctx context.Context, pkgs []string) ([]byte, bool) {
	if len(pkgs) == 0 {
		return nil, true
	}

	cache := v.guestCache
	if cache == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			return nil, v.failed("guest", fmt.Errorf("finding the guest cache: %w", err))
		}

		cache = filepath.Join(userCache, "plg", "guests")
	}

	opts := buildguest.Options{Dir: v.dir, Packages: pkgs, Environ: v.env.environ()}
	if built := contractref.Built(plgModule); !built.Devel() {
		opts.Plg = built.Version
	}

	guest, err := buildguest.Cached(ctx, &opts, cache)
	if err != nil {
		return nil, v.failed("guest", err)
	}

	wasm, err := os.ReadFile(guest.Path)
	if err != nil {
		return nil, v.failed("guest", err)
	}

	how := "built into"
	if guest.Hit {
		how = "found in"
	}

	v.table.row("guest", stepOK, fmt.Sprintf("a custom Go guest with %s, %s the guest cache (%s)",
		strings.Join(pkgs, " "), how, guest.Path))

	return wasm, true
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

// resolve fetches the contract module the plugin pins through the go command, or finds it in the module cache; a
// module the author repository replaces is its local checkout.
func (v *validation) resolve(ctx context.Context, ref contractref.Ref) (modfetch.Module, bool) {
	replaced, err := v.replaced(ref)

	switch {
	case err != nil:
		return modfetch.Module{}, v.failed("contract", err)
	case replaced != nil:
		return *replaced, true
	default:
	}

	fetcher := modfetch.Fetcher{Environ: v.env.environ()}

	mod, err := fetcher.Fetch(ctx, ref.Package, ref.Version)
	if err != nil {
		return modfetch.Module{}, v.failed("contract", err)
	}

	v.table.row("contract", stepOK, fmt.Sprintf("module %s %s (%s)", mod.Path, mod.Version, mod.Dir))

	return mod, true
}

// contract type-checks the contract package and describes the interface and the Host the plugin's Create takes:
// -host's, else the one a Go plugin's Create names, else for a Lua or TypeScript plugin the one its committed
// typings were written for, else the contract's only interface that passes as a Host.
func (v *validation) contract(ref contractref.Ref, mod modfetch.Module, desc *plg.Descriptor) (
	*contractload.Loaded, bool,
) {
	rel := strings.TrimPrefix(strings.TrimPrefix(ref.Package, mod.Path), "/")

	loaded, err := contractload.Load(filepath.Join(mod.Dir, filepath.FromSlash(rel)), ref.Package, ref.Interface, "")
	if err != nil {
		return nil, v.failed("interface", err)
	}

	host := v.host

	switch {
	case host != "":
	case desc.Language == plg.LanguageGo:
		host = createHost(v.files, desc.Entrypoint, ref.Package, loaded.PackageName())
	default:
		// Typings written for an earlier contract may name a Host it no longer has: they are stale then, which the
		// typings step reports, so the Host is chosen as if there were none.
		host = typings.HostIn(string(desc.Language), v.files)
		if host != "" && loaded.SetHost(host) == nil {
			host = ""
		} else {
			host = v.defaultHost(loaded, ref.Package)
		}
	}

	if host != "" {
		err = loaded.SetHost(host)
		if err != nil {
			return nil, v.failed("interface", err)
		}
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

// defaultHost returns the contract package's only interface that passes plg's Host checks, the Host of a Lua or
// TypeScript plugin; with several, it returns none and keeps their names for the hint after a failed load.
func (v *validation) defaultHost(loaded *contractload.Loaded, pkgPath string) string {
	var candidates []string

	for _, host := range loaded.Hosts() {
		if offline.CheckHost(pkgPath, host) == nil {
			candidates = append(candidates, host.Interface.Type.Name)
		}
	}

	if len(candidates) == 1 {
		return candidates[0]
	}

	v.hosts = candidates

	return ""
}

// typings checks that a Lua or TypeScript plugin's committed typings are those plg types generates.
func (v *validation) typings(res *resolved) (*typings.Typings, bool) {
	typ, err := generateTypings(res)
	if err != nil {
		return nil, v.failed("typings", err)
	}

	stale := typ.Stale(v.files)
	if len(stale) > 0 {
		return nil, v.failed("typings", fmt.Errorf("%w: %s not what plg types writes for %s: run %s",
			typings.ErrStale, strings.Join(stale, ", "), res.desc.Implements, v.typesCommand(res)))
	}

	v.table.row("typings", stepOK, strings.Join(typ.Names(), ", ")+" up to date")

	return typ, true
}

// generateTypings generates the typings of the plugin's language. A contract they cannot describe fails with the
// error loading a plugin against it would fail with, which names what breaks it.
func generateTypings(res *resolved) (*typings.Typings, error) {
	contract := &typings.Contract{
		Interface: res.loaded.InterfaceType, Host: res.loaded.HostType, Implements: res.desc.Implements,
	}
	if res.loaded.ValidationType != nil {
		if named, ok := types.Unalias(res.loaded.ValidationType.Type()).(*types.Named); ok {
			contract.Validation = named
		}
	}

	typ, err := typings.Generate(string(res.desc.Language), contract)
	if errors.Is(err, typings.ErrContract) {
		broken := offline.CheckContract(res.loaded.Contract)
		if broken != nil {
			return nil, broken //nolint:wrapcheck // plg's errors name the contract and what breaks it
		}
	}

	if err != nil {
		return nil, err //nolint:wrapcheck // typings' errors say what is wrong
	}

	return typ, nil
}

// typesCommand is the plg types command line that writes the plugin's typings.
func (v *validation) typesCommand(res *resolved) string {
	cmd := "plg types"
	if res.loaded.HostType != nil {
		cmd += " -host " + res.loaded.HostType.Obj().Name()
	}

	return cmd + " " + v.arg
}

// tsTypes type-checks a TypeScript plugin against its typings.
func (v *validation) tsTypes(ctx context.Context, typ *typings.Typings, res *resolved) bool {
	diags, err := v.typeCheck(ctx, typ, res.desc.Entrypoint)
	if err != nil {
		return v.failed("types", err)
	}

	if len(diags) == 0 {
		v.table.row("types", stepOK, fmt.Sprintf("the plugin type-checks against %s (%s)",
			res.loaded.Contract.Interface.Type, typings.TypeScriptFile))

		return true
	}

	for i := range diags {
		v.table.row("types", stepFailed, diags[i].String())
	}

	return false
}

// typeCheck runs the TypeScript compiler on the plugin, compiled through the compilation cache.
func (v *validation) typeCheck(ctx context.Context, typ *typings.Typings, entrypoint string) (
	_ []tscheck.Diagnostic, err error,
) {
	eng := engine.New(ctx, engine.Options{Logger: v.logger(), CacheDir: v.cacheDir})
	rt := eng.NewRuntime(ctx, engine.RuntimeOptions{MemoryLimit: tscheck.MemoryLimit})

	defer func() { err = errors.Join(err, rt.Close(ctx), eng.Close(ctx)) }()

	checker, err := tscheck.Compile(ctx, rt)
	if err != nil {
		return nil, err //nolint:wrapcheck // tscheck's errors name it
	}

	return typ.CheckTypeScript(ctx, checker, v.files, entrypoint) //nolint:wrapcheck // as above
}

// logger is plg's log during validate: on stderr.
func (v *validation) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(v.env.Stderr, nil))
}

// goTypes type-checks a Go plugin's package against the contract.
func (v *validation) goTypes(loaded *contractload.Loaded, desc *plg.Descriptor) bool {
	err := loaded.CheckGo(v.files, goPackageDir(desc.Entrypoint))

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

// goMod checks that a Go plugin's module requires the contract module at the version plugin.yaml pins. When they
// differ, it names the one to change: the older of the two, since the plugin's code may use what the newer
// version adds and its pin is the oldest contract a service may serve it with.
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
		change := fmt.Sprintf("raise the pin in %s to %s@%s", filepath.Join(v.dir, "plugin.yaml"), ref.Name(),
			req.Version)
		if semver.Compare(ref.Version, req.Version) > 0 {
			change = fmt.Sprintf("raise the requirement in %s to %s %s (go get %s@%s)", req.GoMod, mod.Path,
				ref.Version, mod.Path, ref.Version)
		}

		return v.failed("go.mod", fmt.Errorf("%w: %s requires %s %s, plugin.yaml pins %s: %s", modfetch.ErrGoMod,
			req.GoMod, mod.Path, req.Version, ref.Version, change))
	default:
		v.table.row("go.mod", stepOK, fmt.Sprintf("%s requires %s %s", req.GoMod, mod.Path, req.Version))
	}

	return true
}

// load loads the plugin with plg's own loading code and runs its Create in the real guest, with opts: its limits,
// the author repository's ownership rule and its Go guest.
func (v *validation) load(ctx context.Context, contract *offline.Contract, manifest []byte, desc *plg.Descriptor,
	opts *offline.Options,
) bool {
	report, err := offline.Check(ctx, contract, v.plugin(manifest), opts)
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

	if !ok && len(v.hosts) > 0 {
		v.table.row("", stepHint, fmt.Sprintf("if Create takes a Host, pass -host <%s>",
			strings.Join(v.hosts, "|")))
	}

	issues(v.table, report)

	return ok
}

// plugin is the plugin validated, with its manifest's content.
func (v *validation) plugin(manifest []byte) *offline.Plugin {
	return &offline.Plugin{Files: v.files, Key: v.key(), Manifest: manifest}
}

// tests runs the plugin's test cases, each in a fresh instance of the real guest, adding a row per case (and the
// lines showing why it failed) and one summing them up.
func (v *validation) tests(ctx context.Context, contract *offline.Contract, manifest []byte,
	opts *offline.Options,
) bool {
	plugin := v.plugin(manifest)

	files := testcase.Load(plugin.Files)
	if len(files) == 0 {
		v.table.row("tests", stepOK, "no test cases ("+testcase.Glob+")")

		return true
	}

	stubbed := *contract
	if contract.Host != nil {
		host := *contract.Host
		host.Funcs = testcase.HostFuncs(&host) //nolint:contextcheck // each gets its call's context as an argument
		stubbed.Host = &host
	}

	runner, err := offline.Open(ctx, &stubbed, plugin, opts)
	if err != nil {
		return v.failed("tests", err)
	}

	passed, failed := 0, 0

	for _, file := range files {
		if file.Err != nil {
			failed++

			v.table.row("test", stepFailed, file.Err.Error())

			continue
		}

		for _, result := range testcase.Run(ctx, &stubbed, plugin.Files, runner, file) {
			if v.result(file, &result) {
				passed++
			} else {
				failed++
			}
		}
	}

	err = runner.Close(context.WithoutCancel(ctx))
	if err != nil {
		return v.failed("tests", err)
	}

	if failed > 0 {
		return v.failed("tests", fmt.Errorf("%w: %d failed, %d passed", errTestsFailed, failed, passed))
	}

	v.table.row("tests", stepOK, fmt.Sprintf("%d passed", passed))

	return true
}

// result adds the rows of a case of file that ran: one, and the lines showing why it failed. It reports whether the
// case passed.
func (v *validation) result(file *testcase.File, result *testcase.Result) bool {
	if result.Passed() {
		v.table.row("test", stepOK, file.Name+": "+result.Case)

		return true
	}

	v.table.row("test", stepFailed, file.Name+": "+result.Case+": "+result.Failure)

	for _, detail := range result.Details {
		v.table.row("", "", detail)
	}

	return false
}

// errTestsFailed reports test cases that failed.
var errTestsFailed = errors.New("test cases failed")

// issues adds a row per issue of report.
func issues(table *stepTable, report *adminjson.Report) {
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
// plugin's source writes it (contract.Host through an import of pkgPath, whose package is named pkgName, or Host
// through a dot import); empty when Create takes none, another one or cannot be found: the guest then tells what
// Create should be.
func createHost(files fs.FS, entrypoint, pkgPath, pkgName string) string {
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

		if host, found := fileCreateHost(file, pkgPath, pkgName); found {
			return host
		}
	}

	return ""
}

// fileCreateHost returns the Host the Create file declares takes (see createHost) and whether file declares
// Create.
func fileCreateHost(file *ast.File, pkgPath, pkgName string) (string, bool) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == goabi.CreateFunc {
			return paramHost(fn.Type.Params.List, importName(file, pkgPath, pkgName)), true
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

// importName is the name file imports pkgPath, the package pkgName, under: its explicit name, else pkgName; empty
// when file does not import it.
func importName(file *ast.File, pkgPath, pkgName string) string {
	for _, imp := range file.Imports {
		imported, err := strconv.Unquote(imp.Path.Value)
		if err != nil || imported != pkgPath {
			continue
		}

		if imp.Name != nil {
			return imp.Name.Name
		}

		return pkgName
	}

	return ""
}
