//go:build integration

package plg_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/heynemann/go-plg"
	_ "github.com/heynemann/go-plg/guest/typescript"
	"github.com/heynemann/go-plg/internal/httpcap"
	"github.com/heynemann/go-plg/internal/testcontract"
)

// tsCorpus is a filesystem storage over the TypeScript hostile corpus (testdata/hostile/typescript), listing only
// the given plugin directories.
func tsCorpus(keys ...string) *plg.FSStorage {
	return plg.NewFSStorageFromFS("hostile", only{os.DirFS(filepath.Join("testdata", "hostile", "typescript")), keys})
}

// tsSource is a memory storage holding one TypeScript plugin, key "p", made of files.
func tsSource(tb testing.TB, files fstest.MapFS) *plg.MemoryStorage {
	tb.Helper()

	store := plg.NewMemoryStorage("mem")
	require.NoError(tb, store.Put(&plg.Descriptor{Key: "p", Language: plg.LanguageTypeScript, Entrypoint: "plugin.ts"},
		files))

	return store
}

// tsPlugin is the source of a one-file TypeScript plugin whose Create returns the object literal with the given
// members.
func tsPlugin(members string) fstest.MapFS {
	return fstest.MapFS{"plugin.ts": {Data: []byte("export function Create() { return {\n" + members + "\n}; }\n")}}
}

// fooerMembers completes a Fooer object literal: each member not in members is a harmless stub.
func fooerMembers(members string) string {
	for _, stub := range []string{
		"Foo: (n: string) => n", "Count: () => 0", "Fail: () => {}", "Boil: () => [false, 0]",
		"Scalars: (...a: unknown[]) => a", "Spin: (n: number) => n",
	} {
		name, _, _ := strings.Cut(stub, ":")
		if !strings.Contains(members, name+"(") && !strings.Contains(members, name+":") {
			members += ",\n" + stub
		}
	}

	return strings.TrimPrefix(members, ",\n")
}

// tsFooer registers a TypeScript Fooer whose Create returns members (completed by stubs) as "ts/p".
//
//nolint:ireturn // the plugin is served as the contract interface under test
func tsFooer(t *testing.T, ctr *plg.Container, members string, opts ...plg.RegisterOption) testcontract.Fooer {
	t.Helper()

	opts = append(opts, plg.WithStorage(tsSource(t, tsPlugin(fooerMembers(members)))))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "ts", opts...))

	return getFooer(t, ctr, "ts/p")
}

func TestHostileTypeScriptPluginsCannotTakeTheHostDown(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "fooer", plg.WithStorage(tsCorpus("hostile")),
		plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)))

	hostile := getFooer(t, ctr, "fooer/hostile")

	tests := []hostileCase{
		{name: "throw", msg: "throw", message: "hostile throw"},
		{name: "rejected promise", msg: "reject", message: "hostile rejection"},
		{name: "unbounded recursion", msg: "recurse", message: "Maximum call stack size exceeded"},
		{
			name: "promise that never settles", msg: "never",
			message: "the returned promise never settles: no pending job is left to run",
		},
		{
			name:     "out of memory",
			msg:      "oom",
			message:  "out of memory",
			guest:    "the plugin ran out of memory",
			discards: true,
		},
		{name: "infinite loop", msg: "loop", deadline: true, discards: true},
		{
			name: "result larger than the instance's memory", message: "out of memory", discards: true,
			guest: "the plugin ran out of memory",
		},
	}

	panics := 0

	for _, tc := range tests {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		if tc.deadline {
			ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
		}

		before, err := hostile.Count(t.Context())
		require.NoError(t, err)

		served := instanceID(t, ctr, "fooer/hostile")

		if tc.msg == "" {
			_, err = hostile.Foo(ctx, "huge")
		} else {
			err = hostile.Fail(ctx, tc.msg)
		}

		cancel()

		if tc.discards {
			panics++

			tc.checkPanic(t, ctr, err)
		} else {
			var pluginErr *plg.PluginError

			require.ErrorAs(t, err, &pluginErr, tc.name)
			assert.Equal(t, tc.message, pluginErr.Message, tc.name)
			require.NotErrorIs(t, err, plg.ErrCall, tc.name)
		}

		after, err := hostile.Count(t.Context())
		require.NoError(t, err, "%s: the host and the plugin survive", tc.name)

		if tc.discards {
			assert.Equal(t, 1, after, "%s: the next call runs on a fresh instance", tc.name)
			assert.NotEqual(t, served, instanceID(t, ctr, "fooer/hostile"), tc.name)
		} else {
			assert.Equal(t, before+1, after, "%s: a thrown error keeps the instance and its state", tc.name)
			assert.Equal(t, served, instanceID(t, ctr, "fooer/hostile"), tc.name)
		}
	}

	assert.Len(t, ctr.RecentPanics(), panics, "one failure per discarding case, nothing else")
	assert.Contains(t, logs.String(), `"level":"ERROR","msg":"plg: plugin failure","key":"fooer/hostile",`+
		`"phase":"call","value":"out of memory","language":"typescript","method":"Fail"`)
}

// hostileCase is a way the hostile TypeScript plugin misbehaves: Fail(msg), or Foo("huge") when msg is empty.
type hostileCase struct {
	name     string
	msg      string
	message  string // a plugin error's message, or a PanicError's Value
	guest    string // in the PanicError's guest output
	discards bool
	deadline bool
}

