//go:build integration

package plg_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// evalPlugin returns the Lua eval test plugin (testdata/plugins/lua/eval): Foo(code) runs code and returns its
// result as a string, Fail(code) runs code and fails with its error.
//
//nolint:ireturn // the plugin is served as the contract interface under test
func evalPlugin(t *testing.T, opts ...plg.RegisterOption) testcontract.Fooer {
	t.Helper()

	return getFooer(t, fooerContainerIn(t, luaLanguage(), map[string]string{"eval": "eval"}, opts...), "fooer/eval")
}

// evalCase is Lua code for the eval plugin and what it returns, or the error it fails with.
type evalCase struct {
	name, code, want, err string
}

func runEvalCases(t *testing.T, eval testcontract.Fooer, tests []evalCase) {
	t.Helper()

	for _, tc := range tests {
		got, err := eval.Foo(t.Context(), tc.code)
		if tc.err != "" {
			require.ErrorContains(t, err, tc.err, tc.name)

			var pluginErr *plg.PluginError

			require.ErrorAs(t, err, &pluginErr, "%s: an ordinary error", tc.name)

			continue
		}

		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
}

func TestLuaRequireStaysInsideThePlugin(t *testing.T) {
	t.Parallel()

	runEvalCases(t, evalPlugin(t), []evalCase{
		{name: "a package's init.lua and its submodule", code: `local p = require("pkg")
			return p.name .. " " .. p.leaf .. " " .. p.args[1] .. " " .. p.args[2]`, want: "pkg leaf pkg pkg/init.lua"},
		{
			name: "loaded once",
			code: `local a = require("pkg.sub.leaf"); local b = require("pkg.sub.leaf"); require("pkg")
			return LEAF_LOADS`,
			want: "1",
		},
		{name: "the same value every time", code: `return require("pkg") == require("pkg")`, want: "true"},
		{name: "the bundled json module", code: `return require("json") == json`, want: "true"},
		{name: "standard libraries", code: `return require("string") == string`, want: "true"},
		{
			name: "missing", code: `return require("nothing")`,
			err: "module 'nothing' not found in the plugin (no nothing.lua or nothing/init.lua)",
		},
		{name: "only .lua files", code: `return require("notes")`, err: "module 'notes' not found"},
		{name: "parent directory", code: `return require("..eval.plugin")`, err: "invalid module name '..eval.plugin'"},
		{name: "empty component", code: `return require("pkg..sub")`, err: "invalid module name 'pkg..sub'"},
		{name: "absolute path", code: `return require("/etc/passwd")`, err: "invalid module name '/etc/passwd'"},
		{name: "path separators", code: `return require("pkg/init")`, err: "invalid module name 'pkg/init'"},
		{name: "empty name", code: `return require("")`, err: "invalid module name ''"},
		{name: "not a string", code: `return require({})`, err: "bad argument #1 to 'require'"},
		{name: "cycle", code: `return require("cycle.a")`, err: "module 'cycle.a' requires itself"},
		{name: "a module that fails", code: `return require("raises")`, err: "raises failed to load"},
		{
			name: "a module that fails, again", code: `local ok = pcall(require, "raises")
			local ok2, err = pcall(require, "raises"); return tostring(ok) .. tostring(ok2) .. err`,
			want: "falsefalseraises failed to load",
		},
		{name: "syntax error", code: `return require("syntax")`, err: "syntax.lua:1: unexpected symbol near '='"},
	})
}

func TestLuaSandbox(t *testing.T) {
	t.Parallel()

	runEvalCases(t, evalPlugin(t), []evalCase{
		{name: "no file loading", code: `return tostring(dofile) .. tostring(loadfile)`, want: "nilnil"},
		{
			name: "no io, os, package or debug",
			code: `return tostring(io) .. tostring(os) .. tostring(package) .. tostring(debug)`, want: "nilnilnilnil",
		},
		{name: "no bytecode dumps", code: `return tostring(string.dump)`, want: "nil"},
		{
			name: "libraries", code: `return type(string.format) .. type(table.concat) .. type(math.floor) ..
			type(utf8.char) .. type(coroutine.wrap) .. type(json.encode)`,
			want: "functionfunctionfunctionfunctionfunctionfunction",
		},
		{name: "load compiles text", code: `return load("return 1 + 1")()`, want: "2"},
		{name: "load with an environment", code: `return load("return x", "c", "t", {x = "env"})()`, want: "env"},
		{
			name: "load refuses binary chunks",
			code: `local f, err = load("\27Lua\84\0"); return tostring(f) .. " " .. err`,
			want: "nil attempt to load a binary chunk (mode is 't')",
		},
		{
			name: "load refuses readers", code: `return load(function() return nil end)`,
			err: "bad argument #1 to 'load' (string expected, got function)",
		},
		{name: "coroutines", code: `local co = coroutine.wrap(function(a) local b = coroutine.yield(a + 1)
			return b * 2 end); return co(1) + co(10)`, want: "22"},
		{name: "no Host without WithHost", code: `return type(host)`, want: "nil"},
		{name: "http without WithHTTP", code: `local r, err = http.request{url = "http://example.com/"}
			return tostring(r) .. " " .. err`, want: "nil plg http: HTTP is not enabled for this plugin (register it " +
			"with plg.WithHTTP)"},
		{
			name: "http checks its argument", code: `return http.request{url = 1}`,
			err: "http.request: url: want a string, got number",
		},
	})
}

func TestLuaJSON(t *testing.T) {
	t.Parallel()

	runEvalCases(t, evalPlugin(t), []evalCase{
		{
			name: "nested round trip", code: `
			local text = '{"a":[1,2.5,{"b":null,"c":[]}],"d":{},"e":"x","f":true,"g":false}'
			return json.encode(json.decode(text))`,
			want: `{"a":[1,2.5,{"b":null,"c":[]}],"d":{},"e":"x","f":true,"g":false}`,
		},
		{
			name: "objects have sorted keys", code: `return json.encode({zeta = 1, alpha = 2, mid = {b = 1, a = 2}})`,
			want: `{"alpha":2,"mid":{"a":2,"b":1},"zeta":1}`,
		},
		{name: "sequences are arrays", code: `return json.encode({1, "two", {3}})`, want: `[1,"two",[3]]`},
		{name: "an empty table is an object", code: `return json.encode({})`, want: `{}`},
		{name: "marked empty array", code: `return json.encode(json.array())`, want: `[]`},
		{
			name: "marked object with integer-like keys", code: `return json.encode(json.object({["1"] = true}))`,
			want: `{"1":true}`,
		},
		{name: "decoded empty array stays an array", code: `return json.encode(json.decode("[]"))`, want: `[]`},
		{
			name: "decoded arrays index from 1", code: `local a = json.decode("[10,20]"); return a[1] + a[2] + #a`,
			want: "32",
		},
		{
			name: "null", code: `local a = json.decode("[null]"); return tostring(a[1] == json.null) .. #a`,
			want: "true1",
		},
		{name: "unicode escapes", code: `return json.decode('"\\u00e9\\u20ac\\ud83d\\ude00"')`, want: "é€😀"},
		{name: "raw UTF-8 passes through", code: `return json.encode("é€😀")`, want: `"é€😀"`},
		{
			name: "control characters are escaped", code: `return json.encode("a\"b\\c\n\t\1")`,
			want: `"a\"b\\c\n\t\u0001"`,
		},
		{name: "lone surrogate", code: `return json.decode('"\\ud800x"')`, want: "\uFFFDx"},
		{name: "integers stay integers", code: `local n = json.decode("9007199254740993")
			return math.type(n) .. " " .. n`, want: "integer 9007199254740993"},
		{name: "fractions and exponents are floats", code: `local a = json.decode("[1.5, 1e3, -0.0]")
			return math.type(a[1]) .. math.type(a[2]) .. " " .. json.encode(a)`, want: `floatfloat [1.5,1000,-0]`},
		{
			name: "floats print exactly and shortest", code: `return json.encode({0.1, 1/3, 2^60, 1e300})`,
			want: `[0.1,0.3333333333333333,1.152921504606847e+18,1e+300]`,
		},
		{name: "large integers", code: `return json.encode(math.mininteger)`, want: `-9223372036854775808`},
		{
			name: "integer overflow decodes as a float", code: `return math.type(json.decode("99999999999999999999"))`,
			want: "float",
		},
		{
			name: "whitespace", code: `return json.encode(json.decode(" \t\n[ 1 , { \"a\" : 2 } ] \r\n"))`,
			want: `[1,{"a":2}]`,
		},
		{
			name: "malformed: trailing comma", code: `return json.decode("[1,]")`,
			err: "json.decode: unexpected character at byte 4",
		},
		{
			name: "malformed: unterminated", code: `return json.decode('{"a": "b')`,
			err: "json.decode: unterminated string at byte 9",
		},
		{
			name: "malformed: trailing characters", code: `return json.decode("1 2")`,
			err: "json.decode: trailing characters at byte 3",
		},
		{name: "malformed: bare word", code: `return json.decode("nul")`, err: "json.decode: invalid literal"},
		{name: "malformed: leading zero", code: `return json.decode("01")`, err: "json.decode: trailing characters"},
		{name: "malformed: empty", code: `return json.decode("")`, err: "json.decode: unexpected end of input"},
		{name: "malformed: single quotes", code: `return json.decode("'a'")`, err: "json.decode: unexpected character"},
		{
			name: "malformed: control character", code: `return json.decode('"a\nb"')`,
			err: "json.decode: control character in string",
		},
		{name: "malformed: invalid UTF-8", code: `return json.decode('"\255"')`, err: "json.decode: invalid UTF-8"},
		{name: "malformed: bad escape", code: `return json.decode('"\\x"')`, err: "json.decode: invalid escape"},
		{name: "malformed: non-string key", code: `return json.decode('{1: 2}')`, err: "expected a string key"},
		{name: "malformed errors are catchable", code: `local ok, err = pcall(json.decode, "{")
			return tostring(ok) .. ": " .. err`, want: "false: json.decode: expected a string key at byte 2"},
		{
			name: "too deep", code: `return json.decode(string.rep("[", 1001) .. string.rep("]", 1001))`,
			err: "json.decode: nested too deeply",
		},
		{
			name: "deep enough",
			code: `return #json.encode(json.decode(string.rep("[", 1000) .. string.rep("]", 1000)))`,
			want: "2000",
		},
		{
			name: "cycles", code: `local t = {}; t.self = t; return json.encode(t)`,
			err: "json.encode: nested too deeply (a cycle?)",
		},
		{name: "functions", code: `return json.encode({f = print})`, err: "json.encode: cannot encode a function"},
		{name: "NaN", code: `return json.encode(0/0)`, err: "json.encode: cannot encode NaN"},
		{name: "infinity", code: `return json.encode(math.huge)`, err: "json.encode: cannot encode infinity"},
		{name: "invalid UTF-8", code: `return json.encode("\255")`, err: "json.encode: invalid UTF-8 in string"},
		{name: "mixed keys", code: `return json.encode({1, a = 2})`, err: "json.encode: object keys must be strings"},
	})
}

func TestHostileLuaPluginsCannotTakeTheHostDown(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer",
		plg.WithStorage(luaCorpus("hostile")), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)))

	hostile := getFooer(t, ctr, "fooer/hostile")
	served := instanceID(t, ctr, "fooer/hostile")

	// Ordinary Lua errors, which the plugin could have caught: the instance keeps serving.
	for _, tc := range []struct{ msg, want string }{
		{msg: "error", want: "plugin.lua:35: hostile error"},
		{msg: "oomcaught", want: "caught: false not enough memory"},
		{msg: "recurse", want: "stack overflow"},
		{msg: "cstack", want: "stack overflow"},
		{msg: "print", want: "after printing"},
	} {
		err := hostile.Fail(t.Context(), tc.msg)

		var pluginErr *plg.PluginError

		require.ErrorAs(t, err, &pluginErr, tc.msg)
		assert.Contains(t, pluginErr.Message, tc.want, tc.msg)
		assert.Equal(t, served, instanceID(t, ctr, "fooer/hostile"), "%s: an ordinary error keeps the instance", tc.msg)
	}

	err := hostile.Fail(t.Context(), "hugeerror")

	var pluginErr *plg.PluginError

	require.ErrorAs(t, err, &pluginErr)
	assert.Equal(t, strings.Repeat("e", 64<<10), pluginErr.Message, "an oversized message is cut to 64 KiB")

	assert.Contains(t, logs.String(), `"level":"INFO","msg":"plg: guest output","key":"fooer/hostile",`+
		`"stream":"stdout","line":"hostile says hello on stdout"`, "print goes to the log")

	// Failures that discard the instance.
	for _, tc := range []struct {
		call     func(ctx context.Context) error
		name     string
		guest    string
		deadline bool
	}{
		{
			name: "oom", call: func(ctx context.Context) error { return hostile.Fail(ctx, "oom") },
			guest: "lua: not enough memory",
		},
		{
			name: "huge result", call: func(ctx context.Context) error { return errOf(hostile.Foo(ctx, "huge")) },
			guest: "lua: not enough memory",
		},
		{name: "loop", call: func(ctx context.Context) error { return hostile.Fail(ctx, "loop") }, deadline: true},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		if tc.deadline {
			ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
		}

		served := instanceID(t, ctr, "fooer/hostile")
		err := tc.call(ctx)

		cancel()

		failure := requirePanic(t, err, "fooer/hostile", plg.PhaseCall)
		assert.Equal(t, plg.LanguageLua, failure.Language, tc.name)
		require.ErrorIs(t, err, plg.ErrCall, tc.name)
		assert.Contains(t, failure.Guest, tc.guest, tc.name)
		assert.Equal(t, tc.deadline, errors.Is(err, context.DeadlineExceeded), tc.name)
		assert.Same(t, failure, ctr.RecentPanics()[len(ctr.RecentPanics())-1], "%s: kept in the history", tc.name)
		assert.NotEqual(t, served, instanceID(t, ctr, "fooer/hostile"), "%s: the instance is replaced", tc.name)

		got, err := hostile.Foo(t.Context(), tc.name)
		require.NoError(t, err, "%s: the host and the plugin survive", tc.name)
		assert.Equal(t, "hello, "+tc.name, got)
	}

	assert.Len(t, ctr.RecentPanics(), 3, "one failure per discarding case, nothing else")
}

