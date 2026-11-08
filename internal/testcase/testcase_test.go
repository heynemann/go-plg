package testcase_test

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/heynemann/go-plg/internal/adminjson"
	"github.com/heynemann/go-plg/internal/offline"
	"github.com/heynemann/go-plg/internal/testcase"
)

// The errors the test plugins fail calls with.
var (
	errNotFound = errors.New("plg: Spin: user not found")
	errPanicked = errors.New("it panicked or trapped: not found")
	errNoParts  = errors.New("no parts")
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// The test contract: every data type, a method of several results, one of none, a Host and a validation input.
type (
	gift struct {
		Message string `json:"message"`
	}
	line struct {
		SKU   string `json:"sku"`
		Qty   int    `json:"qty"`
		Cents int    `json:"cents"`
	}
	order struct {
		Placed   time.Time         `json:"placed"`
		Gift     *gift             `json:"gift"`
		Tags     map[string]string `json:"tags"`
		Customer string            `json:"customer"`
		Skipped  string            `json:"-"`
		Plain    string
		Lines    []line  `json:"lines"`
		Data     []byte  `json:"data"`
		Big      int64   `json:"big"`
		hidden   int     //nolint:unused // an unexported field, which is no field of a case's values
		Ratio    float32 `json:"ratio"`
		Count    uint16  `json:"count"`
		Small    int8    `json:"small"`
		Flag     bool    `json:"flag"`
	}
	quote struct {
		Reasons  []string `json:"reasons"`
		Total    int      `json:"total"`
		Discount int      `json:"discount"`
	}
	limits struct {
		MaxPercent int `json:"maxPercent"`
	}
	rule interface {
		Quote(ctx context.Context, o order) (quote, error)
		Spin(ctx context.Context, mode string) error
		Split(ctx context.Context, cents, parts int) ([]int, int, error)
	}
	customers interface {
		Pair(ctx context.Context, a, b string) (string, int, error)
		Tier(ctx context.Context, customer string) (string, error)
	}
)

// level is a number with a String method, which a case writes as the number all the same.
type level int

func (level) String() string { return "high" }

// Method indexes.
const (
	quoteMethod = iota
	spinMethod
	splitMethod
	validateMethod
)

// methods returns the methods of the interface T.
func methods[T any]() []reflect.Method {
	typ := reflect.TypeFor[T]()
	out := make([]reflect.Method, typ.NumMethod())

	for i := range out {
		out[i] = typ.Method(i)
	}

	return out
}

// newContract returns the test contract.
func newContract() *offline.Contract {
	host := &offline.Host{Methods: methods[customers](), Params: [][]string{{"a", "b"}, {"customer"}}}
	host.Funcs = testcase.HostFuncs(host)

	return &offline.Contract{
		Methods: methods[rule](), Params: [][]string{{"order"}, {"mode"}, {"cents", "parts"}},
		Host: host, Validation: &offline.Validation{Input: reflect.TypeFor[limits]()},
	}
}

// runner is an [offline.Runner] standing in for a guest: it calls the plugin's code, play, with the context and
// the call a case gives it, and returns what play returns.
type runner struct {
	play     func(ctx context.Context, call *offline.Call) offline.Outcome
	calls    []*offline.Call
	declared []string // the secrets the plugin declares
}

func (r *runner) Run(ctx context.Context, call *offline.Call) offline.Outcome {
	r.calls = append(r.calls, call)

	if r.play == nil {
		return offline.Outcome{}
	}

	return r.play(ctx, call)
}

func (*runner) Close(context.Context) error { return nil }

func (r *runner) Secrets() []string { return r.declared }

// returning plays a plugin returning results.
func returning(results ...any) func(context.Context, *offline.Call) offline.Outcome {
	return func(context.Context, *offline.Call) offline.Outcome {
		values := make([]reflect.Value, len(results))
		for i, result := range results {
			values[i] = reflect.ValueOf(result)
		}

		return offline.Outcome{Results: values}
	}
}

// tier calls the Host's Tier as a plugin would, returning what it answered.
func tier(ctx context.Context, contract *offline.Contract, customer string) (string, error) {
	out := contract.Host.Funcs[1].Call([]reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(customer)})
	err, _ := reflect.TypeAssert[error](out[1]) // nil for a nil error

	return out[0].String(), err
}

// response is what an HTTP stub answered: its status, body and header, or the request's error.
type response struct {
	header http.Header
	body   string
	status int
}

// get makes a GET request of url as the HTTP capability makes a plugin's, returning the response, or the error as
// its body.
func get(ctx context.Context, policy *offline.HTTP, url string) response {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, strings.NewReader("ignored"))
	if err != nil {
		return response{body: "error: " + err.Error()}
	}

	resp, err := policy.Transport.RoundTrip(req)
	if err != nil {
		return response{body: "error: " + err.Error()}
	}

	defer resp.Body.Close() //nolint:errcheck // read whole below

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{body: "error: " + err.Error()}
	}

	return response{status: resp.StatusCode, body: string(body), header: resp.Header}
}

// run loads the files and runs every case of the one test file among them with the runner.
func run(t *testing.T, files fstest.MapFS, plugin *runner) []testcase.Result {
	t.Helper()

	loaded := testcase.Load(files)
	require.Len(t, loaded, 1)
	require.NoError(t, loaded[0].Err)

	return testcase.Run(t.Context(), newContract(), files, plugin, loaded[0])
}

// cases makes the test file tests/t.yaml of cases.
func cases(yaml string, extra ...string) fstest.MapFS {
	files := fstest.MapFS{"tests/t.yaml": {Data: []byte("cases:\n" + yaml)}}
	for i := 0; i+1 < len(extra); i += 2 {
		files[extra[i]] = &fstest.MapFile{Data: []byte(extra[i+1])}
	}

	return files
}

func TestLoadFindsTheTestFilesInOrder(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"tests/b.yaml":         {Data: []byte("cases: []")},
		"tests/a.yaml":         {Data: []byte("cases: [{name: one, call: Spin, input: {mode: x}}]")},
		"tests/c.yml":          {Data: []byte("not: read")},
		"tests/sub/d.yaml":     {Data: []byte("not: read")},
		"fixtures/order.yaml":  {Data: []byte("not: read")},
		"plugin.yaml":          {Data: []byte("not: read")},
		"tests/notes.txt.yaml": {Data: []byte("cases: []")},
	}

	loaded := testcase.Load(files)

	names := make([]string, len(loaded))
	for i, file := range loaded {
		names[i] = file.Name
		require.NoError(t, file.Err)
	}

	assert.Equal(t, []string{"tests/a.yaml", "tests/b.yaml", "tests/notes.txt.yaml"}, names)
	assert.Nil(t, loaded[0].HTTP, "no httpPolicy: requests are refused")
	assert.Equal(t, "one", loaded[0].Cases[0].Name)
	assert.Empty(t, testcase.Load(fstest.MapFS{}), "no tests directory")
}