// checkPanic checks a failure that discards the instance: a PanicError, recorded, with the guest's output.
func (tc *hostileCase) checkPanic(t *testing.T, ctr *plg.Container, err error) {
	t.Helper()

	failure := requirePanic(t, err, "fooer/hostile", plg.PhaseCall)
	assert.Equal(t, plg.LanguageTypeScript, failure.Language, tc.name)
	require.ErrorIs(t, err, plg.ErrCall, tc.name)
	assert.Contains(t, failure.Guest, tc.guest, tc.name)
	assert.Same(t, failure, ctr.RecentPanics()[len(ctr.RecentPanics())-1], "%s: kept in the history", tc.name)

	if tc.deadline {
		require.ErrorIs(t, err, context.DeadlineExceeded, tc.name)
	} else {
		assert.Equal(t, tc.message, failure.Value, tc.name)
	}
}

func TestTypeScriptConsoleIsLoggedWithThePluginKey(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)
	require.NoError(
		t,
		plg.Register[testcontract.Fooer](t.Context(), ctr, "chatty", plg.WithStorage(tsCorpus("hostile")),
			plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)),
	)

	err := getFooer(t, ctr, "chatty/hostile").Fail(t.Context(), "print")
	require.EqualError(t, err, "after printing")

	logged := logs.String()
	assert.Contains(t, logged, `"level":"INFO","msg":"plg: guest output","key":"chatty/hostile","stream":"stdout",`+
		`"line":"hostile says hello on stdout {\"n\":1}"`, "console.log prints objects as JSON")
	assert.Contains(t, logged, `"level":"WARN","msg":"plg: guest output","key":"chatty/hostile","stream":"stderr",`+
		`"line":"and on stderr"`)
}

func TestTypeScriptCreateFailuresFailRegister(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)

	err := plg.Register[testcontract.Fooer](t.Context(), ctr, "oom", plg.WithStorage(tsCorpus("createoom")),
		plg.WithMemoryLimit(hostileLimit))
	require.ErrorIs(t, err, plg.ErrLoad)

	failure := requirePanic(t, err, "oom/createoom", plg.PhaseCreate)
	assert.Equal(t, "out of memory", failure.Value)
	assert.Contains(t, failure.Guest, "the plugin ran out of memory")
	assert.Equal(t, []*plg.PanicError{failure}, ctr.RecentPanics())

	err = plg.Register[testcontract.Fooer](t.Context(), ctr, "throws", plg.WithStorage(tsCorpus("createthrows")))
	require.ErrorIs(t, err, plg.ErrLoad)
	require.ErrorContains(t, err, "throws/createthrows (storage hostile): plugin Create failed: "+
		"cannot start: no configuration", "an async Create's rejection, with the plugin's message")
	assert.Len(t, ctr.RecentPanics(), 1, "a Create that throws is an ordinary error")
}

func TestTypeScriptPluginsThatCannotLoadFailRegister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		files fstest.MapFS
		name  string
		want  string
	}{
		{
			name:  "syntax error",
			files: fstest.MapFS{"plugin.ts": {Data: []byte("export function Create() {\n  return {;\n}\n")}},
			want:  "bundling the TypeScript plugin failed: plugin.ts:2:",
		},
		{
			name:  "package import",
			files: fstest.MapFS{"plugin.ts": {Data: []byte(`import "left-pad";` + "\nexport function Create() {}")}},
			want:  `"left-pad": only relative imports of the plugin's own files are supported`,
		},
		{
			name:  "missing entrypoint",
			files: fstest.MapFS{"main.ts": {Data: []byte("export function Create() {}")}},
			want:  `"plugin.ts": no such file in the plugin`,
		},
		{
			name:  "no Create",
			files: fstest.MapFS{"plugin.ts": {Data: []byte("export function create() { return {}; }")}},
			want:  "plugin source failed to load: the plugin exports no function Create",
		},
		{
			name:  "Create returning no object",
			files: fstest.MapFS{"plugin.ts": {Data: []byte("export function Create() { return 42; }")}},
			want:  "plugin source failed to load: Create returned number, not an object",
		},
		{
			name:  "a method missing",
			files: tsPlugin("Foo() { return ''; }"),
			want:  "plugin source failed to load: the object Create returned has no method Boil",
		},
		{
			name: "top-level code throwing",
			files: fstest.MapFS{
				"plugin.ts": {Data: []byte("throw new Error('bad module');\nexport function Create() {}")},
			},
			want: "plugin source failed to load: bad module",
		},
		{
			name:  "no std or os modules",
			files: fstest.MapFS{"plugin.ts": {Data: []byte("const os = require('os');\nexport function Create() {}")}},
			want:  `"os": only relative imports`,
		},
		{
			name:  "Node globals",
			files: fstest.MapFS{"plugin.ts": {Data: []byte("process.exit(1);\nexport function Create() {}")}},
			want:  "plugin source failed to load: process is not defined",
		},
	}

	ctr := newContainer(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			name := strings.ReplaceAll(tc.name, " ", "-")
			err := plg.Register[testcontract.Fooer](t.Context(), ctr, name, plg.WithStorage(tsSource(t, tc.files)))
			require.ErrorIs(t, err, plg.ErrLoad)
			require.ErrorContains(t, err, name+"/p (storage mem): ")
			require.ErrorContains(t, err, tc.want)

			var failure *plg.PanicError

			assert.NotErrorAs(t, err, &failure, "a plugin that cannot load is an ordinary error")
		})
	}
}

