package typings_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// annotations reads Lua Language Server annotations (https://luals.github.io/wiki/annotations/) the way the
// language server's grammar defines them, for the subset plg types writes: ---@meta, ---@class Name[: Parent],
// ---@field name[?] type, ---@alias Name type and ---@type type, where a type is a union (|) of names (dotted, an
// optional ? after), string literals, fun(params): results, { key: type, ... } table literals, table<K, V>, (type),
// each followed by any number of [].
type annotations struct {
	declared map[string]bool // classes and aliases
	used     map[string]int  // type names referred to, with the line of their first use
}

var (
	luaName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*`)
	luaString = regexp.MustCompile(`^"(?:[^"\\]|\\.)*"`)
	luaIndex  = regexp.MustCompile(`^\d+$`)

	errMalformed = errors.New("malformed annotation")
)

// builtin reports whether name is one of the language server's builtin types.
func builtin(name string) bool {
	switch name {
	case "nil", "any", "boolean", "string", "number", "integer", "table", "lightuserdata", "function", "userdata",
		"thread":
		return true
	default:
		return false
	}
}

// parseAnnotations parses every annotation line of a Lua file.
func parseAnnotations(t *testing.T, data []byte) *annotations {
	t.Helper()

	parsed := &annotations{declared: map[string]bool{}, used: map[string]int{}}
	lines := bufio.NewScanner(bytes.NewReader(data))

	for n := 1; lines.Scan(); n++ {
		line := lines.Text()

		text, ok := strings.CutPrefix(line, "---@")
		if !ok {
			continue
		}

		require.NoError(t, parsed.line(text, n), "line %d: %s", n, line)
	}

	return parsed
}

func (a *annotations) line(text string, n int) error {
	tag, rest, _ := strings.Cut(text, " ")

	switch tag {
	case "meta":
		return expectEnd(rest)
	case "class":
		name, parent, _ := strings.Cut(rest, ": ")

		err := a.declare(name)
		if err != nil || parent == "" {
			return err
		}

		return a.wholeType(parent, n)
	case "alias":
		name, typ, _ := strings.Cut(rest, " ")

		err := a.declare(name)
		if err != nil {
			return err
		}

		return a.wholeType(typ, n)
	case "field":
		typ, err := fieldType(rest)
		if err != nil {
			return err
		}

		return a.wholeType(typ, n)
	case "type":
		return a.wholeType(rest, n)
	default:
		return fmt.Errorf("%w: unknown annotation %q", errMalformed, tag)
	}
}

// declare declares a class or alias name.
func (a *annotations) declare(name string) error {
	if name == "" || luaName.FindString(name) != name {
		return fmt.Errorf("%w: bad name %q", errMalformed, name)
	}

	a.declared[name] = true

	return nil
}

// fieldType returns the type of a field annotation, after its name (an identifier with an optional ?, or
// ["..."]).
func fieldType(rest string) (string, error) {
	if key := luaString.FindString(strings.TrimPrefix(rest, "[")); strings.HasPrefix(rest, "[") && key != "" {
		after, ok := strings.CutPrefix(rest[1+len(key):], "] ")
		if !ok {
			return "", fmt.Errorf("%w: unclosed field key in %q", errMalformed, rest)
		}

		return after, nil
	}

	name := luaName.FindString(rest)

	after, ok := strings.CutPrefix(strings.TrimPrefix(rest[len(name):], "?"), " ")
	if name == "" || !ok {
		return "", fmt.Errorf("%w: no type after field %q", errMalformed, name)
	}

	return after, nil
}

func expectEnd(rest string) error {
	if rest != "" {
		return fmt.Errorf("%w: unexpected %q", errMalformed, rest)
	}

	return nil
}

// wholeType parses text as one type and nothing more.
func (a *annotations) wholeType(text string, n int) error {
	rest, err := a.typ(text, n)
	if err != nil {
		return err
	}

	return expectEnd(rest)
}

// typ parses a type at the start of text and returns what follows it.
func (a *annotations) typ(text string, n int) (string, error) {
	for {
		rest, err := a.term(text, n)
		if err != nil {
			return "", err
		}

		for strings.HasPrefix(rest, "[]") {
			rest = rest[2:]
		}

		if !strings.HasPrefix(rest, "|") {
			return rest, nil
		}

		text = rest[1:]
	}
}

func (a *annotations) term(text string, n int) (string, error) {
	switch {
	case strings.HasPrefix(text, "fun("):
		return a.fun(text[len("fun("):], n)
	case strings.HasPrefix(text, "table<"):
		return a.table(text[len("table<"):], n)
	case strings.HasPrefix(text, "{ "):
		return a.tableLiteral(text[2:], n)
	case strings.HasPrefix(text, "("):
		rest, err := a.typ(text[1:], n)
		if err != nil {
			return "", err
		}

		return cut(rest, ")")
	case strings.HasPrefix(text, `"`):
		literal := luaString.FindString(text)
		if literal == "" {
			return "", fmt.Errorf("%w: unterminated string in %q", errMalformed, text)
		}

		return text[len(literal):], nil
	default:
		return a.name(text, n)
	}
}