func TestLoadRejectsMalformedFiles(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, data, want string }{
		{"not YAML", "cases: [", "tests/t.yaml: parsing: yaml: line 1"},
		{"two documents", "cases: []\n---\ncases: []", "tests/t.yaml must hold exactly one document"},
		{"an anchor", "cases: &a []", "anchors are not allowed"},
		{"a list", "- name: x", "tests/t.yaml:1:1: a test case file is a mapping with cases (and an httpPolicy)"},
		{
			"an unknown key",
			"case: []",
			`tests/t.yaml:1:1: a test case file has no key "case" (keys: cases, httpPolicy)`,
		},
		{"no cases", "httpPolicy: {allowedHosts: [a.example]}", "tests/t.yaml:1:1: the file has no cases"},
		{"cases not a list", "cases: {name: x}", "tests/t.yaml:1:8: cases is a list of test cases"},
		{"a policy not a mapping", "httpPolicy: []\ncases: []", "tests/t.yaml:1:13: httpPolicy is a mapping"},
		{
			"a policy without hosts", "httpPolicy: {timeout: 1s}\ncases: []",
			"tests/t.yaml:1:13: httpPolicy needs allowedHosts, a list of the hosts the plugin may reach",
		},
		{
			"an empty host list", "httpPolicy: {allowedHosts: []}\ncases: []",
			"httpPolicy needs allowedHosts",
		},
		{
			"a host not a string", "httpPolicy: {allowedHosts: [1]}\ncases: []",
			`tests/t.yaml:1:29: an allowed host is a string, not int "1"`,
		},
		{
			"a negative limit", "httpPolicy: {allowedHosts: [a.example], maxRequestBytes: -1}\ncases: []",
			`maxRequestBytes is a non-negative integer, not int "-1"`,
		},
		{
			"a limit not a number", "httpPolicy: {allowedHosts: [a.example], maxResponseBytes: lots}\ncases: []",
			`maxResponseBytes is a non-negative integer, not str "lots"`,
		},
		{
			"a timeout not a duration", "httpPolicy: {allowedHosts: [a.example], timeout: soon}\ncases: []",
			`timeout is a positive duration such as 1.5s, not "soon"`,
		},
		{
			"a negative timeout", "httpPolicy: {allowedHosts: [a.example], timeout: -1s}\ncases: []",
			`timeout is a positive duration such as 1.5s, not "-1s"`,
		},
		{
			"a timeout not a string", "httpPolicy: {allowedHosts: [a.example], timeout: [1s]}\ncases: []",
			"timeout is a string, not a list",
		},
		{
			"an unknown policy key", "httpPolicy: {allowedHosts: [a.example], blockPrivateNetworks: true}\ncases: []",
			`httpPolicy has no key "blockPrivateNetworks"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			loaded := testcase.Load(fstest.MapFS{"tests/t.yaml": {Data: []byte(tc.data)}})
			require.Len(t, loaded, 1)
			require.Error(t, loaded[0].Err)
			assert.Contains(t, loaded[0].Err.Error(), tc.want)
		})
	}

	t.Run("too large", func(t *testing.T) {
		t.Parallel()

		data := "cases: []\n# " + strings.Repeat("x", 1<<20)
		loaded := testcase.Load(fstest.MapFS{"tests/t.yaml": {Data: []byte(data)}})
		require.Error(t, loaded[0].Err)
		assert.Equal(t, "tests/t.yaml: larger than 1048576 bytes", loaded[0].Err.Error())
	})

	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()

		loaded := testcase.Load(fstest.MapFS{"tests/t.yaml": {Mode: 0o20000000000}}) // a directory named .yaml
		require.Error(t, loaded[0].Err)
	})
}

func TestLoadParsesThePolicy(t *testing.T) {
	t.Parallel()

	loaded := testcase.Load(fstest.MapFS{"tests/t.yaml": {Data: []byte(`
httpPolicy:
  allowedHosts: [a.example, "*.b.example:8443"]
  timeout: 1.5s
  maxRequestBytes: 10
  maxResponseBytes: 20
  maxRequestsPerCall: 3
cases: []
`)}})

	require.NoError(t, loaded[0].Err)

	policy := loaded[0].HTTP
	require.NotNil(t, policy)
	assert.NotNil(t, policy.Transport, "the stubs answer the requests")
	assert.Equal(t, []string{"a.example", "*.b.example:8443"}, policy.AllowedHosts)
	assert.Equal(t, 1500*time.Millisecond, policy.Timeout)
	assert.Equal(t, []int{10, 20, 3}, []int{policy.MaxRequestBytes, policy.MaxResponseBytes, policy.MaxRequestsPerCall})
}

// TestMalformedCasesFailAlone holds that a malformed case fails, saying where and why, without running, while
// the file's other cases run.
func TestMalformedCasesFailAlone(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, yaml, want string }{
		{"not a mapping", "- [Spin]", "tests/t.yaml:2:3: a test case is a mapping"},
		{
			"an unknown key",
			"- {name: x, call: Spin, inputs: {}}",
			`a test case has no key "inputs" (keys: call, expect`,
		},
		{"no name", "- {call: Spin}", "tests/t.yaml:2:3: name is required"},
		{"an empty name", `- {name: "", call: Spin}`, "tests/t.yaml:2:10: name is empty"},
		{"a name not a string", "- {name: [x], call: Spin}", "name is a string, not a list"},
		{"no call", "- {name: x}", "call is required"},
		{"input not a mapping", "- {name: x, call: Spin, input: [1]}", "input is a mapping of the call's parameters"},
		{"host not a list", "- {name: x, call: Spin, host: {method: Tier}}", "tests/t.yaml:2:31: host is a list"},
		{"a host stub not a mapping", "- {name: x, call: Spin, host: [Tier]}", "a host stub is a mapping"},
		{"a host stub without method", "- {name: x, call: Spin, host: [{result: a}]}", "method is required"},
		{
			"host args not a mapping", "- {name: x, call: Spin, host: [{method: Tier, args: [a]}]}",
			"args is a mapping of the method's parameters by name",
		},
		{
			"a host stub with a result and an error",
			"- {name: x, call: Spin, host: [{method: Tier, result: a, error: b}]}",
			"a host stub returns a result or an error, not both",
		},
		{
			"a host error not a string", "- {name: x, call: Spin, host: [{method: Tier, error: {a: b}}]}",
			"error is a string, not a mapping",
		},
		{
			"negative times", "- {name: x, call: Spin, host: [{method: Tier, times: -1}]}",
			`times is a non-negative integer, not int "-1"`,
		},
		{"http not a list", "- {name: x, call: Spin, http: {}}", "http is a list"},
		{"an http stub without url", "- {name: x, call: Spin, http: [{status: 200}]}", "url is required"},
		{
			"a relative url", "- {name: x, call: Spin, http: [{url: /codes}]}",
			`url "/codes" is not an absolute http or https URL`,
		},
		{
			"another scheme", "- {name: x, call: Spin, http: [{url: \"ftp://a.example/x\"}]}",
			`url "ftp://a.example/x" is not an absolute http or https URL`,
		},
		{
			"an unparsable url", "- {name: x, call: Spin, http: [{url: \"http://a b/%zz\"}]}",
			"is not an absolute http or https URL",
		},
		{
			"a method not a string", "- {name: x, call: Spin, http: [{url: \"http://a.example\", method: 1}]}",
			`method is a string, not int "1"`,
		},
		{
			"a status out of range", "- {name: x, call: Spin, http: [{url: \"http://a.example\", status: 600}]}",
			"status 600 is not an HTTP status code",
		},
		{
			"a status not a number", "- {name: x, call: Spin, http: [{url: \"http://a.example\", status: ok}]}",
			`status is a non-negative integer, not str "ok"`,
		},
		{
			"headers not a mapping", "- {name: x, call: Spin, http: [{url: \"http://a.example\", headers: [a]}]}",
			"headers is a mapping of header names to values",
		},
		{
			"a header not a string",
			"- {name: x, call: Spin, http: [{url: \"http://a.example\", headers: {X-A: [1]}}]}",
			"a header value is a string, not a list",
		},
		{
			"a body not a string", "- {name: x, call: Spin, http: [{url: \"http://a.example\", body: 10}]}",
			`body is a string, not int "10"`,
		},
		{
			"http times not a number", "- {name: x, call: Spin, http: [{url: \"http://a.example\", times: x}]}",
			`times is a non-negative integer, not str "x"`,
		},
		{"expect not a mapping", "- {name: x, call: Spin, expect: [1]}", "expect is a mapping"},
		{
			"two expectations", "- {name: x, call: Spin, expect: {error: a, result: b}}",
			"expect gives one of result, error and issues",
		},
		{"an expected error not a string", "- {name: x, call: Spin, expect: {error: [a]}}", "error is a string"},
		{"issues not a list", "- {name: x, call: Validate, expect: {issues: {}}}", "issues is a list"},
		{
			"an issue with an unknown key", "- {name: x, call: Validate, expect: {issues: [{sev: error}]}}",
			`an issue has no key "sev" (keys: code, field, message, severity)`,
		},
		{
			"an issue field not a string", "- {name: x, call: Validate, expect: {issues: [{code: [a]}]}}",
			"code is a string, not a list",
		},
		{
			"two malformed issue fields, the first key reported",
			"- {name: x, call: Validate, expect: {issues: [{field: [b], code: [a]}]}}",
			"tests/t.yaml:2:66: code is a string, not a list",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plugin := &runner{}
			results := run(t, cases(tc.yaml+"\n- {name: fine, call: Spin, input: {mode: x}}\n"), plugin)

			require.Len(t, results, 2)
			assert.Contains(t, results[0].Failure, tc.want)
			assert.True(t, results[1].Passed(), "the next case ran: %s", results[1].Failure)
			assert.Len(t, plugin.calls, 1, "only the well-formed case ran")
		})
	}

	t.Run("a name used twice", func(t *testing.T) {
		t.Parallel()

		results := run(t, cases(`
- {name: same, call: Spin, input: {mode: a}}
- {name: same, call: Spin, input: {mode: b}}
`), &runner{})

		assert.True(t, results[0].Passed())
		assert.Equal(t, `tests/t.yaml:4:3: another case of the file is named "same"`, results[1].Failure)
	})
}

// TestValuesDecodeIntoTheContractsTypes runs a case whose input holds a value of every data type, partly from
// fixtures, and holds the arguments the call gets to the values written.
func TestValuesDecodeIntoTheContractsTypes(t *testing.T) {
	t.Parallel()

	plugin := &runner{}
	results := run(t, cases(`
- name: every type
  call: Quote
  input:
    order:
      placed: 2026-11-08T10:00:00.5Z
      gift: {$ref: fixtures/gift.yaml}
      tags: {a: x, "1": y}
      customer: alice
      lines: {$ref: fixtures/lines.yaml}
      data: aGk=
      big: -9007199254740992
      count: 65535
      ratio: 1.5
      small: -128
      flag: true
      Plain: plain
`, "fixtures/gift.yaml", "message: {$ref: fixtures/text.yaml}", "fixtures/text.yaml", "happy",
		"fixtures/lines.yaml", "- {sku: a, qty: 2, cents: 3}\n- {}\n"), plugin)

	require.True(t, results[0].Passed(), results[0].Failure)
	require.Len(t, plugin.calls, 1)
	assert.Equal(t, quoteMethod, plugin.calls[0].Method)

	got, ok := reflect.TypeAssert[order](plugin.calls[0].Args[0])
	require.True(t, ok)
	assert.Equal(t, order{
		Placed: time.Date(2026, 11, 8, 10, 0, 0, 500000000, time.UTC), Gift: &gift{Message: "happy"},
		Tags: map[string]string{"a": "x", "1": "y"}, Customer: "alice", Lines: []line{{"a", 2, 3}, {}},
		Data: []byte("hi"), Big: -1 << 53, Count: 65535, Ratio: 1.5, Small: -128, Flag: true, Plain: "plain",
	}, got)

	nulls := &runner{}
	results = run(t, cases(`
- name: nulls and empties
  call: Quote
  input:
    order: {gift: null, tags: null, lines: [], data: null, placed: "2026-11-08T10:00:00Z", ratio: 2}
`), nulls)

	require.True(t, results[0].Passed(), results[0].Failure)

	got, _ = reflect.TypeAssert[order](nulls.calls[0].Args[0])
	assert.Nil(t, got.Gift)
	assert.Nil(t, got.Tags)
	assert.Nil(t, got.Data)
	assert.NotNil(t, got.Lines, "[] is an empty list, not null")
	assert.Empty(t, got.Lines)
	assert.InDelta(t, 2, got.Ratio, 0, "an integer is a number")
}

// TestValuesOfAnotherTypeFailTheCase holds that a value the contract's type cannot hold fails its case, naming
// where it is and what was expected.
func TestValuesOfAnotherTypeFailTheCase(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, input, want string }{
		{"an unknown field", "{customer: a, custmer: b}", `input.order: unknown field "custmer" (fields: placed, gift`},
		{"a skipped field", `{"-": x}`, `input.order: unknown field "-"`},
		{"an unexported field", "{hidden: 1}", `unknown field "hidden"`},
		{"a nested unknown field", "{lines: [{sku: a, qtty: 1}]}", `input.order.lines[0]: unknown field "qtty"`},
		{"a string for an integer", "{big: ten}", `input.order.big: str "ten" is not an integer (int64)`},
		{"a float for an integer", "{big: 1.5}", `input.order.big: float "1.5" is not an integer (int64)`},
		{"an int8 overflow", "{small: 128}", `input.order.small: int "128" is not an integer (int8)`},
		{"a negative uint", "{count: -1}", `input.order.count: int "-1" is not an integer (uint16)`},
		{"a uint16 overflow", "{count: 65536}", `int "65536" is not an integer (uint16)`},
		{"a float32 overflow", "{ratio: 1e300}", `input.order.ratio: float "1e300" is not a number (float32)`},
		{"a string for a number", "{ratio: x}", `str "x" is not a number (float32)`},
		{"a number for a string", "{customer: 7}", `input.order.customer: int "7" is not a string`},
		{"a date for a string", "{customer: 2026-01-01}", `timestamp "2026-01-01" is not a string`},
		{"a string for a bool", "{flag: yes}", `input.order.flag: str "yes" is not a bool`},
		{"a list for a bool", "{flag: [true]}", `input.order.flag: a list is not a bool`},
		{"null for a string", "{customer: null}", `input.order.customer: null "null" is not a string`},
		{"null for a struct", "null", `input.order: null "null" is not a mapping`},
		{"a list for a struct", "[a]", `input.order: a list is not a mapping`},
		{"a mapping for a list", "{lines: {sku: a}}", `input.order.lines: a mapping is not a list`},
		{"a list for a map", "{tags: [a]}", `input.order.tags: a list is not a mapping`},
		{"a list in a map", `{tags: {a: [b]}}`, `input.order.tags["a"]: a list is not a string`},
		{"a bad timestamp", "{placed: yesterday}", `input.order.placed: str "yesterday" is not a timestamp (RFC 3339)`},
		{"a number for a timestamp", "{placed: 1}", `int "1" is not a timestamp (RFC 3339)`},
		{"bad base64", "{data: '%%'}", `input.order.data: str "%%" is not bytes (a base64 string)`},
		{"a number for bytes", "{data: 1}", `int "1" is not bytes (a base64 string)`},
		{"a list for a pointer's struct", "{gift: [a]}", `input.order.gift: a list is not a mapping`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plugin := &runner{}
			results := run(t, cases("- {name: x, call: Quote, input: {order: "+tc.input+"}}\n"), plugin)

			assert.Contains(t, results[0].Failure, tc.want)
			assert.True(t, strings.HasPrefix(results[0].Failure, "tests/t.yaml:2:"), results[0].Failure)
			assert.Empty(t, plugin.calls, "the case does not run")
		})
	}
}

func TestFixturesFailTheirCaseWhenTheyCannotBeRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, ref, want string
		files           []string
	}{
		{"missing", "fixtures/none.yaml", "tests/t.yaml:2:48: input.order: $ref: open fixtures/none.yaml", nil},
		{"not a string", "[a]", "tests/t.yaml:2:48: input.order: $ref is a path", nil},
		{
			"outside the plugin",
			"../secret.yaml",
			`$ref "../secret.yaml" is not a path inside the plugin's directory`,
			nil,
		},
		{"absolute", "/etc/passwd", `$ref "/etc/passwd" is not a path inside the plugin's directory`, nil},
		{"the plugin's directory", ".", `$ref "." is not a path inside the plugin's directory`, nil},
		{"malformed", "f.yaml", "$ref: f.yaml: parsing: yaml:", []string{"f.yaml", "a: ["}},
		{"of another type", "f.yaml", "f.yaml:1:1: input.order: a list is not a mapping", []string{"f.yaml", "[1]"}},
		{
			"a cycle", "a.yaml", "b.yaml:1:8: input.order.gift: a.yaml refers to itself (through a.yaml -> b.yaml)",
			[]string{"a.yaml", "gift: {$ref: b.yaml}", "b.yaml", "{$ref: a.yaml}"},
		},
		{"too large", "f.yaml", "$ref: f.yaml: larger than", []string{"f.yaml", strings.Repeat("#", 1<<20+1)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plugin := &runner{}
			results := run(
				t,
				cases("- {name: x, call: Quote, input: {order: {$ref: "+tc.ref+"}}}\n", tc.files...),
				plugin,
			)

			assert.Contains(t, results[0].Failure, tc.want)
			assert.Empty(t, plugin.calls)
		})
	}

	t.Run("used twice", func(t *testing.T) {
		t.Parallel()

		plugin := &runner{}
		results := run(t, cases(`
- name: twice
  call: Quote
  input: {order: {lines: [{$ref: l.yaml}, {$ref: l.yaml}]}}
`, "l.yaml", "{sku: a}"), plugin)

		require.True(t, results[0].Passed(), "a fixture used twice is no cycle: %s", results[0].Failure)

		got, _ := reflect.TypeAssert[order](plugin.calls[0].Args[0])
		assert.Equal(t, []line{{SKU: "a"}, {SKU: "a"}}, got.Lines)
	})
}

func TestCallsAndInputsMustMatchTheContract(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, yaml, want string }{
		{
			"an unknown method", "- {name: x, call: Price}",
			`tests/t.yaml:2:3: call "Price" is not a method of the contract (Quote, Spin, Split, Validate)`,
		},
		{
			"a missing parameter", "- {name: x, call: Split, input: {cents: 1}}",
			`tests/t.yaml:2:3: input gives no "parts" (parameters: cents, parts)`,
		},
		{"no input", "- {name: x, call: Spin}", `input gives no "mode" (parameters: mode)`},
		{
			"an unknown parameter", "- {name: x, call: Spin, input: {mode: a, speed: 1}}",
			`tests/t.yaml:2:42: input: the method has no parameter "speed" (parameters: mode)`,
		},
		{
			"Validate's parameter", "- {name: x, call: Validate, input: {limits: {}}}",
			`the method has no parameter "limits" (parameters: in)`,
		},
		{
			"a result of a method returning none", "- {name: x, call: Spin, input: {mode: a}, expect: {result: 1}}",
			"tests/t.yaml:2:60: expect.result: the method returns no result besides its error",
		},
		{
			"one result for several", "- {name: x, call: Split, input: {cents: 1, parts: 1}, expect: {result: 1}}",
			"expect.result: the method returns 2 results: give a list of 2",
		},
		{
			"too few results", "- {name: x, call: Split, input: {cents: 1, parts: 1}, expect: {result: [1]}}",
			"expect.result: the method returns 2 results: give a list of 2",
		},
		{
			"a result of another type",
			"- {name: x, call: Split, input: {cents: 1, parts: 1}, expect: {result: [1, 2]}}",
			"expect.result[0]: int \"1\" is not a list",
		},
		{
			"an expected result of another type",
			"- {name: x, call: Quote, input: {order: {}}, expect: {result: {totl: 1}}}",
			`expect.result: unknown field "totl" (fields: reasons, total, discount)`,
		},
		{
			"issues of a method", "- {name: x, call: Spin, input: {mode: a}, expect: {issues: []}}",
			"expect.issues is for a call of Validate; Spin returns no issues",
		},
		{
			"a result of Validate", "- {name: x, call: Validate, input: {in: {}}, expect: {result: []}}",
			"a call of Validate expects issues, not a result",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plugin := &runner{}
			results := run(t, cases(tc.yaml+"\n"), plugin)

			assert.Contains(t, results[0].Failure, tc.want)
			assert.Empty(t, plugin.calls)
		})
	}

	t.Run("no validation input", func(t *testing.T) {
		t.Parallel()

		contract := newContract()
		contract.Validation = nil
		files := cases("- {name: x, call: Validate, input: {in: {}}}\n")

		results := testcase.Run(t.Context(), contract, files, &runner{}, testcase.Load(files)[0])
		assert.Equal(t, `tests/t.yaml:2:3: call "Validate" is not a method of the contract (Quote, Spin, Split)`,
			results[0].Failure)
	})
}