// shaperMembers completes a Shaper object literal: each member not in members echoes or returns a zero value.
func shaperMembers(members string) string {
	for _, stub := range []string{
		"Echo: (o: unknown) => o", "EchoPtr: (p: unknown) => p", "Inspect: () => ({})", "Bump: (o: unknown) => o",
		"Grow: () => null", "Index: () => null", "Reverse: (b: unknown) => b", "Later: (t: unknown) => t",
		"Sum: (b: number) => b",
	} {
		name, _, _ := strings.Cut(stub, ":")
		if !strings.Contains(members, name+":") {
			members += ",\n" + stub
		}
	}

	return strings.TrimPrefix(members, ",\n")
}

// tsShaper registers a TypeScript Shaper whose Create returns members (completed by stubs) in a container of its
// own, as "shaper/p".
//
//nolint:ireturn // the plugin is served as the contract interface under test
func tsShaper(t *testing.T, members string) (testcontract.Shaper, *plg.Container) {
	t.Helper()

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Shaper](t.Context(), ctr, "shaper",
		plg.WithStorage(tsSource(t, tsPlugin(shaperMembers(members)))), plg.WithPool(1, 1)))

	shaper, err := plg.Get[testcontract.Shaper](t.Context(), ctr, "shaper/p")
	require.NoError(t, err)

	return shaper, ctr
}

// TestTypeScriptResultsThatDoNotFitFailNamingTheField: a JS value that does not fit the Go result type fails
// the call with ErrConversion naming the result and the field (by its json name), and the instance keeps serving.
func TestTypeScriptResultsThatDoNotFitFailNamingTheField(t *testing.T) {
	t.Parallel()

	shaper, ctr := tsShaper(t, `
		Echo: (o: any) => o.Lines === null ? { When: "yesterday" } : { Lines: [{ sku: "a", qty: 1.5 }] },
		EchoPtr: () => ({ Base: 7 }),
		Inspect: () => ({ Nanos: 2 ** 31 * 2 ** 31 }),
		Bump: () => ({ Raw: "bytes", When: { seconds: 1, nanos: 1e9 } }),
		Grow: () => { const t: any = { Name: "loop" }; t.Kids = [t]; return t; },
		Index: () => ({ a: [{ sku: 1 }] }),
		Reverse: () => [1, 2, 3],
		Later: () => new Date(NaN),
		Sum: () => 2 ** 53 + 2`)
	sample := testcontract.SampleOrder()

	tests := []struct {
		err  error
		want string
	}{
		{errOf(shaper.Echo(t.Context(), testcontract.Order{})), "shaper/p.Echo: result 0: When: " +
			"want a Date or {seconds, nanos}, got string"},
		{errOf(shaper.Echo(t.Context(), sample)), "shaper/p.Echo: result 0: Lines[0].qty: wire: not an integer: 1.5"},
		{errOf(shaper.EchoPtr(t.Context(), &sample)), "shaper/p.EchoPtr: result 0: Base: want an object, got number"},
		{errOf(shaper.Inspect(t.Context(), sample)), "shaper/p.Inspect: result 0: Nanos: wire: integer beyond " +
			"±2^53: 4611686018427387904"},
		{errOf(shaper.Bump(t.Context(), sample, 1)), "shaper/p.Bump: result 0: When: wire: invalid encoding: " +
			"1000000000 nanoseconds"},
		{errOf(shaper.Grow(t.Context(), 1, 1)), "shaper/p.Grow: result 0: Kids[0].Kids[0]."},
		{errOf(shaper.Index(t.Context(), nil)), `shaper/p.Index: result 0: ["a"][0].sku: want a string, got number`},
		{errOf(shaper.Reverse(t.Context(), nil)), "shaper/p.Reverse: result 0: want a Uint8Array, got array"},
		{errOf(shaper.Later(t.Context(), time.Now(), 0)), "shaper/p.Later: result 0: wire: invalid Date"},
		{errOf(shaper.Sum(t.Context(), 0)), "shaper/p.Sum: result 0: wire: integer beyond ±2^53: 9007199254740994"},
	}

	served := instanceID(t, ctr, "shaper/p")

	for _, tc := range tests {
		require.ErrorIs(t, tc.err, plg.ErrConversion, tc.want)
		require.NotErrorIs(t, tc.err, plg.ErrCall, tc.want)
		assert.Contains(t, tc.err.Error(), tc.want)
	}

	assert.Contains(t, tests[5].err.Error(), "wire: value nested too deeply", "a cyclic value cannot hang the guest")
	assert.Equal(t, served, instanceID(t, ctr, "shaper/p"), "the instance kept serving")
}

// TestTypeScriptValueForms checks the JS forms results may take besides those the guest hands out: a Date for a
// time.Time, an ArrayBuffer for []byte, absent struct fields as zero values, undefined for nil, and float32
// results rounded as Go's float32 conversion rounds.
func TestTypeScriptValueForms(t *testing.T) {
	t.Parallel()

	shaper, _ := tsShaper(t, `
		Echo: () => ({ Base: {}, Lines: [{ sku: "a" }], Grid: [[1e39, 0.1]] }),
		EchoPtr: () => undefined,
		Reverse: () => new Uint8Array([1, 2, 3, 4]).buffer.slice(1, 3),
		Later: () => new Date(Date.UTC(2026, 2, 1, 12, 0, 0, 7)),
		Grow: () => ({ Name: "leaf", Kids: [] })`)

	got, err := shaper.Echo(t.Context(), testcontract.Order{})
	require.NoError(t, err)
	assert.Equal(t, testcontract.Order{
		Lines: []testcontract.Line{{SKU: "a"}},
		Grid:  [][]float32{{float32(math.Inf(1)), 0.1}},
	}, got, "absent fields are zero values (time.Time{} included); float32 overflows to +Inf, as in Go")

	ptr, err := shaper.EchoPtr(t.Context(), &got)
	require.NoError(t, err)
	assert.Nil(t, ptr, "undefined is nil")

	raw, err := shaper.Reverse(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, []byte{2, 3}, raw, "an ArrayBuffer")

	at, err := shaper.Later(t.Context(), time.Time{}, 0)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, time.March, 1, 12, 0, 0, 7000000, time.UTC), at, "a Date, to the millisecond")

	tree, err := shaper.Grow(t.Context(), 0, 0)
	require.NoError(t, err)
	assert.Equal(t, &testcontract.Tree{Name: "leaf", Kids: []*testcontract.Tree{}}, tree, "empty stays non-nil")
}

