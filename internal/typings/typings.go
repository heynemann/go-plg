// Package typings generates the declarations Lua and TypeScript plugin authors commit next to their plugin, for
// their editors and for plg validate's TypeScript type-check: the interface a plugin implements (what Create
// returns), every type its methods take and return, with struct fields under the names Lua and TypeScript see
// (`json` tag names), the Host (the host global), the validation input and plg's Issue, and the globals each guest
// provides. They describe exactly what crosses: the types are planned as the wire codec plans them
// ([gen.Plan]), so a field tagged `json:"-"` is absent and nil-able values may be null. From the same plan it
// writes the skeletons plg init starts plugins from, in Go, Lua and TypeScript ([NewScaffold]).
package typings

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The files the typings consist of, at the plugin directory's root.
const (
	TypeScriptFile = "plg.d.ts"      // TypeScript declarations
	TSConfigFile   = "tsconfig.json" // the compiler options plg validate checks with, for editors
	LuaFile        = "plg.d.lua"     // Lua Language Server annotations
)

// The languages typings exist for, as plugin.yaml names them.
const (
	LanguageLua        = "lua"
	LanguageTypeScript = "typescript"
)

var (
	// ErrLanguage reports a language without typings: Go plugins import the contract package itself.
	ErrLanguage = errors.New("typings: no typings for this language")
	// ErrContract reports a contract the typings cannot describe.
	ErrContract = errors.New("typings: the contract cannot be described")
	// ErrNotGenerated reports a file in the way that plg types did not write.
	ErrNotGenerated = errors.New("typings: a file plg types did not write is in the way")
	// ErrStale reports committed typings that are not what plg types writes.
	ErrStale = errors.New("typings: stale")
)

// Contract is what typings describe.
type Contract struct {
	// Interface is the interface the plugins implement.
	Interface *types.Named
	// Host is the Host interface the plugins call, nil without one.
	Host *types.Named
	// Validation is the validation input type, nil without one.
	Validation *types.Named
	// Implements is plugin.yaml's implements value, recorded in the files' header.
	Implements string
}

// File is a file of the typings.
type File struct {
	Name string // relative to the plugin directory
	Data []byte
}

// Typings are the files generated for one plugin language.
type Typings struct {
	Language string
	// iface is the implemented interface's name in the declarations, e.g. cohort.Provider.
	iface string
	Files []File
}

// Generate generates the typings of the contract for language.
func Generate(language string, contract *Contract) (*Typings, error) {
	planned, err := newModel(contract)
	if err != nil {
		return nil, err
	}

	iface := planned.ref(planned.iface.obj)

	switch language {
	case LanguageTypeScript:
		return &Typings{
			Language: language,
			Files: []File{
				{Name: TypeScriptFile, Data: planned.typeScript()},
				{Name: TSConfigFile, Data: []byte(tsconfig)},
			},
			iface: iface,
		}, nil
	case LanguageLua:
		return &Typings{Language: language, Files: []File{{Name: LuaFile, Data: planned.lua()}}, iface: iface}, nil
	default:
		return nil, fmt.Errorf("%w: %s (Go plugins import the contract package itself)", ErrLanguage, language)
	}
}

// Names returns the names of the typings' files.
func (t *Typings) Names() []string {
	names := make([]string, len(t.Files))
	for i, file := range t.Files {
		names[i] = file.Name
	}

	return names
}

// Stale returns the names of the typings' files that plugin, the plugin's directory, lacks or holds with other
// contents.
func (t *Typings) Stale(plugin fs.FS) []string {
	var stale []string

	for _, file := range t.Files {
		data, err := fs.ReadFile(plugin, file.Name)
		if err != nil || !bytes.Equal(data, file.Data) {
			stale = append(stale, file.Name)
		}
	}

	return stale
}

// Written says what [Typings.Write] did with a file.
type Written struct {
	Name    string
	Changed bool // false when the file already held the typings
}

// fileMode is the mode of the files Write creates: source files.
const fileMode = 0o644

