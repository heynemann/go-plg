package typings

import (
	"fmt"
	"go/types"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/heynemann/go-plg/internal/layout"
)

// tsconfig is the generated tsconfig.json: the options plg validate type-checks a TypeScript plugin with, which
// editors then share. The guest runs ECMAScript 2023 (QuickJS-ng) with plg's globals only; esbuild bundles
// relative imports with or without their extension, JSON and text files. In an author repository with shared
// directories, rootDirs merges the plugin's directory with the repository's root, so that editors resolve
// `./lib/tiers` from the plugin's root to the shared directory at its repository path, as validate's copy does.
func tsconfig(shared *Shared) []byte {
	rootDirs := ""
	if shared != nil {
		rootDirs = `    "rootDirs": [".", ` + strconv.Quote(shared.Root) + `],` + "\n"
	}

	return []byte(`// ` + generatedLine + `
// The compiler options plg validate type-checks this plugin with: the ECMAScript 2023 built-ins and plg's globals
// (` + TypeScriptFile + `), no DOM or Node.js.
{
  "compilerOptions": {
` + rootDirs + `    "target": "es2023",
    "lib": ["es2023"],
    "types": [],
    "module": "esnext",
    "moduleResolution": "bundler",
    "allowImportingTsExtensions": true,
    "resolveJsonModule": true,
    "allowJs": true,
    "isolatedModules": true,
    "strict": true,
    "noEmit": true
  }
}
`)
}

// luarc is the generated .luarc.json of a Lua plugin in an author repository with shared directories: the Lua
// Language Server's workspace library lists them, so that editors resolve `require("lib.tiers")` to the shared
// directory at its repository path, as validate's copy does.
func luarc(shared *Shared) []byte {
	library := make([]string, len(shared.Dirs))
	for i, dir := range shared.Dirs {
		library[i] = strconv.Quote(path.Join(shared.Root, dir))
	}

	return []byte(`// ` + generatedLine + `
// The Lua Language Server's settings for this plugin: the repository's shared directories it requires.
{
  "workspace.library": [` + strings.Join(library, ", ") + `]
}
`)
}

// tsGlobals declares what plg gives every TypeScript plugin besides the ECMAScript built-ins (the TypeScript
// guest's prelude): plg's shared types, console, fetch, Headers, Response, secrets and text imports.
const tsGlobals = `declare namespace plg {
  /** A time.Time: Unix seconds and nanoseconds (0 to 999999999), in UTC. */
  interface Time {
    seconds: number;
    nanos: number;
  }

  /** What plg takes from the plugin where a value of type T is expected: a Date also stands for a time. */
  type Out<T> =
    T extends Time ? Time | Date : T extends Uint8Array ? T : T extends object ? { [K in keyof T]: Out<T[K]> } : T;

  /** A method's result, returned as it is or as a Promise. */
  type Result<T> = T | Promise<T>;

  /** An issue's severity: any other value counts as an error. */
  type Severity = "error" | "warning" | "info";

  /** An issue a plugin's Validate reports; a missing field is empty. */
  interface Issue {
    severity?: Severity;
    code?: string;
    message?: string;
    field?: string;
  }

  type HeadersInit = Headers | Iterable<readonly [string, string]> | Record<string, string>;
  type BodyInit = string | ArrayBuffer | ArrayBufferView;

  interface RequestInit {
    method?: string;
    headers?: HeadersInit;
    body?: BodyInit | null;
    /** "follow" (the default) follows redirects, "manual" returns them, "error" rejects. */
    redirect?: "follow" | "manual" | "error";
  }

  interface ResponseInit {
    status?: number;
    statusText?: string;
    headers?: HeadersInit;
    url?: string;
    redirected?: boolean;
  }

  /** Writes to the service's log: log, info and debug as the plugin's stdout, trace, warn and error as stderr. */
  interface Console {
    log(...data: unknown[]): void;
    info(...data: unknown[]): void;
    debug(...data: unknown[]): void;
    trace(...data: unknown[]): void;
    warn(...data: unknown[]): void;
    error(...data: unknown[]): void;
  }

  /** The secrets plugin.yaml declares (secrets: [...]), which the service resolves. */
  interface Secrets {
    /** Returns the value of the secret name; throws an Error when plugin.yaml does not declare it or it cannot be
     * resolved. */
    get(name: string): string;
  }
}

declare class Headers implements Iterable<[string, string]> {
  constructor(init?: plg.HeadersInit | null);
  append(name: string, value: string): void;
  set(name: string, value: string): void;
  get(name: string): string | null;
  has(name: string): boolean;
  delete(name: string): void;
  forEach(callback: (value: string, name: string, headers: Headers) => void, thisArg?: unknown): void;
  entries(): IterableIterator<[string, string]>;
  keys(): IterableIterator<string>;
  values(): IterableIterator<string>;
  [Symbol.iterator](): IterableIterator<[string, string]>;
}

declare class Response {
  constructor(body?: plg.BodyInit | null, init?: plg.ResponseInit);
  readonly status: number;
  readonly statusText: string;
  readonly headers: Headers;
  readonly url: string;
  readonly redirected: boolean;
  readonly ok: boolean;
  readonly bodyUsed: boolean;
  bytes(): Promise<Uint8Array>;
  arrayBuffer(): Promise<ArrayBuffer>;
  text(): Promise<string>;
  json(): Promise<any>;
}

/** Makes an HTTP request through the service, under its HTTP policy; a refused or failed request rejects. */
declare function fetch(input: string | { url: string }, init?: plg.RequestInit): Promise<Response>;

declare const console: plg.Console;

declare const secrets: plg.Secrets;

/** A text file the plugin imports: its contents. */
declare module "*.txt" {
  const text: string;
  export default text;
}
`