func TestTypeScriptMultipleResultsAreAnArray(t *testing.T) {
	t.Parallel()

	fooer := tsFooer(t, newContainer(t), `
		Boil: (c: number) => c > 0 ? [true] : "hot",
		Scalars: (...args: any[]) => { args[2] = 128; return args; },
		Spin: async (n: number) => { await null; return n * 2; }`)

	_, _, err := fooer.Boil(t.Context(), 1)
	require.ErrorIs(t, err, plg.ErrConversion)
	require.ErrorContains(t, err, "ts/p.Boil: results: want an array of 2 values, got an array of another length")

	_, _, err = fooer.Boil(t.Context(), -1)
	require.ErrorContains(t, err, "ts/p.Boil: results: want an array of 2 values, got string")

	//nolint:dogsled // Scalars returns fourteen values; only its error matters here
	_, _, _, _, _, _, _, _, _, _, _, _, _, _, err = fooer.Scalars(t.Context(), false, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, "")
	require.ErrorIs(t, err, plg.ErrConversion)
	require.ErrorContains(t, err, "ts/p.Scalars: result 2: wire: value out of range: 128 does not fit int8",
		"a JS number never wraps around like a Go int8 would")

	n, err := fooer.Spin(t.Context(), 21)
	require.NoError(t, err)
	assert.Equal(t, 42, n, "an async method's settled value")
}

func TestTypeScriptPluginsDefaultTo64MiB(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := loggedContainer(t, &logs)
	store := tsSource(t, tsPlugin(fooerMembers("")))
	require.NoError(t, store.Put(&plg.Descriptor{Key: "go", Language: plg.LanguageGo, Entrypoint: "plugin.go"},
		pluginFiles("fooer")))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "mixed", plg.WithStorage(store),
		plg.WithPool(1, 1)))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "limited",
		plg.WithStorage(tsSource(t, tsPlugin(fooerMembers("")))), plg.WithPool(1, 1), plg.WithMemoryLimit(16*mib)))

	logged := logs.String()
	assert.Contains(t, logged, `"key":"mixed/p","language":"typescript",`)
	assert.Contains(t, logged, `"memory_limit":67108864,`, "TypeScript's default")
	assert.Contains(t, logged, `"key":"mixed/go","language":"go",`)
	assert.Contains(t, logged, `"memory_limit":134217728,`, "Go's default, in the same Register")
	assert.Contains(t, logged, `"memory_limit":16777216,`, "WithMemoryLimit applies to every language")
	assert.Equal(t, uint64(64*mib+128*mib+16*mib), plg.MemoryInUse(ctr))
}

// TestTypeScriptHostArgumentsThatDoNotFitReject: a host method called with arguments its Go parameters cannot
// take rejects with a TypeError naming the parameter, before reaching the host; the instance keeps serving.
func TestTypeScriptHostArgumentsThatDoNotFitReject(t *testing.T) {
	t.Parallel()

	src := tsPlugin(`
		async Describe(id: string) { return host.Lookup(id === "number" ? 42 : id); },
		async Relay(msg: string) {
			try { await host.Sum(1, msg === "array" ? [1, "2"] : 2.5); } catch (e: any) { return e.name + ": " + e.message; }
			return "no error";
		},
		Value: () => host.Value(),
		Origin: () => typeof host.Lookup + " " + Object.keys(host).sort().join(","),
		Total: (base: number, more: number[]) => host.Sum(base, more),
		Keys: (keys: object) => host.Keys(keys)`)
	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Hosted](t.Context(), ctr, "hosted",
		plg.WithStorage(tsSource(t, src)), plg.WithHost[testcontract.Host](newTestHost())))

	hosted, err := plg.Get[testcontract.Hosted](t.Context(), ctr, "hosted/p")
	require.NoError(t, err)

	_, err = hosted.Describe(t.Context(), "number")
	require.EqualError(t, err, "host.Lookup: parameter 1: want a string, got number")

	got, err := hosted.Relay(t.Context(), "array")
	require.NoError(t, err)
	assert.Equal(t, "TypeError: host.Sum: parameter 2: [1]: want an integer number, got string", got)

	got, err = hosted.Relay(t.Context(), "float")
	require.NoError(t, err)
	assert.Equal(t, "TypeError: host.Sum: parameter 2: want an array, got number", got)

	got, err = hosted.Origin(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "function Fail,Keys,Lookup,Size,Sum,Value", got, "host has exactly the Host's methods")

	total, err := hosted.Total(t.Context(), 1, 2, 3)
	require.NoError(t, err)
	assert.Equal(t, 6, total, "the instance kept serving")
}

// sumRecorder is testHost recording the arguments of every Sum call it serves, in order.
type sumRecorder struct {
	*testHost

	sums chan string
}

func (h *sumRecorder) Sum(ctx context.Context, base int, more ...int) (int, error) {
	h.sums <- fmt.Sprint(base, more)

	return h.testHost.Sum(ctx, base, more...)
}