// Write writes the typings into the plugin directory dir. A file already holding them is left untouched; a file
// plg types did not write (it lacks the generated header) is never overwritten: Write then fails with
// [ErrNotGenerated] before writing anything.
func (t *Typings) Write(dir string) (_ []Written, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("typings: %w", err)
	}

	defer func() { err = errors.Join(err, root.Close()) }()

	written := make([]Written, len(t.Files))

	for i, file := range t.Files {
		written[i].Name = file.Name

		data, readErr := root.ReadFile(file.Name)

		switch {
		case errors.Is(readErr, fs.ErrNotExist):
			written[i].Changed = true
		case readErr != nil:
			return nil, fmt.Errorf("typings: %w", readErr)
		case !Generated(data):
			return nil, fmt.Errorf("%w: %s: move it away, then run plg types again", ErrNotGenerated,
				filepath.Join(dir, file.Name))
		default:
			written[i].Changed = !bytes.Equal(data, file.Data)
		}
	}

	for i, file := range t.Files {
		if !written[i].Changed {
			continue
		}

		writeErr := root.WriteFile(file.Name, file.Data, fileMode)
		if writeErr != nil {
			return nil, fmt.Errorf("typings: %w", writeErr)
		}
	}

	return written, nil
}

// The header line every generated file has among its first lines, after the comment marker of its language.
const generatedLine = "Code generated by plg types. DO NOT EDIT."

// headerLines is how many first lines may hold the generated line (Lua's start with ---@meta).
const headerLines = 3

// Generated reports whether data is a file plg types wrote: one of its first lines is the generated header.
func Generated(data []byte) bool {
	lines := bufio.NewScanner(bytes.NewReader(data))

	for range headerLines {
		if !lines.Scan() {
			return false
		}

		if comment, ok := commentText(lines.Text()); ok && comment == generatedLine {
			return true
		}
	}

	return false
}

// HostIn returns the Host interface the committed typings of a language in the plugin directory plugin were
// written for (see [HostOf]), empty without them or without a Host.
func HostIn(language string, plugin fs.FS) string {
	name := map[string]string{LanguageTypeScript: TypeScriptFile, LanguageLua: LuaFile}[language]
	if name == "" {
		return ""
	}

	data, err := fs.ReadFile(plugin, name)
	if err != nil {
		return ""
	}

	return HostOf(data)
}

// HostOf returns the Host interface a generated typings file was written for (its `Host:` header line), empty
// when it has none or is not generated.
func HostOf(data []byte) string {
	if !Generated(data) {
		return ""
	}

	lines := bufio.NewScanner(bytes.NewReader(data))
	for lines.Scan() {
		line := lines.Text()
		if line == metaLine {
			continue
		}

		comment, ok := commentText(line)
		if !ok {
			return ""
		}

		if host, found := strings.CutPrefix(comment, hostPrefix); found {
			return host
		}
	}

	return ""
}

// The header line naming the Host, and Lua's meta annotation.
const (
	hostPrefix       = "Host: "
	implementsPrefix = "Implements: "
	metaLine         = "---@meta"
)

// commentText returns the text of a line comment in TypeScript or Lua.
func commentText(line string) (string, bool) {
	for _, marker := range []string{"// ", "-- "} {
		if text, ok := strings.CutPrefix(line, marker); ok {
			return text, true
		}
	}

	return "", false
}

// header returns the generated files' header lines: the generated line, implements and the Host, then the
// description, each after the comment marker.
func (m *model) header(marker string, description ...string) string {
	var out strings.Builder

	out.WriteString(marker + " " + generatedLine + "\n")
	out.WriteString(marker + " " + implementsPrefix + m.contract.Implements + "\n")

	if m.host != nil {
		out.WriteString(marker + " " + hostPrefix + m.host.obj.Name() + "\n")
	}

	out.WriteString(marker + "\n")

	for _, line := range description {
		out.WriteString(marker + " " + line + "\n")
	}

	return out.String()
}