// typeScript renders plg.d.ts.
func (m *model) typeScript() []byte {
	var out strings.Builder

	out.WriteString(m.header("//",
		"TypeScript declarations for plugins of this interface, for editors and plg validate's type-check:",
		"Create returns a "+m.ref(m.iface.obj)+", whose methods may return their result or a Promise of it;",
		"struct fields have their json names, and nil-able values (slices, maps, pointers, []byte) may be null.",
		"Regenerate them with `plg types` when plugin.yaml's implements changes."))
	out.WriteString("\n")
	out.WriteString(tsGlobals)

	for _, space := range m.namespaces() {
		fmt.Fprintf(&out, "\ndeclare namespace %s {\n", space)

		for i, decl := range m.declsIn(space) {
			if i > 0 {
				out.WriteString("\n")
			}

			m.tsDecl(&out, decl)
		}

		out.WriteString("}\n")
	}

	if m.host != nil {
		out.WriteString(
			"\n/** The service's Host (" + m.ref(m.host.obj) + "): every method returns a Promise, rejected " +
				"with the Host's error. */\n",
		)
		fmt.Fprintf(&out, "declare const host: %s;\n", m.ref(m.host.obj))
	}

	return []byte(out.String())
}

// namespaces returns the namespaces the declarations use, sorted.
func (m *model) namespaces() []string {
	spaces := make([]string, 0, len(m.spaces))
	for _, space := range m.spaces {
		spaces = append(spaces, space)
	}

	slices.Sort(spaces)

	return spaces
}

// declsIn returns the declarations of a namespace, in name order: the interfaces and the named types.
func (m *model) declsIn(space string) []*types.TypeName {
	var objs []*types.TypeName

	for _, iface := range []*ifaceModel{m.iface, m.host} {
		if iface != nil && m.spaces[iface.obj.Pkg().Path()] == space {
			objs = append(objs, iface.obj)
		}
	}

	for _, obj := range m.decls {
		if m.spaces[obj.Pkg().Path()] == space {
			objs = append(objs, obj)
		}
	}

	slices.SortFunc(objs, func(a, b *types.TypeName) int {
		return strings.Compare(typeName(a.Name()), typeName(b.Name()))
	})

	return objs
}

// tsDecl declares obj in its namespace.
func (m *model) tsDecl(out *strings.Builder, obj *types.TypeName) {
	name := typeName(obj.Name())

	switch {
	case obj == m.iface.obj:
		fmt.Fprintf(out, "  /** The interface plugins implement: what Create returns (%s). */\n", goName(obj))
		fmt.Fprintf(out, "  interface %s {\n", name)
		m.tsMethods(out, m.iface, pluginMethods)
		m.tsExtras(out)
		out.WriteString("  }\n")
	case m.host != nil && obj == m.host.obj:
		fmt.Fprintf(out, "  /** The service's Host (%s), the host global. */\n", goName(obj))
		fmt.Fprintf(out, "  interface %s {\n", name)
		m.tsMethods(out, m.host, hostMethods)
		out.WriteString("  }\n")
	default:
		n := m.nodes[obj]
		fmt.Fprintf(out, "  /** %s */\n", goName(obj))

		if n.Kind != layout.Struct {
			fmt.Fprintf(out, "  type %s = %s;\n", name, m.tsShape(n))

			return
		}

		fmt.Fprintf(out, "  interface %s {\n", name)

		for _, field := range n.Fields {
			fmt.Fprintf(out, "    %s: %s;\n", tsProperty(field.Name), m.ts(field.Node))
		}

		out.WriteString("  }\n")
	}
}

// goName names a Go type by its package path and name.
func goName(obj *types.TypeName) string {
	return obj.Pkg().Path() + "." + obj.Name()
}

// side says whose methods an interface's are.
type side int

const (
	// pluginMethods are the implemented interface's: plg hands their parameters over and takes their results
	// back, which the plugin may return as a Promise.
	pluginMethods side = iota
	// hostMethods are the Host's: the plugin hands their parameters to plg and gets a Promise of their results.
	hostMethods
)

// flow is which way a value crosses.
type flow int

const (
	handedOver flow = iota // from plg to the plugin, as plg decodes it
	handedBack             // from the plugin to plg, where a Date also stands for a time
)

