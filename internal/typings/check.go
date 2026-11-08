package typings

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/heynemann/go-plg/internal/engine"
	"github.com/heynemann/go-plg/internal/memfs"
	"github.com/heynemann/go-plg/internal/tscheck"
)

// Where the type-check mounts the plugin and its own project.
const (
	pluginMount = "/plugin"
	checkMount  = "/plg"
	checkFile   = "check.ts"
)

// CheckTypeScript type-checks a TypeScript plugin, the directory plugin whose entrypoint is the file entrypoint,
// against the typings, with the compiler options of their tsconfig.json: the plugin's own files (each against
// the declarations), and that what its Create returns implements the interface. The plugin's typings must be
// these (see [Typings.Stale]). Diagnostics name the plugin's files relative to its directory, in the compiler's
// order; those about what Create returns come last, placed at the plugin's export of Create (else at the
// entrypoint's start), their message starting with "Create: ".
func (t *Typings) CheckTypeScript(ctx context.Context, checker *tscheck.Checker, plugin fs.FS,
	entrypoint string,
) ([]tscheck.Diagnostic, error) {
	if t.Language != LanguageTypeScript {
		return nil, fmt.Errorf("%w: %s typings check no TypeScript", ErrLanguage, t.Language)
	}

	project, err := checkProject(entrypoint, t.iface)
	if err != nil {
		return nil, err
	}

	diags, err := checker.Check(ctx, pluginMount, checkMount+"/"+TSConfigFile,
		engine.Mount{FS: plugin, GuestPath: pluginMount}, engine.Mount{FS: project, GuestPath: checkMount})
	if err != nil {
		return nil, fmt.Errorf("typings: %w", err)
	}

	var (
		own, create []tscheck.Diagnostic
		source      []byte // the entrypoint, read once a diagnostic is about Create
	)

	for _, diag := range diags {
		file, inPlugin := relative(diag.File)
		if inPlugin {
			diag.File = file
			own = append(own, diag)

			continue
		}

		if source == nil {
			source, _ = fs.ReadFile(plugin, entrypoint) //nolint:errcheck // without it the position is 1:1
		}

		diag.File, diag.Message = entrypoint, "Create: "+diag.Message
		placeAtCreate(&diag, source)

		create = append(create, diag)
	}

	return append(own, create...), nil
}

// checkProject returns the type-check's own project: a tsconfig.json extending the plugin's, over the plugin's
// files and check.ts, which passes the plugin's Create where a function returning iface is expected.
func checkProject(entrypoint, iface string) (*memfs.FS, error) {
	config := `{"extends": "../plugin/` + TSConfigFile + `", "include": ["../plugin/**/*"], "files": ["` + checkFile +
		`"]}` + "\n"
	check := `import { Create } from ` + strconv.Quote("../plugin/"+entrypoint) + ";\n" +
		"declare function implementsInterface(create: (...args: never[]) => " + iface + " | Promise<" + iface +
		">): void;\n" +
		"implementsInterface(Create);\n"

	var project memfs.Builder

	err := project.Add(TSConfigFile, []byte(config))
	if err == nil {
		err = project.Add(checkFile, []byte(check))
	}

	if err != nil {
		return nil, fmt.Errorf("typings: %w", err)
	}

	files, err := project.Build()
	if err != nil {
		return nil, fmt.Errorf("typings: %w", err)
	}

	return files, nil
}

// relative returns the plugin-relative name of a file the compiler names relative to the plugin's directory,
// and whether it is the plugin's ("" and true for a diagnostic without a file); false for the type-check's own
// files.
func relative(name string) (string, bool) {
	if name == "" {
		return "", true
	}

	abs := path.Join(pluginMount, name)
	if rest, ok := strings.CutPrefix(abs, pluginMount+"/"); ok {
		return rest, true
	}

	return abs, !strings.HasPrefix(abs, checkMount+"/")
}

// exportsCreate finds the declaration exporting Create in TypeScript or JavaScript source: the name's position
// in `export [async] function Create`, `export const|let|var Create` or `export { ..., Create, ... }`.
var exportsCreate = regexp.MustCompile(
	`\bexport\s+(?:async\s+)?function\s*\*?\s*(Create)\b|\bexport\s+(?:const|let|var)\s+(Create)\b|` +
		`\bexport\s*\{[^}]*?\b(Create)\b`)

// placeAtCreate sets diag's line and column (in UTF-16 code units, as the compiler counts them) to where source
// exports Create, or to 1, 1.
func placeAtCreate(diag *tscheck.Diagnostic, source []byte) {
	diag.Line, diag.Column = 1, 1

	match := exportsCreate.FindSubmatchIndex(source)
	if match == nil {
		return
	}

	offset := 0

	for group := 2; group < len(match); group += 2 {
		if match[group] >= 0 {
			offset = match[group]

			break
		}
	}

	before := string(source[:offset])
	start := strings.LastIndexByte(before, '\n') + 1

	diag.Line, diag.Column = strings.Count(before, "\n")+1, len(utf16.Encode([]rune(before[start:])))+1
}