// TestExpectationsCompareWhatTheCallReturned runs cases against plugins returning results, errors and issues,
// and holds each case's failure and the lines showing the expected and actual values.
func TestExpectationsCompareWhatTheCallReturned(t *testing.T) {
	t.Parallel()

	failing := func(outcome offline.Outcome) func(context.Context, *offline.Call) offline.Outcome {
		return func(context.Context, *offline.Call) offline.Outcome { return outcome }
	}
	issues := func(got ...adminjson.Issue) func(context.Context, *offline.Call) offline.Outcome {
		return failing(offline.Outcome{Issues: got})
	}
	warning := adminjson.Issue{Severity: "warning", Code: "slow", Message: "too slow", Field: "speed"}

	tests := []struct {
		play    func(context.Context, *offline.Call) offline.Outcome
		name    string
		yaml    string
		failure string
		details []string
	}{
		{
			name: "equal results",
			yaml: "- {name: x, call: Split, input: {cents: 3, parts: 2}, expect: {result: [[1, 1], 1]}}",
			play: returning([]int{1, 1}, 1),
		},
		{
			name: "no expectation", yaml: "- {name: x, call: Split, input: {cents: 3, parts: 2}}",
			play: returning([]int{9}, 9),
		},
		{
			name:    "other results",
			yaml:    "- {name: x, call: Split, input: {cents: 3, parts: 2}, expect: {result: [[1, 1], 1]}}",
			play:    returning([]int{1, 2, 3}, 0),
			failure: "Split returned other results than expect.result",
			details: []string{"result[0]: expected [1, 1], got [1, 2, 3]", "result[1]: expected 1, got 0"},
		},
		{
			name:    "null and empty differ",
			yaml:    "- {name: x, call: Split, input: {cents: 3, parts: 2}, expect: {result: [[], 1]}}",
			play:    returning([]int(nil), 1),
			failure: "Split returned other results than expect.result",
			details: []string{"result[0]: expected [], got null"},
		},
		{
			name: "null is nil",
			yaml: "- {name: x, call: Split, input: {cents: 3, parts: 2}, expect: {result: [null, 1]}}",
			play: returning([]int(nil), 1),
		},
		{
			name:    "a struct's fields",
			yaml:    "- {name: x, call: Quote, input: {order: {}}, expect: {result: {total: 9, discount: 1, reasons: [a, b]}}}",
			play:    returning(quote{Total: 8, Discount: 1, Reasons: []string{"a", "c"}}),
			failure: "Quote returned other results than expect.result",
			details: []string{`result.reasons[1]: expected "b", got "c"`, "result.total: expected 9, got 8"},
		},
		{
			name: "the expected error", yaml: "- {name: x, call: Spin, input: {mode: a}, expect: {error: not found}}",
			play: failing(offline.Outcome{Err: errNotFound, Returned: "user not found"}),
		},
		{
			name: "another error", yaml: "- {name: x, call: Spin, input: {mode: a}, expect: {error: expired}}",
			play:    failing(offline.Outcome{Err: errNotFound, Returned: "user not found"}),
			failure: "Spin returned another error than expect.error",
			details: []string{`expected an error containing "expired"`, `got "user not found"`},
		},
		{
			name:    "a failure that is no error of the plugin's",
			yaml:    "- {name: x, call: Spin, input: {mode: a}, expect: {error: not found}}",
			play:    failing(offline.Outcome{Err: errPanicked}),
			failure: "Spin failed: it panicked or trapped: not found",
		},
		{
			name:    "an unexpected error",
			yaml:    "- {name: x, call: Split, input: {cents: 3, parts: 2}, expect: {result: [[1], 1]}}",
			play:    failing(offline.Outcome{Err: errNoParts, Returned: "no parts"}),
			failure: "Split failed: no parts",
			details: []string{"expected results [[1], 1]", `got error "no parts"`},
		},
		{
			name:    "an unexpected error of Validate",
			yaml:    "- {name: x, call: Validate, input: {in: {}}, expect: {issues: [{code: slow}]}}",
			play:    failing(offline.Outcome{Err: errNotFound, Returned: "user not found"}),
			failure: "Validate failed: plg: Spin: user not found",
			details: []string{`expected issues [{code: "slow"}]`, `got error "user not found"`},
		},
		{
			name:    "a panic where a result is expected",
			yaml:    "- {name: x, call: Quote, input: {order: {}}, expect: {result: {total: 1}}}",
			play:    failing(offline.Outcome{Err: errPanicked}),
			failure: "Quote failed: it panicked or trapped: not found",
			details: []string{"expected result {reasons: null, total: 1, discount: 0}"},
		},
		{
			name: "no error", yaml: "- {name: x, call: Split, input: {cents: 3, parts: 2}, expect: {error: no parts}}",
			play:    returning([]int{1, 1}, 1),
			failure: "Split returned no error",
			details: []string{`expected an error containing "no parts"`, "got results [[1, 1], 1]"},
		},
		{
			name:    "no error of a method of one result",
			yaml:    "- {name: x, call: Quote, input: {order: {}}, expect: {error: empty}}",
			play:    returning(quote{}),
			failure: "Quote returned no error",
			details: []string{
				`expected an error containing "empty"`,
				"got result {reasons: null, total: 0, discount: 0}",
			},
		},
		{
			name:    "no error of a method of no result",
			yaml:    "- {name: x, call: Spin, input: {mode: a}, expect: {error: x}}",
			play:    returning(),
			failure: "Spin returned no error",
			details: []string{`expected an error containing "x"`, "got no result"},
		},
		{
			name: "no error of Validate", yaml: "- {name: x, call: Validate, input: {in: {}}, expect: {error: x}}",
			play:    issues(warning),
			failure: "Validate returned no error",
			details: []string{
				`expected an error containing "x"`,
				`got issues [{severity: "warning", code: "slow", message: "too slow", field: "speed"}]`,
			},
		},
		{
			name: "the issues expected",
			play: issues(warning, adminjson.Issue{Severity: "error", Code: "max"}),
			yaml: "- {name: x, call: Validate, input: {in: {maxPercent: 5}}, expect: {issues: [{code: slow, field: speed}, {severity: error}]}}",
		},
		{
			name: "no issues", play: issues(),
			yaml: "- {name: x, call: Validate, input: {in: {}}, expect: {issues: []}}",
		},
		{
			name:    "other issues",
			play:    issues(warning),
			yaml:    "- {name: x, call: Validate, input: {in: {}}, expect: {issues: [{severity: error, code: slow, message: slow}]}}",
			failure: "Validate returned other issues than expect.issues",
			details: []string{
				`issues[0].severity: expected "error", got "warning"`,
				`issues[0].message: expected "slow", got "too slow"`,
			},
		},
		{
			name: "more issues", play: issues(warning, adminjson.Issue{Severity: "info", Code: "note"}),
			yaml:    "- {name: x, call: Validate, input: {in: {}}, expect: {issues: [{code: slow}]}}",
			failure: "Validate returned other issues than expect.issues",
			details: []string{`issues: expected 1, got 2: [{severity: "warning", code: "slow", message: "too slow", ` +
				`field: "speed"}, {severity: "info", code: "note"}]`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			results := run(t, cases(tc.yaml+"\n"), &runner{play: tc.play})

			assert.Equal(t, tc.failure, results[0].Failure)
			assert.Equal(t, tc.details, results[0].Details)
			assert.Equal(t, tc.failure == "", results[0].Passed())
		})
	}
}