// luaCorpus is a filesystem storage over the Lua hostile corpus (testdata/hostile/lua), listing only the given
// plugin directories.
func luaCorpus(keys ...string) *plg.FSStorage {
	return plg.NewFSStorageFromFS("hostile", only{os.DirFS(filepath.Join("testdata", "hostile", "lua")), keys})
}

func TestLuaPluginsThatFailToLoadFailRegister(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)

	for _, tc := range []struct {
		key, want string
	}{
		{key: "createerror", want: "plugin Create failed: no configuration"},
		{key: "createnil", want: "plugin Create failed: not today"},
		{key: "nocreate", want: "plugin source failed to load: the plugin defines no global function Create"},
		{key: "notatable", want: "plugin source failed to load: Create returned number, want a table"},
		{key: "missingmethod", want: "plugin source failed to load: the table Create returned has no method Boil"},
		{key: "syntaxerror", want: "plugin source failed to load: plugin.lua:3: unexpected symbol near 'end'"},
		{key: "chunkerror", want: "plugin source failed to load: plugin.lua:3: attempt to index a nil value " +
			"(local 'config')"},
		{key: "requireescape", want: "plugin source failed to load: plugin.lua:2: invalid module name '..secret'"},
	} {
		err := plg.Register[testcontract.Fooer](t.Context(), ctr, tc.key, plg.WithStorage(luaCorpus(tc.key)))
		require.ErrorIs(t, err, plg.ErrLoad, tc.key)
		require.ErrorContains(t, err, tc.key+"/"+tc.key, tc.key)
		require.ErrorContains(t, err, tc.want, tc.key)

		_, err = plg.Get[testcontract.Fooer](t.Context(), ctr, tc.key+"/"+tc.key)
		require.ErrorIs(t, err, plg.ErrNotFound, "%s: nothing is published", tc.key)
	}

	assert.Empty(t, ctr.RecentPanics(), "a plugin that fails to load is an ordinary error")

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "createoom", plg.WithStorage(luaCorpus("createoom")),
		plg.WithMemoryLimit(hostileLimit))
	require.ErrorIs(t, err, plg.ErrLoad)

	failure := requirePanic(t, err, "createoom/createoom", plg.PhaseCreate)
	assert.Equal(t, plg.LanguageLua, failure.Language)
	assert.Contains(t, failure.Guest, "lua: not enough memory")
}

