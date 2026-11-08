package typings

import (
	"fmt"
	"go/types"
	"strings"

	"github.com/heynemann/go-plg/internal/layout"
)

// luaGlobals annotates what plg gives every Lua plugin besides Lua's own libraries (the Lua guest's glue):
// plg's shared types, http, secrets and json.
const luaGlobals = `---A time.Time: Unix seconds and nanoseconds (0 to 999999999), in UTC.
---@class plg.Time
---@field seconds integer
---@field nanos integer

---An issue's severity: any other value counts as an error.
---@alias plg.Severity "error"|"warning"|"info"

---An issue a plugin's Validate reports; a missing field is empty.
---@class plg.Issue
---@field severity? plg.Severity
---@field code? string
---@field message? string
---@field field? string

---An HTTP request: method defaults to GET; redirect = false returns redirects instead of following them.
---@class plg.Request
---@field method? string
---@field url string
---@field headers? table<string, string|string[]>
---@field body? string
---@field redirect? boolean

---An HTTP response; several values of a header are joined with ", ".
---@class plg.Response
---@field status integer
---@field headers table<string, string>
---@field body string

---@class plg.HTTP
---@field request fun(req: plg.Request): plg.Response|nil, string?

---HTTP requests through the service, under its HTTP policy: request returns nil and the reason when refused or failed.
---@type plg.HTTP
http = {}

---@class plg.Secrets
---@field get fun(name: string): string

---The secrets plugin.yaml declares (secrets: [...]), which the service resolves: get returns a secret's value and
---raises an error when plugin.yaml does not declare it or it cannot be resolved.
---@type plg.Secrets
secrets = {}

---@class plg.JSON
---@field encode fun(value: any): string
---@field decode fun(text: string): any
---@field array fun(t?: table): table
---@field object fun(t?: table): table
---@field null lightuserdata

---The bundled JSON module (also require("json")).
---@type plg.JSON
json = {}
`

// lua renders plg.d.lua.
func (m *model) lua() []byte {
	var out strings.Builder

	out.WriteString(metaLine + "\n")
	out.WriteString(m.header("--",
		"Lua Language Server annotations for plugins of this interface: Create returns a "+m.ref(m.iface.obj)+",",
		"a table of its methods (annotate it: ---@class MyPlugin: "+m.ref(m.iface.obj)+"); struct fields have their",
		"json names, and nil-able values (slices, maps, pointers, []byte) may be nil. A method returns its result,",
		"or an array table of several, and fails with error(...) or nil and a message.",
		"Regenerate them with `plg types` when plugin.yaml's implements changes."))
	out.WriteString("\n")
	out.WriteString(luaGlobals)

	for _, space := range m.namespaces() {
		for _, obj := range m.declsIn(space) {
			out.WriteString("\n")
			m.luaDecl(&out, obj)
		}
	}

	if m.host != nil {
		out.WriteString("\n---The service's Host: each method returns its result (several in an array table) and " +
			"raises the Host's error.\n")
		fmt.Fprintf(&out, "---@type %s\nhost = {}\n", m.ref(m.host.obj))
	}

	return []byte(out.String())
}

// luaDecl annotates obj.
func (m *model) luaDecl(out *strings.Builder, obj *types.TypeName) {
	ref := m.ref(obj)

	switch {
	case obj == m.iface.obj:
		fmt.Fprintf(out, "---The interface plugins implement: what Create returns (%s).\n---@class %s\n", goName(obj),
			ref)
		m.luaMethods(out, m.iface, "self: "+ref)

		if m.validation != nil && !m.iface.has("Validate") {
			fmt.Fprintf(out, "---@field Validate? fun(self: %s, input: %s): plg.Issue[]|nil\n", ref,
				m.luaType(m.validation))
		}

		if !m.iface.has("Close") {
			fmt.Fprintf(out, "---@field Close? fun(self: %s)\n", ref)
		}
	case m.host != nil && obj == m.host.obj:
		fmt.Fprintf(out, "---The service's Host (%s), the host global.\n---@class %s\n", goName(obj), ref)
		m.luaMethods(out, m.host, "")
	default:
		n := m.nodes[obj]
		fmt.Fprintf(out, "---%s\n", goName(obj))

		if n.Kind != layout.Struct {
			fmt.Fprintf(out, "---@alias %s %s\n", ref, m.luaShape(n))

			return
		}

		fmt.Fprintf(out, "---@class %s\n", ref)

		for _, field := range n.Fields {
			fmt.Fprintf(out, "---@field %s %s\n", luaKey(field.Name), m.luaType(field.Node))
		}
	}
}