// received returns the Sum calls recorded since the last time, in order.
func (h *sumRecorder) received() []string {
	var got []string

	for {
		select {
		case sum := <-h.sums:
			got = append(got, sum)
		default:
			return got
		}
	}
}

// reentrantMembers is a Hosted plugin whose host call arguments and results run getters that call the host or
// fetch while the glue encodes them. Relay(op [url]) calls host.Sum(1, more), more[0] being a getter returning
// 2 after making the op's nested request (more[1], for "path", a value that does not fit). For "silent", that
// request (Lookup of an 8-byte id) is exactly as long as what the outer call encodes before more[0]: overwritten by
// it, the outer call reached the host as a valid Sum(8, [2]). Describe(op) returns a user whose id getter makes a
// host call that does not fit, then throws (op "throw") or returns, before name (a number, for op "path") does not
// fit either.
const reentrantMembers = `
	async Relay(cmd: string) {
		const [op, url] = cmd.split(" ");
		let inner: Promise<unknown> = Promise.resolve("-");
		const nested: Record<string, () => void> = {
			host: () => { inner = host.Sum(99, [7]); },
			silent: () => { inner = host.Lookup(String.fromCharCode(0, 0, 0, 0, 1, 0, 0, 0)).catch(() => "no user"); },
			fetch: () => { inner = fetch(url, { method: "POST", body: "inner" }).then((r) => r.text()); },
			throw: () => { host.Sum(1, [1, 2, "x"]).catch(() => {}); throw new Error("getter failed"); },
			path: () => { inner = host.Sum(1, [1, 2, "x"]).catch((e: any) => e.message); },
		};
		const more: unknown[] = op === "path" ? [0, "y"] : [0];
		Object.defineProperty(more, 0, { enumerable: true, get() { nested[op](); return 2; } });
		try {
			const outer = await host.Sum(1, more);
			return outer + " " + await inner;
		} catch (e: any) {
			return e.name + ": " + e.message + " | " + await inner;
		}
	},
	async Describe(op: string) {
		const user: Record<string, unknown> = { name: op === "path" ? 5 : "n" };
		Object.defineProperty(user, "id", { enumerable: true, get() {
			host.Sum(1, [1, 2, "x"]).catch(() => {});
			if (op === "throw") throw new Error("getter failed");
			return "u";
		} });
		return user;
	},
	Value: () => host.Value(),
	Origin: () => "",
	Total: (base: number, more: number[]) => host.Sum(base, more),
	Keys: (keys: object) => host.Keys(keys)`

// TestTypeScriptReentrantHostCallsKeepTheirOwnArguments: the glue runs the plugin's getters while it encodes a host
// call's arguments or a method's results, and a getter may call the host or fetch meanwhile. Each request reaches
// the host exactly as its caller made it, blocking or multiplexed, and each failure to convert names its own value
// (or is the getter's own exception), not the nested call's.
func TestTypeScriptReentrantHostCallsKeepTheirOwnArguments(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		assert.NoError(t, err)

		reply := req.Method + " " + req.URL.Path + " " + string(body)
		_, err = io.WriteString(writer, reply) //nolint:gosec // G705: a test server echoing its own test's request
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	const notAnInteger = "host.Sum: parameter 2: [2]: want an integer number, got string"

	calls := []struct {
		cmd, want string
		sums      []string
	}{
		{cmd: "host", want: "3 106", sums: []string{"99 [7]", "1 [2]"}},
		{cmd: "silent", want: "3 no user", sums: []string{"1 [2]"}},
		{cmd: "fetch " + srv.URL + "/inner", want: "3 POST /inner inner", sums: []string{"1 [2]"}},
		{cmd: "throw", want: "Error: getter failed | -"},
		{
			cmd:  "path",
			want: "TypeError: host.Sum: parameter 2: [1]: want an integer number, got string | " + notAnInteger,
		},
	}

	for _, mode := range []struct {
		name string
		opts []plg.RegisterOption
	}{
		{name: "blocking"},
		{name: "multiplexed", opts: []plg.RegisterOption{plg.WithConcurrentCalls(2)}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			recorder := &sumRecorder{testHost: newTestHost(), sums: make(chan string, 8)}
			opts := append([]plg.RegisterOption{
				plg.WithStorage(tsSource(t, tsPlugin(reentrantMembers))), plg.WithPool(1, 1),
				plg.WithHost[testcontract.Host](recorder), plg.WithHTTP(openPolicy(hostOf(t, srv.URL))),
			}, mode.opts...)
			ctr := newContainer(t)
			require.NoError(t, plg.Register[testcontract.Hosted](t.Context(), ctr, "reentrant", opts...))

			hosted, err := plg.Get[testcontract.Hosted](t.Context(), ctr, "reentrant/p")
			require.NoError(t, err)

			for _, call := range calls {
				got, relayErr := hosted.Relay(t.Context(), call.cmd)
				require.NoError(t, relayErr, call.cmd)
				assert.Equal(t, call.want, got, call.cmd)
				assert.Equal(t, call.sums, recorder.received(), "%s: the Sum calls the host served", call.cmd)
			}

			_, err = hosted.Describe(t.Context(), "path")
			require.ErrorIs(t, err, plg.ErrConversion)
			require.ErrorContains(t, err, ": result 0: name: want a string, got number")

			_, err = hosted.Describe(t.Context(), "throw")
			require.NotErrorIs(t, err, plg.ErrConversion)
			require.EqualError(t, err, "getter failed", "the getter's own exception")

			user, err := hosted.Describe(t.Context(), "ok")
			require.NoError(t, err)
			assert.Equal(t, testcontract.User{ID: "u", Name: "n"}, user)
			assert.Empty(t, recorder.received(), "no host call that does not fit reached the host")
		})
	}
}