func TestLuaEntrypointMustBeALuaFile(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	files := fstest.MapFS{"plugin.lua": {Data: []byte("function Create() return {} end")}, "lib.lua/x.lua": {}}

	for name, tc := range map[string]struct{ entry, want string }{
		"missing":      {entry: "main.lua", want: "entrypoint: stat main.lua: file does not exist"},
		"not lua":      {entry: "plugin.go", want: `a Lua entrypoint is a .lua file: "plugin.go"`},
		"a directory":  {entry: "lib.lua", want: `a Lua entrypoint is a .lua file: "lib.lua"`},
		"invalid path": {entry: "../plugin.lua", want: `a Lua entrypoint is a .lua file: "../plugin.lua"`},
	} {
		store := plg.NewMemoryStorage(name)
		require.NoError(t, store.Put(&plg.Descriptor{
			Key: "k", Implements: implementsOf[testcontract.Fooer](), Language: plg.LanguageLua, Entrypoint: tc.entry,
		}, files))

		err := plg.Register[testcontract.Fooer](t.Context(), ctr, strings.ReplaceAll(name, " ", "-"),
			plg.WithStorage(store))
		require.ErrorIs(t, err, plg.ErrLoad, name)
		require.ErrorContains(t, err, tc.want, name)
	}
}