// TestDiffsShowEveryKindOfValue holds the lines showing the differences between expected and returned values of
// every data type.
func TestDiffsShowEveryKindOfValue(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	got := order{
		Placed: when.Add(time.Second), Gift: &gift{Message: "b"}, Tags: map[string]string{"k": "1", "z": "2"},
		Customer: "c", Lines: []line{{SKU: "x"}}, Data: []byte{1}, Big: 2, Count: 3, Ratio: 0.5, Flag: true,
	}

	// A contract whose Quote returns an order, a value of every data type.
	contract := newContract()
	contract.Methods[quoteMethod].Type = reflect.TypeFor[func(context.Context, order) (order, error)]()

	files := cases(`
- name: everything differs
  call: Quote
  input: {order: {}}
  expect:
    result:
      placed: 2026-11-08T10:00:00Z
      gift: {message: a}
      tags: {a: "0", k: "0"}
      customer: d
      lines: [{sku: y}]
      data: AA==
      big: 1
      count: 4
      ratio: 1.5
      flag: false
- name: pointers
  call: Quote
  input: {order: {}}
  expect:
    result: {gift: null, tags: {}, placed: 2026-11-08T10:00:01Z}
- name: equal
  call: Quote
  input: {order: {}}
  expect:
    result: {gift: {message: b}, tags: {k: "1", z: "2"}, placed: 2026-11-08T10:00:01Z, customer: c, lines: [{sku: x}],
             data: AQ==, big: 2, count: 3, ratio: 0.5, flag: true}
`)

	results := testcase.Run(t.Context(), contract, files, &runner{play: returning(got)}, testcase.Load(files)[0])

	assert.Equal(t, []string{
		"result.placed: expected 2026-11-08T10:00:00Z, got 2026-11-08T10:00:01Z",
		`result.gift.message: expected "a", got "b"`,
		`result.tags["a"]: expected "0", got nothing`,
		`result.tags["k"]: expected "0", got "1"`,
		`result.tags["z"]: expected nothing, got "2"`,
		`result.customer: expected "d", got "c"`,
		`result.lines[0].sku: expected "y", got "x"`,
		`result.data: expected "AA==", got "AQ=="`,
		"result.big: expected 1, got 2",
		"result.ratio: expected 1.5, got 0.5",
		"result.count: expected 4, got 3",
		"result.flag: expected false, got true",
	}, results[0].Details)

	assert.Equal(t, []string{
		`result.gift: expected null, got {message: "b"}`,
		`result.tags["k"]: expected nothing, got "1"`,
	}, results[1].Details[:2])
	assert.True(t, results[2].Passed(), results[2].Failure)
}

