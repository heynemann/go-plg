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
	"slices"
	"strconv"
	"strings"
	"time"

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
	"github.com/heynemann/go-plg/internal/goallow"
	"github.com/heynemann/go-plg/internal/gorestrict"
	"github.com/heynemann/go-plg/internal/modfetch"
	"github.com/heynemann/go-plg/internal/offline"
	"github.com/heynemann/go-plg/internal/testcase"
	"github.com/heynemann/go-plg/internal/tscheck"
	"github.com/heynemann/go-plg/internal/typings"
)

// plgModule is plg's module, whose version as this command was built decides the custom Go guests' plg.
const plgModule = "github.com/heynemann/go-plg"

// Colour modes of -color.
const (
	colourAuto   = "auto"
	colourAlways = "always"
	colourNever  = "never"
)

// defaultNearLimit is -near-limit's default: a call is near a limit at 80% of it.
const defaultNearLimit = 0.8

// runValidate runs `plg validate`.
func runValidate(env *Env, args []string) outcome {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	host := flags.String("host", "", "the contract's Host interface the plugin's Create takes")
	cacheDir := flags.String("cache-dir", "", "compilation cache directory")
	guestCache := flags.String("guest-cache", "", "directory of the custom Go guests built for contracts' "+
		"//plg:guest-packages (default <user cache>/plg/guests)")
	format := flags.String("format", formatTable, "table or json")
	junit := flags.String("junit", "", "also write the report as JUnit XML to this file")
	colour := flags.String("color", colourAuto, "colour the table: auto (on a terminal), always or never")
	nearLimit := flags.Float64("near-limit", defaultNearLimit, "flag test cases whose peak memory or time is at "+
		"or above this share of its limit")

	operands, res, ok := parseMixed("validate", flags, args)
	if !ok {
		return res
	}

	if msg := validateUsage(operands, *format, *colour, *nearLimit); msg != "" {
		return usageError("validate", msg)
	}

	dir := resolve(env.Dir, operands[0])
	run := validation{
		env: env, dir: dir, arg: operands[0], host: *host, cacheDir: resolve(env.Dir, *cacheDir),
		nearLimit: *nearLimit, ownership: true,
	}

	if *guestCache != "" {
		run.guestCache = resolve(env.Dir, *guestCache)
	}

	out := &output{format: *format, colour: wantsColour(env, *colour)}
	if *junit != "" {
		out.junit = resolve(env.Dir, *junit)
	}

	if authorrepo.Exists(dir) {
		return validateRepo(&run, out)
	}

	run.report = newPluginReport(operands[0], dir)
	rep := &report{Version: reportVersion, Plugins: []*pluginReport{run.report}}
	rep.Valid = run.run(context.Background())

	err := out.plugin(env.Stdout, rep, run.report)
	if err == nil {
		err = out.show(env.Stdout, rep)
	}

	if err != nil {
		return failure("validate", err)
	}

	if !rep.Valid {
		return outcome{code: exitFailure, msg: fmt.Sprintf("plg validate: %s is not a valid plugin\n", dir)}
	}

	return outcome{code: exitOK}
}

// validateUsage returns what is wrong with validate's operands and flags; empty when nothing is.
func validateUsage(operands []string, format, colour string, nearLimit float64) string {
	switch {
	case len(operands) != 1:
		return "name one plugin directory or author repository"
	case format != formatTable && format != formatJSON:
		return fmt.Sprintf("-format must be table or json, not %q", format)
	case colour != colourAuto && colour != colourAlways && colour != colourNever:
		return fmt.Sprintf("-color must be auto, always or never, not %q", colour)
	case !(nearLimit > 0 && nearLimit <= 1):
		return fmt.Sprintf("-near-limit must be above 0 and at most 1, not %v", nearLimit)
	default:
		return ""
	}
}