func TestLuaHostedPluginNeedsWithHost(t *testing.T) {
	t.Parallel()

	err := plg.Register[testcontract.Hosted](t.Context(), newContainer(t), "nohost",
		plg.WithStorage(luaLanguage().storage(t, "nohost", map[string]string{"ref": "hosted"})))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, "plugin Create failed: plugin.lua:")
	require.ErrorContains(t, err, "attempt to index a nil value (global 'host')", "no host global without WithHost")
}

// BenchmarkCallLua is one call into a Lua plugin (Foo: a require'd function, string building), with every
// protection on.
func BenchmarkCallLua(b *testing.B) {
	benchCall(b, luaLanguage())
}

// BenchmarkCallLuaParallel calls a Lua plugin from GOMAXPROCS goroutines: with a pool of one instance the calls
// contend for it, with one instance per goroutine they run in parallel.
func BenchmarkCallLuaParallel(b *testing.B) {
	benchCallParallel(b, luaLanguage())
}

// BenchmarkCallLuaStruct is one call into a Lua plugin with SampleOrder as argument and result: the glue builds
// the nested tables from the wire encoding and encodes them back.
func BenchmarkCallLuaStruct(b *testing.B) {
	benchCallStruct(b, luaLanguage())
}

// BenchmarkCallLuaHost is one Lua plugin call making one host call (Value).
func BenchmarkCallLuaHost(b *testing.B) {
	benchHostValue(b, luaLanguage())
}