// TestNaNAndTheInfinitiesAreValuesOfTheirOwn: a case expects NaN as .nan and the infinities as .inf and -.inf,
// and a difference shows them so; NaN is the same value as NaN.
func TestNaNAndTheInfinitiesAreValuesOfTheirOwn(t *testing.T) {
	t.Parallel()

	contract := newContract()
	contract.Methods[quoteMethod].Type = reflect.TypeFor[func(context.Context, order) (float64, error)]()

	tests := []struct {
		expect  string
		details []string
		got     float64
	}{
		{expect: ".nan", got: math.NaN()},
		{expect: ".inf", got: math.Inf(1)},
		{expect: "-.inf", got: math.Inf(-1)},
		{expect: ".nan", got: 1.5, details: []string{"result: expected .nan, got 1.5"}},
		{expect: "1.5", got: math.NaN(), details: []string{"result: expected 1.5, got .nan"}},
		{expect: ".inf", got: math.Inf(-1), details: []string{"result: expected .inf, got -.inf"}},
		{expect: ".nan", got: math.Inf(1), details: []string{"result: expected .nan, got .inf"}},
	}

	for _, tc := range tests {
		files := cases("- {name: x, call: Quote, input: {order: {}}, expect: {result: " + tc.expect + "}}\n")
		results := testcase.Run(t.Context(), contract, files, &runner{play: returning(tc.got)}, testcase.Load(files)[0])

		assert.Equal(t, tc.details, results[0].Details, "expect %s, got %v", tc.expect, tc.got)
		assert.Equal(t, tc.details == nil, results[0].Passed(), "expect %s, got %v", tc.expect, tc.got)
	}
}