// detachedMembers is a Hosted plugin whose Relay(op [url]) hands bytes to the glue: a Uint8Array or ArrayBuffer
// detached by ArrayBuffer.prototype.transfer, or a genuine empty one, to a Host method, as a POST body, or to
// UTF-8 decoding. It returns the Host's answer, or the exception the plugin caught.
const detachedMembers = `
	async Relay(cmd: string) {
		const [op, url] = cmd.split(" ");
		const u8 = () => { const b = new Uint8Array([1, 2, 3]); (b.buffer as any).transfer(); return b; };
		const ab = () => { const b = new ArrayBuffer(3); (b as any).transfer(); return b; };
		const ops: Record<string, () => Promise<unknown>> = {
			"host-u8": () => host.Size(u8()),
			"host-ab": () => host.Size(ab()),
			"host-empty": () => host.Size(new Uint8Array(0)),
			"host-empty-ab": () => host.Size(new ArrayBuffer(0)),
			"post-u8": () => fetch(url, { method: "POST", body: u8() }).then((r) => r.text()),
			"post-empty": () => fetch(url, { method: "POST", body: new Uint8Array(0) }).then((r) => r.text()),
			"text-u8": () => new Response(u8()).text().then((s) => "text " + JSON.stringify(s)),
			"text-empty": () => new Response(new Uint8Array(0)).text().then((s) => "text " + JSON.stringify(s)),
		};
		try {
			return String(await ops[op]());
		} catch (e: any) {
			return e.name + ": " + e.message;
		}
	},
	Describe: () => ({}),
	Value: () => "",
	Origin: () => "",
	Total: (base: number) => base,
	Keys: (keys: object) => keys`

// TestTypeScriptDetachedBuffersFailToCross: a detached Uint8Array or ArrayBuffer cannot be read, so handing one to
// the host — as a method's result, a Host method's argument or a POST body — or decoding it as UTF-8 fails with the
// plugin-side TypeError, where it used to cross as empty bytes. Genuine empty buffers still cross as empty.
func TestTypeScriptDetachedBuffersFailToCross(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		assert.NoError(t, err)

		_, err = fmt.Fprintf(writer, "%s %d", req.Method, len(body)) //nolint:gosec // G705: echoes its own test
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	ctr := newContainer(t)
	require.NoError(t, plg.Register[testcontract.Hosted](t.Context(), ctr, "detached",
		plg.WithStorage(tsSource(t, tsPlugin(detachedMembers))), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit),
		plg.WithHost[testcontract.Host](newTestHost()), plg.WithHTTP(openPolicy(hostOf(t, srv.URL)))))
	require.NoError(t, plg.Register[testcontract.Shaper](t.Context(), ctr, "shaper",
		plg.WithStorage(tsSource(t, tsPlugin(shaperMembers(`Reverse: (b: Uint8Array) => {
			if (b.length === 0) return new Uint8Array(0);
			const d = b.length === 1 ? new Uint8Array(3) : new ArrayBuffer(3);
			((d as any).buffer ?? d).transfer();
			return d;
		}`)))), plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit)))

	hosted, err := plg.Get[testcontract.Hosted](t.Context(), ctr, "detached/p")
	require.NoError(t, err)

	// QuickJS's messages: a Uint8Array over a detached buffer is out of bounds, an ArrayBuffer itself detached.
	const detachedView, detachedBuffer = "ArrayBuffer is detached or resized", "ArrayBuffer is detached"

	for _, call := range []struct{ cmd, want string }{
		{cmd: "host-u8", want: "TypeError: " + detachedView},
		{cmd: "host-ab", want: "TypeError: " + detachedBuffer},
		{cmd: "host-empty", want: "0"},
		{cmd: "host-empty-ab", want: "0"},
		{cmd: "post-u8 " + srv.URL, want: "TypeError: " + detachedView},
		{cmd: "post-empty " + srv.URL, want: "POST 0"},
		{cmd: "text-u8", want: "TypeError: " + detachedView},
		{cmd: "text-empty", want: `text ""`},
	} {
		got, relayErr := hosted.Relay(t.Context(), call.cmd)
		require.NoError(t, relayErr, call.cmd)
		assert.Equal(t, call.want, got, call.cmd)
	}

	shaper, err := plg.Get[testcontract.Shaper](t.Context(), ctr, "shaper/p")
	require.NoError(t, err)

	for in, want := range map[string]string{"\x01": detachedView, "\x01\x02": detachedBuffer} {
		_, err = shaper.Reverse(t.Context(), []byte(in))

		var pluginErr *plg.PluginError

		require.ErrorAs(t, err, &pluginErr, "a detached result (%q)", in)
		assert.Equal(t, want, pluginErr.Message, "the plugin's own TypeError (%q)", in)
		require.NotErrorIs(t, err, plg.ErrConversion)
	}

	empty, err := shaper.Reverse(t.Context(), []byte{})
	require.NoError(t, err)
	assert.Equal(t, []byte{}, empty, "a genuine empty result crosses as empty, not nil")
}