// BenchmarkCallLuaHostStruct is one Lua plugin call making one host call returning a struct (Describe).
func BenchmarkCallLuaHostStruct(b *testing.B) {
	benchHostStruct(b, luaLanguage())
}

// BenchmarkHTTPRoundTripLua is one Lua plugin call making one http.request through the host.
func BenchmarkHTTPRoundTripLua(b *testing.B) {
	benchHTTP(b, luaLanguage())
}

func BenchmarkHTTPRoundTripLuaParallel(b *testing.B) {
	benchHTTPParallel(b, luaLanguage())
}

// nestedHostCalls is a Hosted Lua plugin whose Relay(scenario [url]) runs one operation 300 times while __gc
// finalizers, kept pending and run by an incremental collector stepping on every allocation, make other host
// calls, http requests or json encodings meanwhile. It returns "nested=<finalizer runs inside the operation>
// bad=<runs whose result differed from the one made with the collector stopped> first=<the first bad result>".
//
//nolint:dupword // Lua source, where nested blocks close with "end end"
const nestedHostCalls = `
local P = {}
P.__index = P

local inside, nested, armed, op, url = false, 0, false, nil, nil
local mt = {}
mt.__gc = function()
  if not armed then return end
  if inside then nested = nested + 1 end
  if op == "json" then
    json.encode({ nested = { 1, 2, 3 } })
  elseif op == "http" then
    http.request({ url = url })
    host.Value()
  else
    host.Sum(7, { 1, 2 })
    host.Value()
  end
  setmetatable({}, mt)
end

local long = {}
for i = 1, 100 do long[string.rep("k", 48) .. i] = { i, string.rep("v", i) } end

local function same(a, b)
  if type(a) ~= "table" or type(b) ~= "table" then return a == b end
  for k, v in pairs(a) do
    if not same(v, b[k]) then return false end
  end
  for k in pairs(b) do
    if a[k] == nil then return false end
  end
  return true
end

local ops = {
  host = function() return host.Lookup("ada") end,
  http = function()
    local body
    -- gsub calls back from C: the request cannot yield, so it is a blocking round trip.
    string.gsub("x", "x", function() body = http.request({ url = url }).body end)
    return { body = body, user = host.Lookup("ada") }
  end,
  json = function() return json.encode(long) end,
}

function P:Relay(cmd)
  op, url = cmd:match("^(%S+) ?(.*)$")
  local run = ops[op]
  collectgarbage("stop")
  local want = run()
  for _ = 1, 20 do setmetatable({}, mt) end
  armed, nested = true, 0
  collectgarbage("restart")
  collectgarbage("incremental", 0, 1000, 0)
  local bad, first = 0, nil
  for _ = 1, 300 do
    inside = true
    local ok, got = pcall(run)
    inside = false
    if not ok or not same(got, want) then
      bad = bad + 1
      first = first or tostring(got)
    end
  end
  armed = false
  collectgarbage("incremental", 200, 100, 13)
  collectgarbage()
  return string.format("nested=%d bad=%d first=%s", nested, bad, tostring(first))
end

function P:Describe() return {} end
function P:Value() return "" end
function P:Origin() return "" end
function P:Total(base) return base end
function P:Keys(keys) return keys end

function Create() return setmetatable({}, P) end
`