// TestHostStubsAnswerThePluginsHostCalls runs cases whose plugin calls the Host's Tier, and holds what the
// stubs answered, the calls they count and the problems that fail a case.
func TestHostStubsAnswerThePluginsHostCalls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, yaml string
		failure    string
		customers  []string
		answers    []string
		details    []string
	}{
		{
			name:      "matching arguments",
			yaml:      "host: [{method: Tier, args: {customer: alice}, result: gold}, {method: Tier, result: silver}]",
			customers: []string{"alice", "bob", "alice"}, answers: []string{"gold", "silver", "gold"},
		},
		{
			name: "the first match wins", yaml: "host: [{method: Tier, result: a}, {method: Tier, result: b}]",
			customers: []string{"x"}, answers: []string{"a"},
		},
		{
			name: "an error", yaml: "host: [{method: Tier, error: down}]",
			customers: []string{"x"}, answers: []string{"error: down"},
		},
		{
			name: "no result", yaml: "host: [{method: Tier}]",
			customers: []string{"x"}, answers: []string{""},
		},
		{
			name: "the expected count", yaml: "host: [{method: Tier, result: a, times: 2}, {method: Pair, times: 0}]",
			customers: []string{"x", "y"}, answers: []string{"a", "a"},
		},
		{
			name: "another count", yaml: "host: [{method: Tier, args: {customer: x}, result: a, times: 2}]",
			customers: []string{"x"}, answers: []string{"a"},
			failure: "the stubs were not used as the case declares",
			details: []string{`host stub Tier(customer: "x") answered 1 call, expected 2`},
		},
		{
			name: "a call no stub matches", yaml: "host: [{method: Tier, args: {customer: x}, result: a, times: 0}]",
			customers: []string{"y", "x"}, answers: []string{
				`error: plg validate: no host stub of the test case matches Tier(customer: "y")`, "a",
			},
			failure: "the stubs were not used as the case declares",
			details: []string{
				`Host call Tier(customer: "y") matched no host stub`,
				`host stub Tier(customer: "x") answered 1 call, expected 0`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var answers []string

			contract := newContract()
			files := cases("- {name: x, call: Spin, input: {mode: a}, " + tc.yaml + "}\n")
			plugin := &runner{play: func(ctx context.Context, _ *offline.Call) offline.Outcome {
				for _, customer := range tc.customers {
					got, err := tier(ctx, contract, customer)
					if err != nil {
						got = "error: " + err.Error()
					}

					answers = append(answers, got)
				}

				return offline.Outcome{}
			}}

			results := testcase.Run(t.Context(), contract, files, plugin, testcase.Load(files)[0])

			assert.Equal(t, tc.answers, answers)
			assert.Equal(t, tc.failure, results[0].Failure)
			assert.Equal(t, tc.details, results[0].Details)
		})
	}

	t.Run("several results", func(t *testing.T) {
		t.Parallel()

		var out []reflect.Value

		contract := newContract()
		files := cases(
			"- {name: x, call: Spin, input: {mode: a}, host: [{method: Pair, args: {b: y}, result: [p, 2]}]}\n",
		)

		results := testcase.Run(t.Context(), contract, files, &runner{
			play: func(ctx context.Context, _ *offline.Call) offline.Outcome {
				out = contract.Host.Funcs[0].Call([]reflect.Value{
					reflect.ValueOf(ctx), reflect.ValueOf("x"), reflect.ValueOf("y"),
				})

				return offline.Outcome{}
			},
		}, testcase.Load(files)[0])

		require.True(t, results[0].Passed(), results[0].Failure)
		require.Len(t, out, 3)
		assert.Equal(t, "p", out[0].String())
		assert.Equal(t, int64(2), out[1].Int())
		assert.True(t, out[2].IsNil())
	})

	t.Run("outside a case", func(t *testing.T) {
		t.Parallel()

		_, err := tier(t.Context(), newContract(), "x")
		assert.EqualError(t, err, "plg validate: no test case is running")
	})
}