// luaMethods annotates an interface's methods as fields holding functions, with self first when given.
func (m *model) luaMethods(out *strings.Builder, iface *ifaceModel, self string) {
	for _, meth := range iface.methods {
		names := paramNames(meth.params)

		params := make([]string, 0, len(meth.params)+1)
		if self != "" {
			params = append(params, self)
		}

		for i, p := range meth.params {
			params = append(params, names[i]+": "+m.luaType(p.node))
		}

		fmt.Fprintf(out, "---@field %s fun(%s)%s\n", meth.name, strings.Join(params, ", "), m.luaResults(meth.results))
	}
}

// luaResults returns a function's result annotation: none, the one result, or an array table of several.
func (m *model) luaResults(results []*node) string {
	switch len(results) {
	case 0:
		return ""
	case 1:
		return ": " + m.luaType(results[0])
	}

	items := make([]string, len(results))
	for i, r := range results {
		items[i] = fmt.Sprintf("[%d]: %s", i+1, m.luaType(r))
	}

	return ": { " + strings.Join(items, ", ") + " }"
}

// luaType returns the type of n: its name when it is declared, else its shape.
func (m *model) luaType(n *node) string {
	if obj := declared(n); obj != nil {
		return m.ref(obj)
	}

	return m.luaShape(n)
}

// luaScalars are the Lua Language Server types of the scalar shapes; TestScalarTablesSpellEveryScalarShape holds
// it to every one.
//
//nolint:gochecknoglobals // immutable lookup table
var luaScalars = map[shape]string{
	shapeBool: "boolean", shapeInteger: "integer", shapeFloat: "number", shapeString: "string",
	shapeBytes: "string|nil", shapeTime: "plg.Time",
}

// luaShape returns the type of n's wire kind, naming the types it holds.
func (m *model) luaShape(n *node) string {
	switch shp := shapeOf(n.Kind); shp {
	case shapeBool, shapeInteger, shapeFloat, shapeString, shapeBytes, shapeTime:
		return luaScalars[shp]
	case shapeSlice:
		elem := m.luaType(n.Elem)
		if strings.HasSuffix(elem, "|nil") {
			elem = "(" + elem + ")"
		}

		return elem + "[]|nil"
	case shapeMap:
		return "table<string, " + m.luaType(n.Elem) + ">|nil"
	case shapePointer:
		elem := m.luaType(n.Elem)
		if strings.HasSuffix(elem, "|nil") {
			return elem
		}

		return elem + "|nil"
	case shapeStruct:
		return m.luaStruct(n)
	default:
		panic(noShape(shp))
	}
}

// luaStruct returns the table type of the struct n.
func (m *model) luaStruct(n *node) string {
	if len(n.Fields) == 0 {
		return "table"
	}

	fields := make([]string, len(n.Fields))
	for i, field := range n.Fields {
		fields[i] = luaKey(field.Name) + ": " + m.luaType(field.Node)
	}

	return "{ " + strings.Join(fields, ", ") + " }"
}

// luaKey returns a field name: as it is when it is an identifier (and no keyword), else as ["name"].
func luaKey(name string) string {
	if identifier.MatchString(name) && !strings.Contains(name, "$") && !luaKeywords[name] {
		return name
	}

	return "[" + quote(name) + "]"
}