// TestLuaNestedHostCallsKeepTheirOwnAnswers: a Host call, http request or json encoding made by a finalizer while
// another one encodes its request or decodes its answer cannot overwrite that one's buffers: the outer operation
// gets exactly its own result.
func TestLuaNestedHostCallsKeepTheirOwnAnswers(t *testing.T) {
	t.Parallel()

	api := newAPIServer(t)
	store := plg.NewMemoryStorage("mem")
	require.NoError(t, store.Put(&plg.Descriptor{
		Key: "p", Implements: implementsOf[testcontract.Hosted](), Language: plg.LanguageLua, Entrypoint: "plugin.lua",
	},
		fstest.MapFS{"plugin.lua": {Data: []byte(nestedHostCalls)}}))

	policy := openPolicy(hostOf(t, api.URL))
	policy.MaxRequestsPerCall = 1 << 16 // the finalizers' requests count too

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Hosted](t.Context(), ctr, "nested", plg.WithStorage(store),
		plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit), plg.WithHost[testcontract.Host](newTestHost()),
		plg.WithHTTP(policy)))

	hosted, err := plg.Get[testcontract.Hosted](t.Context(), ctr, "nested/p")
	require.NoError(t, err)

	for _, scenario := range []string{"host", "http " + api.URL + "/v1", "json"} {
		got, relayErr := hosted.Relay(t.Context(), scenario)
		require.NoError(t, relayErr, scenario)

		var nested, bad int

		_, scanErr := fmt.Sscanf(got, "nested=%d bad=%d", &nested, &bad)
		require.NoError(t, scanErr, got)
		assert.Positive(t, nested, "%s: finalizers ran inside the operation: %s", scenario, got)
		assert.Zero(t, bad, "%s: every run got its own result: %s", scenario, got)
	}
}