// TestSecretFakesAnswerThePluginsReads runs cases whose plugin reads secrets, and holds what each read got, and
// the problems and errors that fail a case: a read of a secret the case does not fake, a fake of a secret
// plugin.yaml does not declare, a fake that is neither a string nor null.
func TestSecretFakesAnswerThePluginsReads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, yaml string
		failure    string
		reads      []string
		answers    []string
		details    []string
	}{
		{
			name:  "values and a failing read",
			yaml:  "secrets: {api_key: s3cret, backup: null}",
			reads: []string{"api_key", "backup", "api_key"},
			answers: []string{
				"s3cret",
				"error: plg validate: the test case fakes this secret's read failing",
				"s3cret",
			},
		},
		{
			name: "an empty value", yaml: "secrets: {api_key: ''}", reads: []string{"api_key"}, answers: []string{""},
		},
		{
			name: "a read without a fake", yaml: "secrets: {api_key: s3cret}", reads: []string{"backup"},
			answers: []string{"error: plg validate: the test case fakes no such secret: backup"},
			failure: "the stubs were not used as the case declares",
			details: []string{"secret backup was read, but the case fakes no value for it under secrets:"},
		},
		{
			name: "no secrets", reads: []string{"api_key"},
			answers: []string{"error: plg validate: the test case fakes no such secret: api_key"},
			failure: "the stubs were not used as the case declares",
			details: []string{"secret api_key was read, but the case fakes no value for it under secrets:"},
		},
		{
			name: "an undeclared fake", yaml: "secrets: {api_key: a, zeta: z, other: o}",
			failure: `tests/t.yaml:2:52: secrets fakes "other", which plugin.yaml does not declare ` +
				`(secrets: [api_key, backup])`,
		},
		{
			name: "a fake not a string", yaml: "secrets: {api_key: 42}",
			failure: `tests/t.yaml:2:62: secrets: api_key is a string, or null for a read that fails, not int "42"`,
		},
		{
			name: "secrets not a mapping", yaml: "secrets: [api_key]",
			failure: `tests/t.yaml:2:52: secrets is a mapping of secret names to values, not a list`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var answers []string

			yaml := "- {name: x, call: Spin, input: {mode: a}"
			if tc.yaml != "" {
				yaml += ", " + tc.yaml
			}

			files := cases(yaml + "}\n")
			plugin := &runner{declared: []string{"api_key", "backup"}, play: func(ctx context.Context,
				_ *offline.Call,
			) offline.Outcome {
				for _, name := range tc.reads {
					got, err := testcase.Secret(ctx, name)
					if err != nil {
						got = "error: " + err.Error()
					}

					answers = append(answers, got)
				}

				return offline.Outcome{}
			}}

			results := testcase.Run(t.Context(), newContract(), files, plugin, testcase.Load(files)[0])

			assert.Equal(t, tc.answers, answers)
			assert.Equal(t, tc.failure, results[0].Failure)
			assert.Equal(t, tc.details, results[0].Details)
		})
	}

	t.Run("outside a case", func(t *testing.T) {
		t.Parallel()

		_, err := testcase.Secret(t.Context(), "api_key")
		assert.EqualError(t, err, "plg validate: no test case is running")
	})
}

func TestHostStubsMustMatchTheHost(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, yaml, want string }{
		{
			"an unknown method",
			"[{method: Rank}]",
			"tests/t.yaml:2:50: host[0]: Rank is not a method of the Host (Pair, Tier)",
		},
		{
			"an unknown parameter",
			"[{method: Tier, args: {user: a}}]",
			`host[0].args: the method has no parameter "user" (parameters: customer)`,
		},
		{
			"an argument of another type",
			"[{method: Tier, args: {customer: 1}}]",
			`host[0].args.customer: int "1" is not a string`,
		},
		{"a result of another type", "[{method: Tier, result: [a]}]", "host[0].result: a list is not a string"},
		{
			"one result for several",
			"[{method: Pair, result: a}]",
			"host[0].result: the method returns 2 results: give a list of 2",
		},
		{
			"a second stub's error",
			"[{method: Tier}, {method: Pair, result: [a, b]}]",
			`host[1].result[1]: str "b" is not an integer (int)`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plugin := &runner{}
			results := run(t, cases("- {name: x, call: Spin, input: {mode: a}, host: "+tc.yaml+"}\n"), plugin)

			assert.Contains(t, results[0].Failure, tc.want)
			assert.Empty(t, plugin.calls)
		})
	}

	t.Run("no Host", func(t *testing.T) {
		t.Parallel()

		contract := newContract()
		contract.Host = nil
		files := cases("- {name: x, call: Spin, input: {mode: a}, host: [{method: Tier}]}\n")

		results := testcase.Run(t.Context(), contract, files, &runner{}, testcase.Load(files)[0])
		assert.Equal(t, "tests/t.yaml:2:50: the case has host stubs but the plugin's Create takes no Host (for Lua "+
			"and TypeScript plugins, name it: plg validate -host <Interface>)", results[0].Failure)
	})
}