// TestTypeScriptMultipleResultsStopAtAThrowingGetter: Boil returns its two results as an array whose first element
// is a getter, which throws (c = 1), runs out of memory (c = 2) or returns true; the second element's getter counts
// its runs in later, which Count returns. A throw fails the call with the plugin's own error and an out-of-memory
// discards the instance, as anywhere else in the plugin, and in both no later getter runs.
func TestTypeScriptMultipleResultsStopAtAThrowingGetter(t *testing.T) {
	t.Parallel()

	ctr := newContainer(t)
	fooer := tsFooer(t, ctr, `
		later: 0,
		Boil(c: number) {
			const self = this as any;
			const results: unknown[] = [];
			Object.defineProperty(results, 0, { enumerable: true, get() {
				if (c === 1) throw new Error("getter failed");
				const hoard: unknown[] = [];
				while (c === 2) hoard.push(new Array(1 << 16).fill("leak"));
				return true;
			} });
			Object.defineProperty(results, 1, { enumerable: true, get() { self.later++; return 1.5; } });
			return results;
		},
		Count() { return (this as any).later; }`, plg.WithPool(1, 1), plg.WithMemoryLimit(hostileLimit))

	served := instanceID(t, ctr, "ts/p")

	_, _, err := fooer.Boil(t.Context(), 1)

	var pluginErr *plg.PluginError

	require.ErrorAs(t, err, &pluginErr)
	assert.Equal(t, "getter failed", pluginErr.Message, "the getter's own exception")
	require.NotErrorIs(t, err, plg.ErrConversion)

	later, err := fooer.Count(t.Context())
	require.NoError(t, err)
	assert.Zero(t, later, "no getter ran after the one that threw")
	assert.Equal(t, served, instanceID(t, ctr, "ts/p"), "a thrown error keeps the instance")

	_, _, err = fooer.Boil(t.Context(), 2)
	failure := requirePanic(t, err, "ts/p", plg.PhaseCall)
	assert.Equal(t, "out of memory", failure.Value)
	assert.Contains(t, failure.Guest, "the plugin ran out of memory")
	assert.NotEqual(t, served, instanceID(t, ctr, "ts/p"), "out of memory discards the instance")

	boiled, temp, err := fooer.Boil(t.Context(), 0)
	require.NoError(t, err)
	assert.True(t, boiled)
	assert.InDelta(t, 1.5, temp, 0)

	later, err = fooer.Count(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, later, "the fresh instance ran the second getter once, for the call that succeeded")
}

// TestTypeScriptFetch exercises the parts of fetch the contract suite does not: JSON bodies, consumed bodies,
// the redirect modes, relative redirects, byte bodies and Headers, and refusals as TypeErrors.
func TestTypeScriptFetch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		var reply string

		switch req.URL.Path {
		case "/json":
			writer.Header().Set("Content-Type", "application/json")

			reply = `{"a":[1,2]}`
		case "/dir/redirect":
			writer.Header().Set("Location", "../landed?x=1")
			writer.WriteHeader(http.StatusFound)
		case "/landed":
			reply = "landed " + req.URL.RawQuery
		default:
			body, err := io.ReadAll(req.Body)
			assert.NoError(t, err)

			reply = strings.Join([]string{
				req.Method, req.Header.Get("Content-Type"), strings.Join(req.Header.Values("X-A"), ","), string(body),
			}, "|")
		}

		_, err := io.WriteString(writer, reply) //nolint:gosec // G705: a test server echoing its own test's request
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	fooer := tsFooer(t, newContainer(t), `
		async Foo(cmd: string) {
			const [op, url] = cmd.split(" ");
			switch (op) {
			case "json": {
				const r = await fetch(url);
				const v = await r.json();
				let twice = "";
				try { await r.text(); } catch (e: any) { twice = e.message; }
				return JSON.stringify({ v, ok: r.ok, used: r.bodyUsed, twice, type: r.headers.get("content-type") });
			}
			case "manual": {
				const r = await fetch(url, { redirect: "manual" });
				return r.status + " " + r.headers.get("location") + " " + r.redirected;
			}
			case "error":
				try { await fetch(url, { redirect: "error" }); return "followed"; } catch (e: any) { return e.name + ": " + e.message; }
			case "follow": {
				const r = await fetch(url);
				return r.status + " " + r.redirected + " " + r.url + " " + await r.text();
			}
			case "bytes": {
				const headers = new Headers([["X-A", "1"], ["x-a", "2"]]);
				const r = await fetch(url, { method: "put", body: new Uint8Array([104, 105]), headers });
				return await r.text() + " " + [...headers.keys()].join();
			}
			default:
				try { await fetch(url); return "fetched"; } catch (e: any) { return e.name + ": " + e.message; }
			}
		}`, plg.WithHTTP(openPolicy(hostOf(t, srv.URL))))

	tests := []struct{ cmd, want string }{
		{"json " + srv.URL + "/json", `{"v":{"a":[1,2]},"ok":true,"used":true,` +
			`"twice":"Body has already been consumed","type":"application/json"}`},
		{"manual " + srv.URL + "/dir/redirect", "302 ../landed?x=1 false"},
		{"error " + srv.URL + "/dir/redirect", "TypeError: fetch: redirected to ../landed?x=1"},
		{"follow " + srv.URL + "/dir/redirect", "200 true " + srv.URL + "/landed?x=1 landed x=1"},
		{"bytes " + srv.URL + "/echo", "PUT||1, 2|hi x-a"}, // a header's values are combined, as fetch does
		{"refused http://api.example.com/", "TypeError: " + httpcap.ErrHostNotAllowed.Error()},
	}

	for _, tc := range tests {
		got, err := fooer.Foo(t.Context(), tc.cmd)
		require.NoError(t, err, tc.cmd)
		assert.True(t, strings.HasPrefix(got, tc.want), "%s: got %q, want %q", tc.cmd, got, tc.want)
	}
}