// name reads a type name, with an optional ?, noting its use.
func (a *annotations) name(text string, n int) (string, error) {
	name := luaName.FindString(text)
	if name == "" {
		return "", fmt.Errorf("%w: no type at %q", errMalformed, text)
	}

	if _, seen := a.used[name]; !seen && !builtin(name) {
		a.used[name] = n
	}

	return strings.TrimPrefix(text[len(name):], "?"), nil
}

// table reads the rest of table<K, V>.
func (a *annotations) table(text string, n int) (string, error) {
	rest, err := a.typ(text, n)
	if err != nil {
		return "", err
	}

	rest, ok := strings.CutPrefix(rest, ", ")
	if !ok {
		return "", fmt.Errorf("%w: table<K, V> without its comma: %q", errMalformed, text)
	}

	rest, err = a.typ(rest, n)
	if err != nil {
		return "", err
	}

	return cut(rest, ">")
}

// fun parses the parameters and results of fun( ... ): results.
func (a *annotations) fun(text string, n int) (string, error) {
	rest := text

	for !strings.HasPrefix(rest, ")") {
		name := luaName.FindString(rest)
		if name == "" || strings.Contains(name, ".") {
			return "", fmt.Errorf("%w: bad parameter at %q", errMalformed, rest)
		}

		after, ok := strings.CutPrefix(strings.TrimPrefix(rest[len(name):], "?"), ": ")
		if !ok {
			return "", fmt.Errorf("%w: parameter %s without a type", errMalformed, name)
		}

		var err error

		rest, err = a.typ(after, n)
		if err != nil {
			return "", err
		}

		rest = strings.TrimPrefix(rest, ", ")
	}

	rest = rest[1:]

	results, ok := strings.CutPrefix(rest, ": ")
	if !ok {
		return rest, nil
	}

	for {
		var err error

		rest, err = a.typ(results, n)
		if err != nil {
			return "", err
		}

		next, more := strings.CutPrefix(rest, ", ")
		if !more {
			return rest, nil
		}

		results = next
	}
}

// tableLiteral parses the fields of { key: type, ... }.
func (a *annotations) tableLiteral(text string, n int) (string, error) {
	rest := text

	for {
		var key string

		switch {
		case strings.HasPrefix(rest, "["):
			end := strings.Index(rest, "]: ")
			if end < 0 {
				return "", fmt.Errorf("%w: unclosed key in %q", errMalformed, rest)
			}

			key, rest = rest[1:end], rest[end+3:]
			if !luaString.MatchString(key) && !luaIndex.MatchString(key) {
				return "", fmt.Errorf("%w: bad key %q", errMalformed, key)
			}
		default:
			key = luaName.FindString(rest)

			var ok bool

			rest, ok = strings.CutPrefix(rest[len(key):], ": ")
			if key == "" || !ok {
				return "", fmt.Errorf("%w: bad field at %q", errMalformed, rest)
			}
		}

		var err error

		rest, err = a.typ(rest, n)
		if err != nil {
			return "", err
		}

		if after, ok := strings.CutPrefix(rest, " }"); ok {
			return after, nil
		}

		rest, _ = strings.CutPrefix(rest, ", ")
	}
}

func cut(text, prefix string) (string, error) {
	rest, ok := strings.CutPrefix(text, prefix)
	if !ok {
		return "", fmt.Errorf("%w: want %q at %q", errMalformed, prefix, text)
	}

	return rest, nil
}

// TestLuaAnnotationsParse parses every annotation of the golden Lua typings with the language server's grammar
// and resolves every type they name: each is a builtin or a class or alias the file declares. The file is valid
// Lua too: the cli tests run it in the Lua guest.
func TestLuaAnnotationsParse(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "golden", "plg.d.lua"))
	require.NoError(t, err)

	parsed := parseAnnotations(t, data)

	for name, line := range parsed.used {
		assert.True(t, parsed.declared[name], "line %d uses %s, which nothing declares", line, name)
	}

	for _, name := range []string{
		"everything.Store", "everything.Directory", "everything.Limits", "everything.Item", "everything.Tree",
		"time.Duration", "plg.Time", "plg.Issue", "plg.Request", "plg.Response",
	} {
		assert.True(t, parsed.declared[name], name)
	}

	assert.True(t, bytes.HasPrefix(data, []byte("---@meta\n")), "a meta file, which the language server never runs")
}

// TestLuaAnnotationParserRejectsMalformedAnnotations holds the parser above to the grammar, so that the golden
// file passing it means something.
func TestLuaAnnotationParserRejectsMalformedAnnotations(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"field name", "field x fun(a): ", "field x table<string>", "field x { a: integer", "alias A",
		"class", "type (string", "field [\"a\" string", "type fun(a integer)", "nonsense x", "meta x",
	} {
		parser := &annotations{declared: map[string]bool{}, used: map[string]int{}}
		assert.ErrorIs(t, parser.line(bad, 1), errMalformed, bad)
	}
}