// TestHTTPStubsAnswerThePluginsRequests runs cases whose plugin makes requests, and holds the responses, the
// counts and the problems that fail a case.
func TestHTTPStubsAnswerThePluginsRequests(t *testing.T) {
	t.Parallel()

	const policy = "httpPolicy: {allowedHosts: [a.example]}\ncases:\n"

	tests := []struct {
		name, yaml string
		failure    string
		urls       []string
		want       []response
		details    []string
	}{
		{
			name: "matching URLs",
			yaml: `[{url: "https://a.example/x?q=1", status: 201, headers: {X-A: b}, body: hi, times: 1},` +
				`{method: post, url: "https://a.example/y"}, {url: "https://a.example/y", body: "y"}]`,
			urls: []string{"https://a.example/x?q=1", "https://a.example/y"},
			want: []response{
				{status: 201, body: "hi", header: http.Header{"X-A": {"b"}}},
				{status: 200, body: "y", header: http.Header{}},
			},
		},
		{
			name: "a request no stub matches",
			yaml: `[{url: "https://a.example/x", times: 1}]`,
			urls: []string{"https://a.example/z"},
			want: []response{
				{body: "error: plg validate: no http stub of the test case matches GET https://a.example/z"},
			},
			failure: "the stubs were not used as the case declares",
			details: []string{
				"HTTP request GET https://a.example/z matched no http stub",
				"http stub GET https://a.example/x answered 0 calls, expected 1",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got []response

			files := fstest.MapFS{"tests/t.yaml": {
				Data: []byte(policy + "- {name: x, call: Spin, input: {mode: a}, http: " + tc.yaml + "}\n"),
			}}
			plugin := &runner{play: func(ctx context.Context, call *offline.Call) offline.Outcome {
				for _, url := range tc.urls {
					got = append(got, get(ctx, call.HTTP, url))
				}

				return offline.Outcome{}
			}}

			loaded := testcase.Load(files)
			require.NoError(t, loaded[0].Err)

			results := testcase.Run(t.Context(), newContract(), files, plugin, loaded[0])

			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.failure, results[0].Failure)
			assert.Equal(t, tc.details, results[0].Details)
		})
	}

	t.Run("without a policy", func(t *testing.T) {
		t.Parallel()

		plugin := &runner{}
		results := run(t, cases(`- {name: x, call: Spin, input: {mode: a}, http: [{url: "https://a.example"}]}`+"\n"),
			plugin)

		assert.Equal(t, "tests/t.yaml:2:3: the case has http stubs but the file no httpPolicy: every request is "+
			"refused", results[0].Failure)
		assert.Empty(t, plugin.calls)
	})

	t.Run("outside a case", func(t *testing.T) {
		t.Parallel()

		loaded := testcase.Load(fstest.MapFS{"tests/t.yaml": {Data: []byte(policy)}})

		got := get(t.Context(), loaded[0].HTTP, "https://a.example")
		assert.Equal(t, "error: plg validate: no test case is running", got.body)
	})
}

func TestFormatWritesValuesAsCasesDo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value any
		want  string
	}{
		{map[string]int{"b": 2, "a": 1}, `{"a": 1, "b": 2}`},
		{&gift{Message: "x"}, `{message: "x"}`},
		{(*gift)(nil), "null"},
		{[]byte("hi"), `"aGk="`},
		{time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), "2026-01-02T03:04:05.000000006Z"},
		{[]float64{1.5, 2, math.NaN(), math.Inf(1)}, "[1.5, 2, .nan, .inf]"},
		{float32(math.Inf(-1)), "-.inf"},
		{"quote\"d", `"quote\"d"`},
		{line{SKU: "a", Qty: 1}, `{sku: "a", qty: 1, cents: 0}`},
		{[]level{2}, "[2]"},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.want, testcase.Format(reflect.ValueOf(tc.value)))
	}
}

// TestExamplesDecodeTheValidationInput reads the example validation inputs, examples/validation/*.yaml, in name
// order: each decodes into the contract's validation input, a $ref taking it from a fixture; one that does not parse
// or decode fails alone, naming where; without a validation input in the contract every example fails with
// ErrNoValidation. ValidateWith is the call of Validate with an example.
func TestExamplesDecodeTheValidationInput(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"examples/validation/b.yaml":    {Data: []byte("maxPercent: 5\n")},
		"examples/validation/a.yaml":    {Data: []byte("{$ref: fixtures/limits.yaml}\n")},
		"examples/validation/c.yaml":    {Data: []byte("maxPercent: high\n")},
		"examples/validation/d.yaml":    {Data: []byte("maxPercent: [\n")},
		"examples/validation/notes.txt": {Data: []byte("not an example")},
		"examples/other.yaml":           {Data: []byte("maxPercent: 1\n")},
		"fixtures/limits.yaml":          {Data: []byte("maxPercent: 20\n")},
	}

	contract := newContract()
	got := testcase.Examples(contract, files)
	require.Len(t, got, 4)

	for i, name := range []string{"a", "b", "c", "d"} {
		assert.Equal(t, "examples/validation/"+name+".yaml", got[i].Name)
	}

	require.NoError(t, got[0].Err)
	assert.Equal(t, limits{MaxPercent: 20}, got[0].Input.Interface())
	require.NoError(t, got[1].Err)
	assert.Equal(t, limits{MaxPercent: 5}, got[1].Input.Interface())
	require.EqualError(t, got[2].Err, `examples/validation/c.yaml:1:13: example.maxPercent: str "high" is not an `+
		`integer (int)`)
	require.ErrorContains(t, got[3].Err, "examples/validation/d.yaml: ")

	call := testcase.ValidateWith(contract, got[1].Input)
	assert.Equal(t, validateMethod, call.Method)
	assert.Equal(t, []reflect.Value{got[1].Input}, call.Args)
	assert.Nil(t, call.HTTP, "Validate's examples make no requests")

	contract.Validation = nil

	for _, example := range testcase.Examples(contract, files) {
		require.ErrorIs(t, example.Err, testcase.ErrNoValidation, example.Name)
		require.ErrorContains(t, example.Err, example.Name+": the contract declares no validation input "+
			"(//plg:validation)")
		assert.False(t, example.Input.IsValid())
	}

	assert.Empty(t, testcase.Examples(newContract(), fstest.MapFS{}), "no examples")
}

// TestResultsCarryWhatTheCallTook keeps the resources the runner measured for a case's call, passed or failed, and
// none for a case that did not run.
func TestResultsCarryWhatTheCallTook(t *testing.T) {
	t.Parallel()

	used := offline.Resources{Memory: 3 << 20, MemoryLimit: 16 << 20, Duration: time.Millisecond, Timeout: time.Second}
	plugin := &runner{play: func(context.Context, *offline.Call) offline.Outcome {
		return offline.Outcome{
			Results:   []reflect.Value{reflect.ValueOf([]int{5, 5}), reflect.ValueOf(0)},
			Resources: used,
		}
	}}

	got := run(t, cases(`
  - {name: passes, call: Split, input: {cents: 10, parts: 2}, expect: {result: [[5, 5], 0]}}
  - {name: fails, call: Split, input: {cents: 10, parts: 2}, expect: {result: [[10], 0]}}
  - {name: malformed, call: Split, input: {cents: ten, parts: 2}}
`), plugin)
	require.Len(t, got, 3)
	assert.True(t, got[0].Passed())
	assert.Equal(t, used, got[0].Resources)
	assert.False(t, got[1].Passed())
	assert.Equal(t, used, got[1].Resources)
	assert.False(t, got[2].Passed())
	assert.Zero(t, got[2].Resources, "never ran")
	assert.Len(t, plugin.calls, 2)
}