func TestTypeScriptCloseRunsThePluginsClose(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	ctr := plg.New(plg.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	tsFooer(t, ctr, `async Close() { await null; console.log("closing", typeof this.Spin); }`, plg.WithPool(1, 1))

	store := tsSource(t, tsPlugin(fooerMembers(`Close() { throw new Error("cannot close"); }`)))
	require.NoError(t, plg.Register[testcontract.Fooer](t.Context(), ctr, "bad", plg.WithStorage(store),
		plg.WithPool(1, 1)))

	err := ctr.Close(t.Context())
	require.ErrorContains(t, err, "bad/p", "a Close that throws fails Container.Close")
	require.ErrorContains(t, err, "cannot close")
	assert.Contains(t, logs.String(), `"stream":"stdout","line":"closing function"`, "Close is called with this bound")
}

// TestTypeScriptDeepCallsLeaveNoNativeStack: wazero runs each JS frame on KiBs of its native stack, which grows
// for the deepest call and stays with the instance's export, outside the instance's memory limit. After a deep
// recursion, the instance holds no more host memory than before (without the guest's deep-stack flag, about
// 40 MiB more).
//
//nolint:paralleltest // it measures the process heap, which no other test may move meanwhile
func TestTypeScriptDeepCallsLeaveNoNativeStack(t *testing.T) {
	fooer := tsFooer(t, newContainer(t),
		"Spin(n: number) { const down = (k: number): number => (k === 0 ? 0 : down(k - 1) + 1); return down(n); }",
		plg.WithPool(1, 1))

	_, err := fooer.Spin(t.Context(), 1)
	require.NoError(t, err)

	before := heapInUse()

	depth, err := fooer.Spin(t.Context(), 2500)
	require.NoError(t, err)
	assert.Equal(t, 2500, depth)
	assert.Less(t, heapInUse()-before, int64(4<<20), "the instance kept no grown native stack")
}

// heapInUse returns the bytes of live heap objects, after a collection.
func heapInUse() int64 {
	runtime.GC() //nolint:revive // call-to-gc: the live heap is measured after a collection

	var stats runtime.MemStats

	runtime.ReadMemStats(&stats)

	return int64(stats.HeapAlloc) //nolint:gosec // G115: the heap is far smaller than 2^63 bytes
}

// BenchmarkCallTypeScript is one call into a TypeScript plugin (Foo: an imported function, string building), with
// every protection on.
func BenchmarkCallTypeScript(b *testing.B) {
	benchCall(b, typescript())
}

// BenchmarkCallTypeScriptParallel calls a TypeScript plugin from GOMAXPROCS goroutines: with a pool of one
// instance the calls contend for it, with one instance per goroutine they run in parallel.
func BenchmarkCallTypeScriptParallel(b *testing.B) {
	benchCallParallel(b, typescript())
}

// BenchmarkCallTypeScriptStruct is one call into a TypeScript plugin with SampleOrder as argument and result: the
// glue builds the JS objects from the wire encoding and encodes them back.
func BenchmarkCallTypeScriptStruct(b *testing.B) {
	benchCallStruct(b, typescript())
}

// BenchmarkCallTypeScriptHost is one TypeScript plugin call awaiting one host call (Value).
func BenchmarkCallTypeScriptHost(b *testing.B) {
	benchHostValue(b, typescript())
}

// BenchmarkCallTypeScriptHostStruct is one TypeScript plugin call awaiting one host call returning a struct
// (Describe).
func BenchmarkCallTypeScriptHostStruct(b *testing.B) {
	benchHostStruct(b, typescript())
}

// BenchmarkHTTPRoundTripTypeScript is one TypeScript plugin call awaiting one fetch through the host.
func BenchmarkHTTPRoundTripTypeScript(b *testing.B) {
	benchHTTP(b, typescript())
}

func BenchmarkHTTPRoundTripTypeScriptParallel(b *testing.B) {
	benchHTTPParallel(b, typescript())
}

// TestABrokenMultiplexedInstanceSkipsItsHangingClose runs a call out of memory on a multiplexed TypeScript
// instance whose Close never returns, while the step running it succeeds: the broken instance is discarded
// without running Close, so the failing call returns and a new instance serves the next call by its deadline.
// The healthy instance left runs Close when the container closes, stopped at the close timeout.
func TestABrokenMultiplexedInstanceSkipsItsHangingClose(t *testing.T) {
	t.Parallel()

	ctr := plg.New()
	plg.SetCloseTimeout(ctr, 100*time.Millisecond)

	plug := tsFooer(t, ctr, `Close() { for (;;) {} },
Fail(msg: string) { const hoard = []; for (;;) { hoard.push(new Array(1 << 16).fill(msg)); } }`,
		plg.WithPool(1, 1), plg.WithConcurrentCalls(2), plg.WithMemoryLimit(16<<20))

	failed := make(chan error, 1)

	go func() { failed <- plug.Fail(t.Context(), "oom") }()

	select {
	case err := <-failed:
		culprit, ok := errors.AsType[*plg.PanicError](err)
		require.True(t, ok, "out of memory is the call's panic: %v", err)
		assert.Equal(t, plg.PhaseCall, culprit.Phase)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "discarding the broken instance ran its hanging Close")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	out, err := plug.Foo(ctx, "next")
	require.NoError(t, err, "a new instance serves the next call")
	assert.Equal(t, "next", out)
	assert.Equal(t, 1, inspect(t, ctr, "ts/p").Pool.Instances, "the broken instance's place is back")
	assert.Equal(t, uint64(16<<20), plg.MemoryInUse(ctr), "and its budget charge")

	require.ErrorIs(t, ctr.Close(t.Context()), context.DeadlineExceeded, "a healthy instance's Close is bounded")
	assert.Zero(t, plg.MemoryInUse(ctr))
}