// tsMethods declares an interface's methods.
func (m *model) tsMethods(out *strings.Builder, iface *ifaceModel, whose side) {
	params, results := handedOver, handedBack
	if whose == hostMethods {
		params, results = handedBack, handedOver
	}

	for _, meth := range iface.methods {
		names := paramNames(meth.params)
		declared := make([]string, len(meth.params))

		for i, p := range meth.params {
			declared[i] = names[i] + ": " + m.tsValue(p.node, params)
		}

		values := make([]string, len(meth.results))
		for i, r := range meth.results {
			values[i] = m.tsValue(r, results)
		}

		var result string

		switch len(values) {
		case 0:
			result = "void"
		case 1:
			result = values[0]
		default:
			result = "[" + strings.Join(values, ", ") + "]"
		}

		if whose == hostMethods {
			result = "Promise<" + result + ">"
		} else {
			result = "plg.Result<" + result + ">"
		}

		fmt.Fprintf(out, "    %s(%s): %s;\n", meth.name, strings.Join(declared, ", "), result)
	}
}

// tsExtras declares the implemented interface's optional methods plg looks up by name: Validate (with a
// validation input) and Close, unless the interface has a method of that name.
func (m *model) tsExtras(out *strings.Builder) {
	if m.validation != nil && !m.iface.has("Validate") {
		out.WriteString("    /** Validates the plugin against the service's input; plg calls it before the plugin " +
			"serves. */\n")
		fmt.Fprintf(out, "    Validate?(input: %s): plg.Result<plg.Issue[] | null | undefined>;\n",
			m.ts(m.validation))
	}

	if !m.iface.has("Close") {
		out.WriteString("    /** Called when plg retires the instance. */\n")
		out.WriteString("    Close?(): plg.Result<void>;\n")
	}
}

func (iface *ifaceModel) has(name string) bool {
	return slices.ContainsFunc(iface.methods, func(meth method) bool { return meth.name == name })
}

// tsValue returns the type of a value crossing the given way.
func (m *model) tsValue(n *node, way flow) string {
	if way == handedBack && m.reachesTime(n) {
		return "plg.Out<" + m.ts(n) + ">"
	}

	return m.ts(n)
}

// ts returns the type of n: its name when it is declared, else its shape.
func (m *model) ts(n *node) string {
	if obj := declared(n); obj != nil {
		return m.ref(obj)
	}

	return m.tsShape(n)
}

// tsNumber is TypeScript's type of every integer and float kind.
const tsNumber = "number"

// tsScalars are the TypeScript types of the scalar shapes; TestScalarTablesSpellEveryScalarShape holds it to
// every one.
//
//nolint:gochecknoglobals // immutable lookup table
var tsScalars = map[shape]string{
	shapeBool: "boolean", shapeInteger: tsNumber, shapeFloat: tsNumber, shapeString: "string",
	shapeBytes: "Uint8Array | null", shapeTime: "plg.Time",
}

// tsShape returns the type of n's wire kind, naming the types it holds.
func (m *model) tsShape(n *node) string {
	switch shp := shapeOf(n.Kind); shp {
	case shapeBool, shapeInteger, shapeFloat, shapeString, shapeBytes, shapeTime:
		return tsScalars[shp]
	case shapeSlice:
		return tsArray(m.ts(n.Elem)) + " | null"
	case shapeMap:
		return "{ [key: string]: " + m.ts(n.Elem) + " } | null"
	case shapePointer:
		return tsNullable(m.ts(n.Elem))
	case shapeStruct:
		return m.tsStruct(n)
	default:
		panic(noShape(shp))
	}
}

// tsStruct returns the object type of the struct n.
func (m *model) tsStruct(n *node) string {
	if len(n.Fields) == 0 {
		return "{}"
	}

	fields := make([]string, len(n.Fields))
	for i, field := range n.Fields {
		fields[i] = tsProperty(field.Name) + ": " + m.ts(field.Node)
	}

	return "{ " + strings.Join(fields, "; ") + " }"
}

// tsArray returns the array type of elements of type elem, whose only unions are "X | null".
func tsArray(elem string) string {
	if strings.HasSuffix(elem, " | null") {
		return "(" + elem + ")[]"
	}

	return elem + "[]"
}

// tsNullable returns typ or null.
func tsNullable(typ string) string {
	if strings.HasSuffix(typ, " | null") {
		return typ
	}

	return typ + " | null"
}

var identifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// tsProperty returns a property name: as it is when it is an identifier, else quoted.
func tsProperty(name string) string {
	if identifier.MatchString(name) {
		return name
	}

	return quote(name)
}

// quote returns name as a string literal TypeScript and Lua (5.3 and later) both read: printable ASCII as it is
// (quotes and backslashes escaped), every other code point as a \u{...} escape.
func quote(name string) string {
	var out strings.Builder

	out.WriteByte('"')

	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case r >= ' ' && r < utf8.RuneSelf && r != 0x7f:
			out.WriteRune(r)
		default:
			out.WriteString(`\u{` + strconv.FormatInt(int64(r), 16) + `}`)
		}
	}

	out.WriteByte('"')

	return out.String()
}