// wantsColour reports whether the table is coloured in mode (-color): always, never, or on a terminal unless
// NO_COLOR is set or TERM is dumb.
func wantsColour(env *Env, mode string) bool {
	switch mode {
	case colourAlways:
		return true
	case colourNever:
		return false
	default:
	}

	file, ok := env.Stdout.(*os.File)
	if !ok || env.Getenv("NO_COLOR") != "" || env.Getenv("TERM") == "dumb" {
		return false
	}

	info, err := file.Stat()

	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// validation is one run of plg validate (or plg types) on the plugin directory dir, named arg on the command line;
// each step records itself in report. repo is the author repository listing the plugin, if any, and files the
// plugin's files, the repository's shared directories copied in.
type validation struct {
	env    *Env
	report *pluginReport
	repo   *authorrepo.Repo
	// modules, when set, keeps the contract modules resolved, so that later runs (plg dev's) reuse them.
	modules    *modules
	files      fs.FS
	dir        string
	arg        string
	host       string
	cacheDir   string
	guestCache string   // empty means the default, <user cache>/plg/guests
	hosts      []string // the contract's Host candidates when there are several and none was chosen
	nearLimit  float64  // the share of a limit a test case's call is near it at (-near-limit)
	ownership  bool     // check the manifest's owner (validate does, types does not)
	// newPlugin is set by plg init: the plugin has no source yet, so its Host is -host's or the contract's only
	// candidate.
	newPlugin bool
}

// resolved is what the steps up to the interface found.
type resolved struct {
	loaded   *contractload.Loaded
	mod      modfetch.Module
	ref      contractref.Ref
	manifest []byte
	desc     plg.Descriptor
}

// run runs the steps in order and reports whether none failed. It stops at the first that fails, until the plugin
// loaded: then Validate's examples, the test cases and the resource report all run.
func (v *validation) run(ctx context.Context) bool {
	v.steps(ctx)
	v.report.finish()

	return v.report.Valid
}

// steps runs the steps.
func (v *validation) steps(ctx context.Context) {
	res, cleanup, ok := v.resolveContract(ctx)
	if !ok {
		return
	}

	defer cleanup()

	opts := v.options(&res.desc, res.loaded.Directives.Limits)

	if v.languageChecks(ctx, res, opts) && v.load(ctx, res.loaded.Contract, res.manifest, &res.desc, opts) {
		v.exercise(ctx, res.loaded.Contract, res.manifest, opts)
	}
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

// languageChecks runs the steps of the plugin's language: a Go plugin's go.mod pin, sandbox rules, type-check and
// custom guest, which it sets in opts; a Lua or TypeScript plugin's typings, then a Lua plugin's syntax and a
// TypeScript plugin's type-check against the typings.
func (v *validation) languageChecks(ctx context.Context, res *resolved, opts *offline.Options) bool {
	if res.desc.Language == plg.LanguageGo {
		return v.goSteps(ctx, res, opts)
	}

	typ, fresh := v.typings(res)
	if !fresh {
		return false
	}

	if res.desc.Language == plg.LanguageTypeScript {
		return v.tsTypes(ctx, typ, res)
	}

	return v.luaSyntax(ctx, opts)
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

// goSteps runs a Go plugin's own steps: its go.mod's pin, its sandbox rules, its type-check and its custom guest,
// which it sets in opts.
func (v *validation) goSteps(ctx context.Context, res *resolved, opts *offline.Options) bool {
	guestPackages := res.loaded.Directives.GuestPackages
	if !v.goMod(res.ref, res.mod) || !v.sandbox(res.ref.Package, &res.desc, guestPackages) ||
		!v.goTypes(res.loaded, &res.desc) {
		return false
	}

	var ok bool

	opts.GoGuest, ok = v.goGuest(ctx, guestPackages)

	return ok
}

// sandbox checks a Go plugin's source (its package directory, vendored packages included) against what the Go
// guest runs: no go statements, and imports only of what the guest provides: its allowlist, the contract package
// contract, the plugin's vendored packages and the contract's //plg:guest-packages, guestPackages.
func (v *validation) sandbox(contract string, desc *plg.Descriptor, guestPackages []string) bool {
	dir := goPackageDir(desc.Entrypoint)
	vendor := path.Join(dir, "vendor")

	err := gorestrict.Check(v.files, dir, func(imp string) bool {
		if goallow.Allowed(imp) || imp == contract || slices.Contains(guestPackages, imp) {
			return true
		}

		info, err := fs.Stat(v.files, path.Join(vendor, imp))

		return err == nil && info.IsDir()
	})
	if err != nil {
		v.failed("sandbox", err)

		if errors.Is(err, gorestrict.ErrImport) {
			v.report.hint("the Go guest provides its allowlist (the standard library packages plg lists and " +
				"github.com/heynemann/go-plg/plugin), the contract package, the plugin's vendor/ packages and the " +
				"contract's //plg:guest-packages")
		}

		return false
	}

	v.report.row("sandbox", resultOK, "no go statements, and imports only what the Go guest provides")

	return true
}

// luaSyntax compiles every .lua file of a Lua plugin, the modules it requires only when a call needs them
// included, with the Lua guest's compiler.
func (v *validation) luaSyntax(ctx context.Context, opts *offline.Options) bool {
	errs, err := offline.LuaSyntax(ctx, v.files, opts)
	if err != nil {
		return v.failed("syntax", err)
	}

	if len(errs) > 0 {
		v.report.row("syntax", resultFailed, fmt.Sprintf("%d .lua %s not compile", len(errs),
			plural(len(errs), "file does", "files do")))

		for _, msg := range errs {
			v.report.line(msg)
		}

		return false
	}

	v.report.row("syntax", resultOK, "every .lua file compiles")

	return true
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

	v.report.row("limits", resultOK, detail+" ("+limitsOrigin(declared, repo)+")")

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

	v.report.row("guest", resultOK, fmt.Sprintf("a custom Go guest with %s, %s the guest cache (%s)",
		strings.Join(pkgs, " "), how, guest.Path))

	return wasm, true
}

// failed adds the failed step's row.
func (v *validation) failed(step string, err error) bool {
	v.report.row(step, resultFailed, err.Error())

	return false
}

// manifest reads and parses the plugin's plugin.yaml and, for validate, checks its owner as a load does: the
// missing-ownership issue is a warning, an error (failing the step) when the author repository requires ownership.
func (v *validation) manifest() ([]byte, plg.Descriptor, bool) {
	data, err := os.ReadFile(filepath.Join(v.dir, "plugin.yaml"))
	if err != nil {
		return nil, plg.Descriptor{}, v.failed("manifest", err)
	}

	desc, err := plg.ParseManifest(data)
	if err != nil {
		return nil, plg.Descriptor{}, v.failed("manifest", err)
	}

	var missing []adminjson.Issue

	if v.ownership {
		missing, err = offline.Ownership(data, v.repo != nil && v.repo.RequireOwnership)
		if err != nil {
			return nil, plg.Descriptor{}, v.failed("manifest", err)
		}
	}

	result := issuesResult(missing)
	v.report.row("manifest", result, fmt.Sprintf("%s plugin %s implementing %s", desc.Language, v.key(),
		desc.Implements))

	for _, issue := range missing {
		v.report.issue(issue)
	}

	return data, desc, result != resultFailed
}

// issuesResult is the result of a step reporting issues: failed with an error issue, else warning with a warning
// issue, else ok.
func issuesResult(issues []adminjson.Issue) string {
	result := resultOK

	for _, issue := range issues {
		switch {
		case issue.Severity == string(plg.SeverityError):
			return resultFailed
		case issue.Severity == string(plg.SeverityWarning):
			result = resultWarning
		default:
		}
	}

	return result
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

	if mod, ok := v.modules.find(ref); ok {
		v.report.row("contract", resultOK, fmt.Sprintf("module %s %s (%s; resolved by an earlier run)", mod.Path,
			mod.Version, mod.Dir))

		return mod, true
	}

	fetcher := modfetch.Fetcher{Environ: v.env.environ()}

	mod, err := fetcher.Fetch(ctx, ref.Package, ref.Version)
	if err != nil {
		return modfetch.Module{}, v.failed("contract", err)
	}

	v.modules.keep(ref, mod)
	v.report.row("contract", resultOK, fmt.Sprintf("module %s %s (%s)", mod.Path, mod.Version, mod.Dir))

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

	host := v.chooseHost(loaded, ref, desc)
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

	v.report.row("interface", resultOK, detail)

	return loaded, true
}

// chooseHost returns the Host the plugin's Create takes, to set on loaded (see contract); empty when there is none
// to set, or the one its typings name is set already. A new plugin (plg init's) takes -host's, else the contract's
// only candidate.
func (v *validation) chooseHost(loaded *contractload.Loaded, ref contractref.Ref, desc *plg.Descriptor) string {
	switch {
	case v.host != "":
		return v.host
	case v.newPlugin:
		return v.defaultHost(loaded, ref.Package)
	case desc.Language == plg.LanguageGo:
		return createHost(v.files, desc.Entrypoint, ref.Package, loaded.PackageName())
	}

	// Typings written for an earlier contract may name a Host it no longer has: they are stale then, which the
	// typings step reports, so the Host is chosen as if there were none.
	host := typings.HostIn(string(desc.Language), v.files)
	if host != "" && loaded.SetHost(host) == nil {
		return ""
	}

	return v.defaultHost(loaded, ref.Package)
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
	typ, err := generateTypings(res, v.sharedDirs())
	if err != nil {
		return nil, v.failed("typings", err)
	}

	stale := typ.Stale(v.files)
	if len(stale) > 0 {
		return nil, v.failed("typings", fmt.Errorf("%w: %s not what plg types writes for %s: run %s",
			typings.ErrStale, strings.Join(stale, ", "), res.desc.Implements, v.typesCommand(res)))
	}

	v.report.row("typings", resultOK, strings.Join(typ.Names(), ", ")+" up to date")

	return typ, true
}

// generateTypings generates the typings of the plugin's language, with the editor configuration for shared.
func generateTypings(res *resolved, shared *typings.Shared) (*typings.Typings, error) {
	typ, err := typings.Generate(string(res.desc.Language), typingsContract(res), shared)
	if err != nil {
		return nil, describable(res, err)
	}

	return typ, nil
}

// typingsContract is the contract as the typings (and plg init's skeletons) describe it.
func typingsContract(res *resolved) *typings.Contract {
	contract := &typings.Contract{
		Interface: res.loaded.InterfaceType, Host: res.loaded.HostType, Implements: res.desc.Implements,
	}
	if res.loaded.ValidationType != nil {
		if named, ok := types.Unalias(res.loaded.ValidationType.Type()).(*types.Named); ok {
			contract.Validation = named
		}
	}

	return contract
}

// describable returns err, an error of the typings: a contract they cannot describe fails with the error loading
// a plugin against it would fail with, which names what breaks it.
func describable(res *resolved, err error) error {
	if errors.Is(err, typings.ErrContract) {
		broken := offline.CheckContract(res.loaded.Contract)
		if broken != nil {
			return broken //nolint:wrapcheck // plg's errors name the contract and what breaks it
		}
	}

	return err
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
		v.report.row("types", resultOK, fmt.Sprintf("the plugin type-checks against %s (%s)",
			res.loaded.Contract.Interface.Type, typings.TypeScriptFile))

		return true
	}

	for i := range diags {
		v.report.row("types", resultFailed, diags[i].String())
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
		v.report.row("types", resultWarning, err.Error())
	case err != nil:
		return v.failed("types", err)
	default:
		v.report.row("types", resultOK, "the plugin's package type-checks against "+
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
		v.report.row("go.mod", resultWarning, fmt.Sprintf("%s replaces %s by the directory %s: the pin %s is not "+
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
		v.report.row("go.mod", resultOK, fmt.Sprintf("%s requires %s %s", req.GoMod, mod.Path, req.Version))
	}

	return true
}

// load loads the plugin with plg's own loading code and runs its Create in the real guest, with opts: its limits,
// the author repository's ownership rule and its Go guest. The manifest step reported the ownership issue.
func (v *validation) load(ctx context.Context, contract *offline.Contract, manifest []byte, desc *plg.Descriptor,
	opts *offline.Options,
) bool {
	report, err := offline.Check(ctx, contract, v.plugin(manifest), opts)
	if err != nil {
		return v.failed("load", err)
	}

	ok := report.Error == nil
	if ok {
		v.report.row("load", resultOK, fmt.Sprintf("Create ran in the %s guest (version %s)", desc.Language,
			report.Version))
	} else {
		v.report.row("load", resultFailed, *report.Error)
	}

	if !ok && len(v.hosts) > 0 {
		v.report.hint(fmt.Sprintf("if Create takes a Host, pass -host <%s>", strings.Join(v.hosts, "|")))
	}

	for _, issue := range report.Issues {
		if issue.Code != plg.CodeOwnershipMissing {
			v.report.issue(issue)
		}
	}

	return ok
}

// plugin is the plugin validated, with its manifest's content.
func (v *validation) plugin(manifest []byte) *offline.Plugin {
	return &offline.Plugin{Files: v.files, Key: v.key(), Manifest: manifest}
}

// exercise runs the steps of the loaded plugin, each call in a fresh instance of the real guest, with contract's
// Host answered by the test cases' stubs (failing every call outside a case): Validate with each example input,
// the test cases, then the resource report of the cases' calls.
func (v *validation) exercise(ctx context.Context, contract *offline.Contract, manifest []byte,
	opts *offline.Options,
) {
	plugin := v.plugin(manifest)
	examples := testcase.Examples(contract, plugin.Files)
	files := testcase.Load(plugin.Files)

	stubbed := *contract
	if contract.Host != nil {
		host := *contract.Host
		host.Funcs = testcase.HostFuncs(&host) //nolint:contextcheck // each gets its call's context as an argument
		stubbed.Host = &host
	}

	var runner offline.Runner

	if len(examples) > 0 || len(files) > 0 {
		faking := *opts
		faking.Secrets = testcase.Secret // the running case's fakes

		var err error

		runner, err = offline.Open(ctx, &stubbed, plugin, &faking)
		if err != nil {
			v.failed("tests", err)

			return
		}
	}

	v.examples(ctx, &stubbed, runner, examples)

	calls := v.tests(ctx, &stubbed, plugin.Files, runner, files)

	if runner != nil {
		err := runner.Close(context.WithoutCancel(ctx))
		if err != nil {
			v.failed("tests", err)
		}
	}

	v.resources(calls)
}

// examples calls the plugin's Validate with each example validation input, as a service registered with
// plg.WithValidation would, adding a row per example (an error issue fails it: such a service would reject the
// plugin; without a validation input in the contract it is a warning, not used) and one summing them up, counting
// the examples not used apart from those that passed; without examples, a contract declaring a validation input
// gets a row saying so.
func (v *validation) examples(ctx context.Context, contract *offline.Contract, runner offline.Runner,
	examples []testcase.Example,
) {
	if len(examples) == 0 {
		if contract.Validation != nil {
			v.report.row("validate", resultOK, "no example inputs ("+testcase.ExamplesGlob+")")
		}

		return
	}

	failed, unused := 0, 0

	for i := range examples {
		switch {
		case errors.Is(examples[i].Err, testcase.ErrNoValidation):
			unused++

			v.report.about("example", resultWarning, examples[i].Name, "not used: "+testcase.ErrNoValidation.Error())
		case !v.example(ctx, contract, runner, &examples[i]):
			failed++
		default:
		}
	}

	if failed > 0 {
		v.failed("validate", fmt.Errorf("%w: %d of %d", errExamplesFailed, failed, len(examples)))

		return
	}

	passed := len(examples) - unused
	detail := fmt.Sprintf("%d example %s", passed, plural(passed, "input passes", "inputs pass"))

	if unused > 0 {
		detail += fmt.Sprintf(", %d not used", unused)
	}

	v.report.row("validate", resultOK, detail)
}

// errExamplesFailed reports example validation inputs that failed.
var errExamplesFailed = errors.New("example validation inputs failed")

// example calls Validate with the example input (one the contract declares a validation input for) and adds its
// row; it reports whether the example passed.
func (v *validation) example(ctx context.Context, contract *offline.Contract, runner offline.Runner,
	example *testcase.Example,
) bool {
	if example.Err != nil {
		v.report.about("example", resultFailed, example.Name, example.Err.Error())

		return false
	}

	outcome := runner.Run(ctx, testcase.ValidateWith(contract, example.Input))
	if outcome.Err != nil {
		v.report.about("example", resultFailed, example.Name, "Validate failed: "+outcome.Err.Error())

		return false
	}

	result, detail := exampleResult(outcome.Issues)
	v.report.about("example", result, example.Name, detail)

	for _, issue := range outcome.Issues {
		v.report.issue(issue)
	}

	return result != resultFailed
}

// measured is what a test case's call took.
type measured struct {
	call string // the case: its file and name
	used offline.Resources
}

// tests runs the test cases of files, each in a fresh instance by runner, adding a row per case (and the lines
// showing why it failed) and one summing them up. It returns what the calls that ran took.
func (v *validation) tests(ctx context.Context, contract *offline.Contract, plugin fs.FS, runner offline.Runner,
	files []*testcase.File,
) []measured {
	if len(files) == 0 {
		v.report.row("tests", resultOK, "no test cases ("+testcase.Glob+")")

		return nil
	}

	var calls []measured

	passed, failed := 0, 0

	for _, file := range files {
		if file.Err != nil {
			failed++

			v.report.row("test", resultFailed, file.Err.Error())

			continue
		}

		for _, result := range testcase.Run(ctx, contract, plugin, runner, file) {
			if v.result(file, &result) {
				passed++
			} else {
				failed++
			}

			if result.Resources.Memory > 0 {
				calls = append(calls, measured{call: file.Name + ": " + result.Case, used: result.Resources})
			}
		}
	}

	if failed > 0 {
		v.failed("tests", fmt.Errorf("%w: %d failed, %d passed", errTestsFailed, failed, passed))
	} else {
		v.report.row("tests", resultOK, fmt.Sprintf("%d passed", passed))
	}

	return calls
}

// result adds the rows of a case of file that ran: one, and the lines showing why it failed. It reports whether the
// case passed.
func (v *validation) result(file *testcase.File, result *testcase.Result) bool {
	subject := file.Name + ": " + result.Case

	if result.Passed() {
		v.report.about("test", resultOK, subject, "")

		return true
	}

	v.report.about("test", resultFailed, subject, result.Failure)

	for _, detail := range result.Details {
		v.report.line(detail)
	}

	return false
}

// errTestsFailed reports test cases that failed.
var errTestsFailed = errors.New("test cases failed")

// resources adds the resource report: a row per test case's call, with its peak memory and time against its
// limits, a warning when either is at or above the -near-limit share of its limit.
func (v *validation) resources(calls []measured) {
	for _, call := range calls {
		used := call.used
		memShare, timeShare := share(used.Memory, used.MemoryLimit), share(used.Duration, used.Timeout)

		var near []string

		if memShare >= v.nearLimit {
			near = append(near, "memory")
		}

		if timeShare >= v.nearLimit {
			near = append(near, "time")
		}

		result, detail := resultOK, fmt.Sprintf("memory %s of %s (%.0f%%), time %s of %s (%.0f%%)",
			mebibytes(used.Memory), mebibytes(used.MemoryLimit), percent*memShare, used.Duration.Round(roundTo),
			used.Timeout, percent*timeShare)
		if len(near) > 0 {
			result = resultWarning
			detail += fmt.Sprintf("; at or above %.0f%% of its %s %s", percent*v.nearLimit,
				strings.Join(near, " and "), plural(len(near), "limit", "limits"))
		}

		v.report.about("resources", result, call.call, detail)

		v.report.Resources = append(v.report.Resources, &callResources{
			Call: call.call, NearLimit: near, MemoryBytes: used.Memory, MemoryLimitBytes: used.MemoryLimit,
			DurationMs: durationMs(used.Duration), TimeoutMs: durationMs(used.Timeout),
		})
	}
}

// roundTo is what a call's time is rounded to in the resource report.
const roundTo = 10 * time.Microsecond

// percent turns a share into a percentage.
const percent = 100

// exampleResult is the result and detail of an example input Validate returned issues for.
//
//nolint:revive,gocritic // the result, then its detail (nonamedreturns forbids naming them)
func exampleResult(issues []adminjson.Issue) (string, string) {
	count := map[string]int{}
	for _, issue := range issues {
		count[issue.Severity]++
	}

	switch errs, warnings := count[string(plg.SeverityError)], count[string(plg.SeverityWarning)]; {
	case errs > 0:
		return resultFailed, fmt.Sprintf("%d error %s: a service validating with this input would reject the "+
			"plugin", errs, plural(errs, "issue", "issues"))
	case warnings > 0:
		return resultWarning, fmt.Sprintf("%d warning %s", warnings, plural(warnings, "issue", "issues"))
	case len(issues) > 0:
		return resultOK, fmt.Sprintf("%d %s", len(issues), plural(len(issues), "issue", "issues"))
	default:
		return resultOK, "no issues"
	}
}

// share returns used's share of limit; 0 without a limit.
func share[N uint64 | time.Duration](used, limit N) float64 {
	if limit <= 0 {
		return 0
	}

	return float64(used) / float64(limit)
}

// mebibytes formats bytes in MiB.
func mebibytes(bytes uint64) string {
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20)) //nolint:mnd // a MiB
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
